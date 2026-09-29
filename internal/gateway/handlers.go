package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/browser"
	"sever_provider/internal/colabsupervisor"
	"sever_provider/internal/filestore"
	"sever_provider/internal/identity"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/session"
	"sever_provider/internal/settings"
	"sever_provider/internal/workerhub"
)

type IdentityReader interface {
	List(ctx context.Context, domain string) ([]identity.Credential, error)
}

type IdentityDetailsReader interface {
	ListIdentityDetails(ctx context.Context, providerGroup string) ([]identity.IdentityDetail, error)
	GetIdentityDetail(ctx context.Context, id string) (identity.IdentityDetail, error)
}

type GeminiValidator interface {
	ValidateCookie(ctx context.Context, cookie string) error
}

type Handler struct {
	Dispatcher       *scheduler.Dispatcher
	Models           *scheduler.ModelRegistry
	Credentials      IdentityReader
	IdentityDetails  IdentityDetailsReader
	Leases           *scheduler.LeaseManager
	Breakers         *scheduler.BreakerRegistry
	Jobs             *jobstore.Store
	MediaStore       *filestore.Store
	Workers          *workerhub.Hub
	AdminAuth        *admin.Auth
	APIKeys          *admin.APIKeys
	AuditLog         *admin.AuditLogger
	Traffic          *TrafficStore
	ColabSupervisor  *colabsupervisor.Supervisor
	Browser          *browser.Manager
	Sessions         *session.Manager
	GeminiValidator  GeminiValidator
	Settings         *settings.Store
	TikTokAsyncSlots int
	TikTokPool       interface {
		AddSession(string)
		RemoveSession(string)
	}
	StartedAt       time.Time
	Requests        atomic.Uint64
	TotalDurationMs atomic.Uint64
	AITokens        atomic.Uint64
	VoiceStats      sync.Map
}

func NewHandler(dispatcher *scheduler.Dispatcher, models *scheduler.ModelRegistry) *Handler {
	return &Handler{Dispatcher: dispatcher, Models: models, StartedAt: time.Now()}
}

func (h *Handler) SetCredentials(reader IdentityReader) {
	h.Credentials = reader
	if details, ok := reader.(IdentityDetailsReader); ok {
		h.IdentityDetails = details
	}
}
func (h *Handler) SetIdentityDetails(reader IdentityDetailsReader) { h.IdentityDetails = reader }
func (h *Handler) SetLeases(leases *scheduler.LeaseManager)        { h.Leases = leases }
func (h *Handler) SetBreakers(breakers *scheduler.BreakerRegistry) { h.Breakers = breakers }
func (h *Handler) SetJobs(jobs *jobstore.Store)                    { h.Jobs = jobs }
func (h *Handler) SetFiles(files *filestore.Store)                 { h.MediaStore = files }
func (h *Handler) SetWorkers(workers *workerhub.Hub) {
	h.Workers = workers
	if h.Dispatcher != nil {
		h.Dispatcher.Workers = workers
	}
}
func (h *Handler) SetGeminiValidator(v GeminiValidator) { h.GeminiValidator = v }
func (h *Handler) SetSettings(store *settings.Store) {
	h.Settings = store
	if h.Dispatcher != nil {
		h.Dispatcher.Settings = store
	}
}
func (h *Handler) SetBrowserManager(b *browser.Manager) { h.Browser = b }
func (h *Handler) SetTikTokPool(pool interface {
	AddSession(string)
	RemoveSession(string)
}) {
	h.TikTokPool = pool
}

func (h *Handler) recordVoiceRequest(voiceID string) {
	voiceID = strings.TrimSpace(voiceID)
	if voiceID == "" {
		voiceID = "default"
	}
	actual, _ := h.VoiceStats.LoadOrStore(voiceID, &atomic.Uint64{})
	if counter, ok := actual.(*atomic.Uint64); ok {
		counter.Add(1)
	}
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "uptime_seconds": int(time.Since(h.StartedAt).Seconds()), "requests_total": h.Requests.Load()})
}

