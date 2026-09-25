package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"sever_provider/internal/filestore"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/settings"
	"sever_provider/internal/workerhub"
)

type TTSRequest struct {
	Text     string  `json:"text"`
	VoiceID  string  `json:"voice_id"`
	Engine   string  `json:"engine,omitempty"`
	Language string  `json:"language,omitempty"`
	Speed    float64 `json:"speed,omitempty"`
}

type LLMRequest struct {
	Prompt      string         `json:"prompt"`
	Model       string         `json:"model,omitempty"`
	Temperature *float32       `json:"temperature,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	ExtraParams map[string]any `json:"extra_params,omitempty"`
}

type Runner struct {
	Jobs              *jobstore.Store
	Dispatcher        *scheduler.Dispatcher
	Workers           *workerhub.Hub
	Files             *filestore.Store
	Poll              time.Duration
	Timeout           time.Duration
	WorkerID          string
	TikTokConcurrency int
	TikTokSlots       chan struct{}
	Settings          *settings.Store
	Once              sync.Once
}

func (r *Runner) Start(ctx context.Context) {
	if r.Poll <= 0 {
		r.Poll = 200 * time.Millisecond
	}
	if r.Timeout <= 0 {
		r.Timeout = 5 * time.Minute
	}
	if r.WorkerID == "" {
		r.WorkerID = "unified-runner"
	}
	if r.TikTokConcurrency <= 0 {
		r.TikTokConcurrency = 4
	}
	if r.TikTokConcurrency > 32 {
		r.TikTokConcurrency = 32
	}
	if r.TikTokSlots == nil {
		r.TikTokSlots = make(chan struct{}, r.TikTokConcurrency)
	}
	r.Once.Do(func() { go r.loop(ctx) })
}

func (r *Runner) loop(ctx context.Context) {
	ticker := time.NewTicker(r.Poll)
	defer ticker.Stop()
	_, _ = r.Jobs.ResetStale(ctx, r.Timeout)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := 0; i < 8; i++ {
				job, err := r.Jobs.ClaimNext(ctx, r.WorkerID)
				if errors.Is(err, jobstore.ErrNoJobs) {
					break
				}
				if err != nil {
					break
				}
				go r.process(ctx, job)
			}
		}
	}
}

func (r *Runner) process(parent context.Context, job jobstore.Job) {
	ctx, cancel := context.WithTimeout(parent, r.Timeout)
	defer cancel()
	var err error
	switch strings.ToLower(job.Type) {
	case "tts":
		err = r.processTTS(ctx, job)
	case "llm", "chat":
		err = r.processLLM(ctx, job)
	default:
		err = fmt.Errorf("unsupported job type %q", job.Type)
	}
	if err != nil {
		_ = r.Jobs.Fail(context.Background(), job.ID, err)
	}
}

func (r *Runner) processTTS(ctx context.Context, job jobstore.Job) error {
	if r.Dispatcher == nil {
		return errors.New("dispatcher is unavailable")
	}
	var req TTSRequest
	if err := json.Unmarshal(job.Payload, &req); err != nil {
		return fmt.Errorf("decode tts job: %w", err)
	}
	model := job.Model
	if model == "" {
		model = "piper:" + req.VoiceID
		if req.Engine == "tiktok" || strings.HasPrefix(req.VoiceID, "BV") {
			model = "tiktok:" + req.VoiceID
		}
	}
	isTikTok := strings.HasPrefix(model, "tiktok:") || req.Engine == "tiktok" || strings.HasPrefix(req.VoiceID, "BV")
	if isTikTok && r.TikTokSlots != nil {
		select {
		case r.TikTokSlots <- struct{}{}:
			defer func() { <-r.TikTokSlots }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var data []byte
	var mime string
	var err error
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("PIPER_TTS_MODE")))
	if r.Settings != nil {
		mode = strings.ToLower(strings.TrimSpace(r.Settings.Get(ctx, "tts.piper_mode", mode)))
	}
	if mode == "" {
		mode = "auto"
	}
	if mode == "remote" || (mode == "auto" && r.Dispatcher == nil) {
		data, mime, err = r.remotePiperSpeech(ctx, model, req)
	} else {
		data, mime, err = r.Dispatcher.Speech(ctx, provider.SpeechRequest{Model: model, Input: req.Text, Voice: req.VoiceID})
		if err != nil && mode == "auto" && r.Workers != nil {
			data, mime, err = r.remotePiperSpeech(ctx, model, req)
		}
	}
	if err != nil {
		return err
	}
	if r.Files == nil {
		return errors.New("media store is unavailable")
	}
	ext := ".wav"
	if mime == "audio/mpeg" {
		ext = ".mp3"
	}
	fileID, err := r.Files.Save(data, ext)
	if err != nil {
		return err
	}
	result, _ := json.Marshal(map[string]any{"audio_url": "/api/tts/jobs/" + job.ID + "/audio", "file_id": fileID})
	return r.Jobs.CompleteWithOutput(ctx, job.ID, string(result), fileID, mime)
}

func (r *Runner) remotePiperSpeech(ctx context.Context, model string, req TTSRequest) ([]byte, string, error) {
	if r.Workers == nil {
		return nil, "", errors.New("remote Piper worker hub is unavailable")
	}
	return r.Workers.Speech(ctx, provider.SpeechRequest{
		Model: model,
		Input: req.Text,
		Voice: req.VoiceID,
	})
}

func (r *Runner) processLLM(ctx context.Context, job jobstore.Job) error {
	var req LLMRequest
	if err := json.Unmarshal(job.Payload, &req); err != nil {
		return fmt.Errorf("decode llm job: %w", err)
	}
	model := req.Model
	if model == "" {
		model = job.Model
	}
	if r.Dispatcher != nil && r.Dispatcher.Models != nil {
		if spec, ok := r.Dispatcher.Models.Get(model); ok && spec.Provider == "google_colab" {
			if r.Workers == nil {
				return workerhub.ErrNoWorker
			}
			resp, err := r.Workers.Chat(ctx, provider.ChatRequest{Model: model, Messages: []provider.Message{{Role: "user", Content: req.Prompt}}, Temperature: req.Temperature, MaxTokens: req.MaxTokens})
			if err != nil {
				return err // Do not silently fall back to the outbound Colab adapter.
			}
			result, _ := json.Marshal(map[string]any{"text": resp.Content, "model": model, "usage": resp.Usage})
			return r.Jobs.CompleteWithOutput(context.Background(), job.ID, string(result), "", "application/json")
		}
	}
	if r.Dispatcher == nil {
		return errors.New("no LLM worker or dispatcher available")
	}
	if model == "" {
		return errors.New("LLM job model is required")
	}
	resp, err := r.Dispatcher.Chat(ctx, provider.ChatRequest{Model: model, Messages: []provider.Message{{Role: "user", Content: req.Prompt}}, Temperature: req.Temperature, MaxTokens: req.MaxTokens})
	if err != nil {
		return err
	}
	result, _ := json.Marshal(map[string]any{"text": resp.Content, "model": resp.Model, "usage": resp.Usage})
	return r.Jobs.CompleteWithOutput(context.Background(), job.ID, string(result), "", "application/json")
}
