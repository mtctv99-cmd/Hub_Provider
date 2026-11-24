package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
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

const (
	// DefaultJobConcurrency bounds process-wide jobs in flight. Video jobs can
	// hold up to 100 MB each, so an unbounded claim loop is an OOM risk.
	DefaultJobConcurrency = 8
	MaxJobConcurrency     = 64
	// DefaultVideoTimeout is shared by the runner, the supervisor's stale
	// window and the startup check.
	DefaultVideoTimeout = 45 * time.Minute
	// VideoStaleBuffer is added to VideoTimeout before a PROCESSING video job
	// may be reset; the supervisor also enforces a 60m floor.
	VideoStaleBuffer = 15 * time.Minute
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
	VideoTimeout      time.Duration
	WorkerID          string
	TikTokConcurrency int
	TikTokSlots       chan struct{}
	Settings          *settings.Store
	// MaxConcurrency caps jobs in flight across the runner. Zero uses
	// JOB_MAX_CONCURRENCY or DefaultJobConcurrency.
	MaxConcurrency int
	// Slots is the concurrency semaphore. It is created by Start.
	Slots chan struct{}
	Once  sync.Once

	// processFn overrides the type-specific processor in tests. It never
	// changes production behavior because it is unexported.
	processFn func(context.Context, jobstore.Job) error

	wg       sync.WaitGroup
	loopDone chan struct{}
}

func (r *Runner) Start(ctx context.Context) {
	if r.Poll <= 0 {
		r.Poll = 200 * time.Millisecond
	}
	if r.Timeout <= 0 {
		r.Timeout = 5 * time.Minute
	}
	if r.VideoTimeout <= 0 {
		// Colab/T4 video generation is far slower than chat; keep the stale-job
		// reaper from killing a healthy generation.
		r.VideoTimeout = DefaultVideoTimeout
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
	r.initConcurrency()
	r.Once.Do(func() {
		r.loopDone = make(chan struct{})
		go func() {
			defer close(r.loopDone)
			r.loop(ctx)
		}()
	})
}

func (r *Runner) initConcurrency() {
	if r.MaxConcurrency <= 0 && r.Slots != nil {
		r.MaxConcurrency = cap(r.Slots)
	}
	if r.MaxConcurrency <= 0 {
		r.MaxConcurrency = jobConcurrencyFromEnv()
	}
	if r.MaxConcurrency < 1 {
		r.MaxConcurrency = 1
	}
	if r.MaxConcurrency > MaxJobConcurrency {
		r.MaxConcurrency = MaxJobConcurrency
	}
	if r.Slots == nil {
		r.Slots = make(chan struct{}, r.MaxConcurrency)
	}
}

func jobConcurrencyFromEnv() int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("JOB_MAX_CONCURRENCY")))
	if err != nil || value <= 0 {
		return DefaultJobConcurrency
	}
	if value > MaxJobConcurrency {
		return MaxJobConcurrency
	}
	return value
}

// tryAcquireSlot takes a job slot without blocking. The caller must not claim a
// job when it returns false.
func (r *Runner) tryAcquireSlot() bool {
	select {
	case r.Slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (r *Runner) releaseSlot() {
	<-r.Slots
}

// Wait blocks until the claim loop has stopped and every in-flight job has
// returned. Call it after canceling the Start context; it never claims new work.
func (r *Runner) Wait(ctx context.Context) error {
	if r.loopDone != nil {
		select {
		case <-r.loopDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) loop(ctx context.Context) {
	ticker := time.NewTicker(r.Poll)
	defer ticker.Stop()
	// Recover abandoned PROCESSING rows from a previous process, keeping video
	// jobs on their longer window.
	if n, err := r.Jobs.ResetStaleExceptType(ctx, r.Timeout, "video"); err == nil && n > 0 {
		log.Printf("[JOBS] reset %d stale non-video processing jobs", n)
	}
	if n, err := r.Jobs.ResetStaleType(ctx, r.VideoTimeout+VideoStaleBuffer, "video"); err == nil && n > 0 {
		log.Printf("[JOBS] reset %d stale video processing jobs", n)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := 0; i < 8; i++ {
				if !r.tryAcquireSlot() {
					break
				}
				job, err := r.Jobs.ClaimNext(ctx, r.WorkerID)
				if errors.Is(err, jobstore.ErrNoJobs) {
					r.releaseSlot()
					break
				}
				if err != nil {
					r.releaseSlot()
					break
				}
				r.wg.Add(1)
				go func(job jobstore.Job) {
					defer r.wg.Done()
					defer r.releaseSlot()
					r.process(ctx, job)
				}(job)
			}
		}
	}
}

func (r *Runner) process(parent context.Context, job jobstore.Job) {
	timeout := r.Timeout
	if strings.EqualFold(job.Type, "video") && r.VideoTimeout > 0 {
		timeout = r.VideoTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	var err error
	if r.processFn != nil {
		err = r.processFn(ctx, job)
	} else {
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
	}
	if err == nil {
		return
	}
	if parent.Err() != nil {
		// Shutdown/caller cancellation is not a job failure. Put the job back
		// so the next process can claim it instead of marking it FAILED.
		if requeueErr := r.Jobs.Requeue(context.Background(), job.ID); requeueErr != nil && !errors.Is(requeueErr, jobstore.ErrNotFound) {
			log.Printf("[JOBS] requeue %s during shutdown failed: %v", job.ID, requeueErr)
		}
		return
	}
	_ = r.Jobs.Fail(context.Background(), job.ID, err)
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
	if r.Dispatcher == nil && r.Workers == nil {
		return errors.New("no video backend is configured")
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
	videoReq := provider.VideoRequest{
		Model:     model,
		Prompt:    req.Prompt,
		Duration:  req.Duration,
		Size:      req.Size,
		SessionID: req.SessionID,
	}
	var resp provider.VideoResponse
	var err error
	if r.Workers != nil && r.Workers.Available(model) {
		resp, err = r.Workers.Video(ctx, videoReq)
	} else {
		resp, err = r.Dispatcher.Video(ctx, videoReq)
	}
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
// It reuses the image download SSRF guard: https-only, no credentials/IP
// literals, public DNS resolution and private/loopback/CGNAT addresses rejected
// on every redirect.
func downloadUpstreamVideo(ctx context.Context, rawURL string) ([]byte, string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if err := provider.ValidateImageURL(rawURL, false); err != nil {
		return nil, "", fmt.Errorf("invalid video url: %w", err)
	}
	client, closeIdle := provider.NewImageClient(120*time.Second, false)
	defer closeIdle()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
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