func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if h == nil || h.Models == nil || h.Dispatcher == nil {
		writeError(w, http.StatusServiceUnavailable, "server dependencies are not ready", "server_error", "dependencies_unready")
		return
	}
	models := h.Models.List()
	if len(models) == 0 {
		writeError(w, http.StatusServiceUnavailable, "no models are configured", "server_error", "models_unready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "models": len(models), "uptime_seconds": int(time.Since(h.StartedAt).Seconds())})
}

func (h *Handler) ModelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	type item struct {
		ID        string `json:"id"`
		Object    string `json:"object"`
		Created   int64  `json:"created"`
		OwnedBy   string `json:"owned_by"`
		Available bool   `json:"available,omitempty"`
	}
	items := make([]item, 0)
	for _, spec := range h.Models.List() {
		items = append(items, item{ID: spec.ID, Object: "model", Created: 1700000000, OwnedBy: "maika-hub", Available: h.modelAvailable(spec)})
	}
	if h.Workers != nil {
		seen := make(map[string]bool, len(items))
		for _, model := range items {
			seen[model.ID] = true
		}
		for _, worker := range h.Workers.Snapshot() {
			for _, modelID := range append([]string{worker.Model}, worker.Models...) {
				if modelID != "" && !seen[modelID] {
					items = append(items, item{ID: modelID, Object: "model", Created: 1700000000, OwnedBy: "maika-hub", Available: h.Workers.Available(modelID)})
					seen[modelID] = true
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items})
}

func (h *Handler) modelAvailable(spec scheduler.ModelSpec) bool {
	if h.Breakers != nil && !h.Breakers.Get(spec.Provider).Allow(time.Now()) {
		return false
	}
	if spec.Provider == "google_colab" {
		return h.Workers != nil && h.Workers.Available(spec.ID)
	}
	if spec.Provider == "piper_local" {
		voiceID := strings.TrimPrefix(spec.ID, "piper:")
		if h.Settings != nil && !h.Settings.GetBool(context.Background(), "tts.voice."+voiceID+".enabled", true) {
			return false
		}
		mode := "auto"
		if h.Settings != nil {
			mode = h.Settings.Get(context.Background(), "tts.piper_mode", "auto")
		}
		localReady := false
		if mode != "remote" && h.Dispatcher != nil && h.Dispatcher.Adapters != nil {
			if adapter, ok := h.Dispatcher.Adapters.Get("piper_local"); ok {
				if voiceList, ok := adapter.(interface{ Voices() []piper.Voice }); ok {
					for _, voice := range voiceList.Voices() {
						if voice.ID == voiceID {
							localReady = true
							break
						}
					}
				}
				if ready, ok := adapter.(interface{ Ready() bool }); !ok || !ready.Ready() {
					localReady = false
				}
			}
		}
		remoteReady := h.Workers != nil && h.Workers.Available(spec.ID)
		switch mode {
		case "local":
			return localReady
		case "remote":
			return remoteReady
		default:
			return localReady || remoteReady
		}
	}

	if spec.Provider == "tiktok_api" && !h.tiktokReady() {
		return false
	}
	return true
}

func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	started := time.Now()
	var req ChatCompletionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		writeError(w, 400, "model and messages are required", "invalid_request_error", "missing_parameter")
		return
	}
	hasImages := false
	for _, message := range req.Messages {
		if message.HasImages() {
			hasImages = true
		}
		for _, part := range message.Parts {
			if part.Type == "image_url" && part.ImageURL != nil && len(part.ImageURL.URL) > 16*1024*1024 {
				writeError(w, 413, "image payload too large", "invalid_request_error", "image_too_large")
				return
			}
		}
	}
	if hasImages && (h.Models == nil || !h.Models.HasCapability(req.Model, provider.CapabilityVision)) {
		writeError(w, http.StatusBadRequest, "selected model does not support image inputs", "invalid_request_error", "unsupported_capability")
		return
	}
	ctx := r.Context()
	chatReq := provider.ChatRequest{Model: req.Model, SessionID: r.Header.Get("X-Session-ID"), Messages: req.Messages, Tools: req.Tools, ToolChoice: req.ToolChoice, Stream: req.Stream, Temperature: req.Temperature, MaxTokens: req.MaxTokens}
	isWorkerModel := false
	if spec, ok := h.Models.Get(req.Model); ok && spec.Provider == "google_colab" {
		isWorkerModel = true
	} else if h.Workers != nil && h.Workers.Available(req.Model) {
		isWorkerModel = true
	}
	if isWorkerModel {
		if req.Stream {
			h.streamColab(w, ctx, chatReq)
			return
		}
		if h.Workers == nil {
			writeError(w, http.StatusServiceUnavailable, "Worker unavailable", "provider_error", "worker_unavailable")
			return
		}
		resp, err := h.Workers.Chat(ctx, chatReq)
		if err != nil {
			status, message, code := maskColabError(err)
			writeError(w, status, message, "provider_error", code)
			return
		}
		if strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0 {
			writeError(w, http.StatusBadGateway, "upstream worker returned empty response", "provider_error", "empty_response")
			return
		}
		resp.Usage = provider.ComputeUsage(req.Messages, req.Tools, resp.Content, resp.ReasoningContent, resp.ToolCalls, &resp.Usage)
		h.AITokens.Add(uint64(resp.Usage.TotalTokens))
		if h.Traffic != nil && resp.Usage.TotalTokens > 0 {
			go func(tok int) { _ = h.Traffic.RecordMetrics(context.Background(), time.Now(), 0, int64(tok), 0) }(resp.Usage.TotalTokens)
		}
		finish := "stop"
		if resp.FinishReason != "" {
			finish = resp.FinishReason
		}
		msg := &provider.Message{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: resp.ToolCalls}
		writeJSON(w, 200, ChatCompletionResponse{ID: resp.ID, Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model, Choices: []Choice{{Index: 0, Message: msg, FinishReason: &finish}}, Usage: resp.Usage})
		return
	}
	if req.Stream {
		h.streamChat(w, ctx, chatReq)
		return
	}
	resp, err := h.Dispatcher.Chat(ctx, chatReq)
	if err != nil {
		log.Printf("[GATEWAY] chat completion failed: %v", err)
		status, userMsg, code := maskProviderError(err)
		if h.AuditLog != nil {
			_ = h.AuditLog.Record(ctx, admin.AuditEvent{
				RequestID: r.Header.Get("X-Request-ID"),
				EventType: "chat_completion",
				Model:     req.Model,
				Latency:   time.Since(started),
				ErrorKind: code,
				Detail:    userMsg,
			})
		}
		writeError(w, status, userMsg, "provider_error", code)
		return
	}
	resp.Usage = provider.ComputeUsage(chatReq.Messages, chatReq.Tools, resp.Content, resp.ReasoningContent, resp.ToolCalls, &resp.Usage)
	h.AITokens.Add(uint64(resp.Usage.TotalTokens))
	if h.Traffic != nil && resp.Usage.TotalTokens > 0 {
		go func(tok int) { _ = h.Traffic.RecordMetrics(context.Background(), time.Now(), 0, int64(tok), 0) }(resp.Usage.TotalTokens)
	}
	if h.AuditLog != nil {
		_ = h.AuditLog.Record(ctx, admin.AuditEvent{
			RequestID: r.Header.Get("X-Request-ID"),
			EventType: "chat_completion",
			Model:     req.Model,
			Latency:   time.Since(started),
			Detail:    "success",
		})
	}
	finish := "stop"
	if resp.FinishReason != "" {
		finish = resp.FinishReason
	}
	msg := &provider.Message{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: resp.ToolCalls}
	writeJSON(w, 200, ChatCompletionResponse{ID: resp.ID, Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model, Choices: []Choice{{Index: 0, Message: msg, FinishReason: &finish}}, Usage: resp.Usage})
}

