package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

type VideoJobRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	Duration  int    `json:"duration,omitempty"`
	Size      string `json:"size,omitempty"`
	SessionID string `json:"session_id,omitempty"`
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
		case "video":
			err = r.processVideo(ctx, job)
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
		if spec, ok := r.Dispatcher.Models.Get(model); ok && (spec.Provider == "google_colab" || spec.Provider == "vllm") {
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

// processVideo generates a video through the dispatcher, downloads the bytes
// when only a CDN URL is returned, and stores them in the media store.
func (r *Runner) processVideo(ctx context.Context, job jobstore.Job) error {
	if r.Dispatcher == nil {
		return errors.New("dispatcher is unavailable")
	}
	var req VideoJobRequest
	if err := json.Unmarshal(job.Payload, &req); err != nil {
		return fmt.Errorf("decode video job: %w", err)
	}
	model := req.Model
	if model == "" {
		model = job.Model
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return errors.New("video prompt is required")
	}
	resp, err := r.Dispatcher.Video(ctx, provider.VideoRequest{
		Model:     model,
		Prompt:    req.Prompt,
		Duration:  req.Duration,
		Size:      req.Size,
		SessionID: req.SessionID,
	})
	if err != nil {
		return err
	}
	data := resp.Data
	mime := resp.MIME
	if mime == "" {
		mime = "video/mp4"
	}
	if len(data) == 0 && resp.URL != "" {
		videoBytes, videoMIME, err := downloadUpstreamVideo(ctx, resp.URL)
		if err != nil {
			return err
		}
		data = videoBytes
		if videoMIME != "" {
			mime = videoMIME
		}
	}
	if len(data) == 0 {
		return errors.New("video provider returned no downloadable bytes")
	}
	if r.Files == nil {
		return errors.New("media store is unavailable")
	}
	ext := ".mp4"
	if strings.Contains(mime, "webm") {
		ext = ".webm"
	} else if strings.Contains(mime, "quicktime") || strings.Contains(strings.ToLower(ext), "mov") {
		ext = ".mov"
	}
	fileID, err := r.Files.Save(data, ext)
	if err != nil {
		return err
	}
	result, _ := json.Marshal(map[string]any{"video_url": "/v1/files/" + fileID, "file_id": fileID, "upstream_url": resp.URL})
	return r.Jobs.CompleteWithOutput(ctx, job.ID, string(result), fileID, mime)
}

const maxGeneratedVideoBytes = 100 * 1024 * 1024

// downloadUpstreamVideo fetches generated video bytes from a provider CDN URL.
func downloadUpstreamVideo(ctx context.Context, rawURL string) ([]byte, string, error) {
	videoURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || videoURL.Scheme != "https" || videoURL.Host == "" {
		return nil, "", fmt.Errorf("invalid video url")
	}
	client := &http.Client{Timeout: 120 * time.Second}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("video redirect limit exceeded")
		}
		if req.URL.Scheme != "https" {
			return errors.New("video redirect target must be https")
		}
		return nil
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, videoURL.String(), nil)
	if err != nil {
		return nil, "", err
	}
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("video download HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxGeneratedVideoBytes {
		return nil, "", errors.New("video exceeds maximum size")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedVideoBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxGeneratedVideoBytes {
		return nil, "", errors.New("video exceeds maximum size")
	}
	mime := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if mime == "" {
		mime = "video/mp4"
	}
	return data, mime, nil
}