func (h *Handler) streamChat(w http.ResponseWriter, ctx context.Context, req provider.ChatRequest) {
	stream, err := h.Dispatcher.StreamChat(ctx, req)
	if err != nil {
		log.Printf("[GATEWAY] streaming chat failed: %v", err)
		status, userMsg, code := maskProviderError(err)
		writeError(w, status, userMsg, "provider_error", code)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported", "server_error", "streaming_unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	id := "chatcmpl-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	created := time.Now().Unix()
	send := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b); flusher.Flush() }
	var streamContent strings.Builder
	var streamReasoning strings.Builder
	var streamToolCalls []provider.ToolCall
	var streamUsage *provider.Usage
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-stream:
			if !ok {
				toks := provider.ComputeUsage(req.Messages, req.Tools, streamContent.String(), streamReasoning.String(), streamToolCalls, streamUsage)
				h.AITokens.Add(uint64(toks.TotalTokens))
				if h.Traffic != nil && toks.TotalTokens > 0 {
					go func(tok int) { _ = h.Traffic.RecordMetrics(context.Background(), time.Now(), 0, int64(tok), 0) }(toks.TotalTokens)
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			if chunk.Err != nil {
				log.Printf("[GATEWAY] streaming provider error: %v", chunk.Err)
				_, userMsg, code := maskProviderError(chunk.Err)
				send(ErrorEnvelope{Error: APIError{Message: userMsg, Type: "provider_error", Code: code}})
				return
			}
			streamContent.WriteString(chunk.Content)
			streamReasoning.WriteString(chunk.ReasoningContent)
			if len(chunk.ToolCalls) > 0 {
				streamToolCalls = append(streamToolCalls, chunk.ToolCalls...)
			}
			if chunk.Usage != nil {
				streamUsage = chunk.Usage
			}
			delta := &provider.Message{Role: "assistant", Content: chunk.Content, ReasoningContent: chunk.ReasoningContent, ToolCalls: chunk.ToolCalls}
			var finish *string
			if chunk.FinishReason != "" {
				finish = &chunk.FinishReason
			} else if chunk.Done {
				s := "stop"
				finish = &s
			}
			send(ChatCompletionResponse{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model, Choices: []Choice{{Index: 0, Delta: delta, FinishReason: finish}}, Usage: usageOrEmpty(chunk.Usage)})
			if chunk.Done {
				toks := provider.ComputeUsage(req.Messages, req.Tools, streamContent.String(), streamReasoning.String(), streamToolCalls, streamUsage)
				h.AITokens.Add(uint64(toks.TotalTokens))
				if h.Traffic != nil && toks.TotalTokens > 0 {
					go func(tok int) { _ = h.Traffic.RecordMetrics(context.Background(), time.Now(), 0, int64(tok), 0) }(toks.TotalTokens)
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
		}
	}
}

func usageOrEmpty(v *provider.Usage) provider.Usage {
	if v == nil {
		return provider.Usage{}
	}
	return *v
}

func (h *Handler) AudioSpeech(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	started := time.Now()
	var req provider.SpeechRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Model == "" || req.Input == "" {
		writeError(w, 400, "model and input are required", "invalid_request_error", "missing_parameter")
		return
	}
	data, mime, err := h.Dispatcher.Speech(r.Context(), req)
	if err != nil {
		log.Printf("[GATEWAY] speech synthesis failed: %v", err)
		status, userMsg, code := maskProviderError(err)
		if h.AuditLog != nil {
			_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{RequestID: r.Header.Get("X-Request-ID"), EventType: "speech", Model: req.Model, Latency: time.Since(started), ErrorKind: code, Detail: userMsg})
		}
		writeError(w, status, userMsg, "provider_error", code)
		return
	}
	if h.AuditLog != nil {
		_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{RequestID: r.Header.Get("X-Request-ID"), EventType: "speech", Model: req.Model, Latency: time.Since(started), Detail: "success"})
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(200)
	_, _ = w.Write(data)
	if h.Traffic != nil && len(data) > 0 {
		sec := int64(len(data) / 44100)
		if sec == 0 {
			sec = 1
		}
		go func(s int64) { _ = h.Traffic.RecordMetrics(context.Background(), time.Now(), 0, 0, s) }(sec)
	}
}

func (h *Handler) Images(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	started := time.Now()
	var req ImageGenerationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Model == "" || req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "model and prompt are required", "invalid_request_error", "missing_parameter")
		return
	}
	if req.N != 0 && req.N != 1 || req.AspectRatio != "" && req.AspectRatio != "auto" || req.Style != "" || req.Quality != "" || req.Size != "" {
		writeError(w, http.StatusBadRequest, "requested image option is not supported by this model", "invalid_request_error", "unsupported_option")
		return
	}
	if req.ResponseFormat != "" && req.ResponseFormat != "url" && req.ResponseFormat != "b64_json" {
		writeError(w, http.StatusBadRequest, "response_format must be url or b64_json", "invalid_request_error", "unsupported_option")
		return
	}
	resp, err := h.Dispatcher.Image(r.Context(), provider.ImageRequest{Model: req.Model, Prompt: req.Prompt, Size: req.Size})
	if err != nil {
		log.Printf("[GATEWAY] image generation failed: %v", err)
		status, userMsg, code := maskProviderError(err)
		if h.AuditLog != nil {
			_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{
				RequestID: r.Header.Get("X-Request-ID"),
				EventType: "image_generation",
				Model:     req.Model,
				Latency:   time.Since(started),
				ErrorKind: code,
				Detail:    userMsg,
			})
		}
		writeError(w, status, userMsg, "provider_error", code)
		return
	}
	if h.AuditLog != nil {
		_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{
			RequestID: r.Header.Get("X-Request-ID"),
			EventType: "image_generation",
			Model:     req.Model,
			Latency:   time.Since(started),
			Detail:    "success",
		})
	}
	if req.ResponseFormat == "b64_json" {
		if len(resp.Data) == 0 {
			writeError(w, http.StatusBadGateway, "image bytes unavailable for base64 response", "provider_error", "image_bytes_unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(resp.Data)}}})
		return
	}
	if len(resp.Data) > 0 && h.MediaStore != nil {
		ext := ".png"
		if resp.MIME == "image/jpeg" {
			ext = ".jpg"
		} else if resp.MIME == "image/webp" {
			ext = ".webp"
		} else if resp.MIME == "image/gif" {
			ext = ".gif"
		}
		id, saveErr := h.MediaStore.Save(resp.Data, ext)
		if saveErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to store generated image", "server_error", "file_store_failed")
			return
		}
		resp.URL = "/v1/files/" + id
	}
	if resp.URL == "" {
		writeError(w, http.StatusServiceUnavailable, "image storage unavailable", "server_error", "image_storage_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": []map[string]string{{"url": resp.URL}}})
}

func (h *Handler) DashboardStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	uptime := time.Since(h.StartedAt).Seconds()
	if uptime < 1 {
		uptime = 1
	}
	requests := h.Requests.Load()
	voiceStats := make(map[string]uint64)
	h.VoiceStats.Range(func(key, value any) bool {
		if name, ok := key.(string); ok {
			if count, ok := value.(*atomic.Uint64); ok {
				voiceStats[name] = count.Load()
			}
		}
		return true
	})
	status := map[string]any{
		"status":              "ok",
		"models":              len(h.Models.List()),
		"requests_total":      requests,
		"requests_per_second": float64(requests) / uptime,
		"requests_per_minute": float64(requests) * 60 / uptime,
		"total_duration_ms":   h.TotalDurationMs.Load(),
		"ai_tokens_total":     h.AITokens.Load(),
		"voice_requests":      voiceStats,
		"endpoint_urls": map[string]string{
			"chat":   "/v1/chat/completions",
			"speech": "/v1/audio/speech",
			"models": "/v1/models",
			"status": "/internal/status",
		},
	}
	if h.Workers != nil {
		status["workers"] = h.Workers.Snapshot()
	}
	if h.Jobs != nil {
		if pending, err := h.Jobs.CountByStatus(r.Context(), jobstore.PENDING); err == nil {
			status["pending_jobs"] = pending
		}
	}
	if h.Leases != nil {
		status["active_leases"] = h.Leases.ActiveCount()
	} else {
		status["active_leases"] = 0
	}
	if h.Breakers != nil {
		status["breakers"] = h.Breakers.Snapshot()
	} else {
		status["breakers"] = map[string]string{}
	}
	if h.Credentials != nil {
		for _, domain := range []string{"gemini_web", "google_colab", "copilot_ws", "piper_local", "tiktok_api", "deepseek_web"} {
			items, err := h.Credentials.List(r.Context(), domain)
			if err == nil {
				if h.Leases != nil {
					for i := range items {
						items[i].ActiveLeases = h.Leases.ActiveLeasesFor(items[i].ID)
					}
				}
				status[domain] = items
			}
		}
	}
	if h.IdentityDetails != nil {
		if accounts, err := h.IdentityDetails.ListIdentityDetails(r.Context(), "google"); err == nil {
			if h.Leases != nil {
				for _, acc := range accounts {
					for _, c := range acc.Credentials {
						if c != nil {
							c.ActiveLeases = h.Leases.ActiveLeasesFor(c.ID)
						}
					}
				}
			}
			status["accounts"] = accounts
		}
	}
	if h.AuditLog != nil {
		if recentLogs, err := h.AuditLog.Recent(r.Context(), 100); err == nil {
			status["recent_requests"] = recentLogs
			providerRequests := map[string]int{}
			for _, line := range recentLogs {
				provider := strings.TrimSpace(line.Provider)
				if provider == "" {
					provider = modelProvider(line.Model)
				}
				if provider != "" {
					providerRequests[provider]++
				}
			}
			status["provider_requests"] = providerRequests
		}
	}
	writeJSON(w, 200, status)
}
func modelProvider(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(model, "deepseek"):
		return "deepseek_web"
	case strings.Contains(model, "tiktok"), strings.HasPrefix(model, "bv0"):
		return "tiktok_api"
	case strings.Contains(model, "colab"), strings.Contains(model, "gemma"):
		return "google_colab"
	case strings.Contains(model, "gpt"), strings.Contains(model, "copilot"):
		return "copilot_ws"
	case strings.Contains(model, "gemini"), strings.Contains(model, "gemma"):
		return "gemini_web"
	default:
		return ""
	}
}

func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported", "server_error", "streaming_unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	flusher.Flush()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			payload := map[string]any{"requests_total": h.Requests.Load(), "uptime_seconds": int(time.Since(h.StartedAt).Seconds())}
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

func (h *Handler) LegacyTTS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Text    string `json:"text"`
		VoiceID string `json:"voice_id"`
		Voice   string `json:"voice"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	voice := req.VoiceID
	if voice == "" {
		voice = req.Voice
	}
	model := "piper:" + strings.TrimPrefix(voice, "piper:")
	if strings.HasPrefix(voice, "tiktok:") {
		model = voice
	}
	audio, mime, err := h.Dispatcher.Speech(r.Context(), provider.SpeechRequest{Model: model, Input: req.Text, Voice: voice})
	if err != nil {
		log.Printf("[GATEWAY] legacy tts failed: %v", err)
		status, userMsg, code := maskProviderError(err)
		writeError(w, status, userMsg, "provider_error", code)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Write(audio)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(r.Body).Decode(v)
	if err == nil {
		return true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) || strings.Contains(strings.ToLower(err.Error()), "request body too large") {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error", "payload_too_large")
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid JSON", "invalid_request_error", "invalid_json")
	return false
}

func maskProviderError(err error) (int, string, string) {
	if err == nil {
		return http.StatusOK, "", ""
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(lower, "timeout") {
		return http.StatusGatewayTimeout, "upstream provider request timed out", "upstream_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return 499, "request cancelled", "request_cancelled"
	}
	if strings.Contains(lower, "no available credential") {
		return http.StatusServiceUnavailable, "no available provider credential", "no_available_credential"
	}
	if strings.Contains(lower, "unsupported") {
		return http.StatusBadRequest, "requested capability unsupported by provider", "unsupported_capability"
	}
	if strings.Contains(lower, "not configured") {
		return http.StatusNotFound, "model is not configured", "model_not_found"
	}
	return http.StatusBadGateway, "upstream provider failed to complete request", "provider_unavailable"
}

func scanSSE(body string) []string {
	var out []string
	s := bufio.NewScanner(strings.NewReader(body))
	for s.Scan() {
		if strings.HasPrefix(s.Text(), "data:") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(s.Text(), "data:")))
		}
	}
	return out
}
