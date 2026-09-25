package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/identity"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/scheduler"
)

func (h *Handler) AnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	started := time.Now()
	var req AnthropicMessageRequest
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
				writeError(w, http.StatusRequestEntityTooLarge, "image payload too large", "invalid_request_error", "image_too_large")
				return
			}
		}
	}
	if hasImages && (h.Models == nil || !h.Models.HasCapability(req.Model, provider.CapabilityVision)) {
		writeError(w, http.StatusBadRequest, "selected model does not support image inputs", "invalid_request_error", "unsupported_capability")
		return
	}
	chatReq := provider.ChatRequest{Model: req.Model, SessionID: r.Header.Get("X-Session-ID"), Messages: req.Messages, MaxTokens: req.MaxTokens, Stream: req.Stream}
	if req.Stream {
		h.streamAnthropic(w, r.Context(), chatReq)
		return
	}
	resp, err := h.Dispatcher.Chat(r.Context(), chatReq)
	if err != nil {
		status, msg, code := maskProviderError(err)
		if h.AuditLog != nil {
			_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{
				RequestID: r.Header.Get("X-Request-ID"),
				EventType: "anthropic_message",
				Model:     req.Model,
				Latency:   time.Since(started),
				ErrorKind: code,
				Detail:    msg,
			})
		}
		writeError(w, status, msg, "api_error", code)
		return
	}
	if h.AuditLog != nil {
		_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{
			RequestID: r.Header.Get("X-Request-ID"),
			EventType: "anthropic_message",
			Model:     req.Model,
			Latency:   time.Since(started),
			Detail:    "success",
		})
	}
	var contentBlocks []map[string]any
	if resp.ReasoningContent != "" {
		contentBlocks = append(contentBlocks, map[string]any{
			"type":     "thinking",
			"thinking": resp.ReasoningContent,
		})
	}
	contentBlocks = append(contentBlocks, map[string]any{
		"type": "text",
		"text": resp.Content,
	})
	writeJSON(w, 200, map[string]any{"id": resp.ID, "type": "message", "role": "assistant", "model": req.Model, "content": contentBlocks, "stop_reason": "end_turn", "usage": resp.Usage})
}

func (h *Handler) streamAnthropic(w http.ResponseWriter, ctx context.Context, req provider.ChatRequest) {
	stream, err := h.Dispatcher.StreamChat(ctx, req)
	if err != nil {
		status, msg, code := maskProviderError(err)
		writeError(w, status, msg, "api_error", code)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported", "server_error", "streaming_unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	id := "msg_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	send := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}
		send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": req.Model, "content": []any{}, "stop_reason": nil}})
		thinkingStarted := false
		textStarted := false
		blockIndex := 0

		for chunk := range stream {
			if chunk.Err != nil {
				status, msg, code := maskProviderError(chunk.Err)
				send("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": msg, "code": code}})
				_ = status
				return
			}
			if chunk.ReasoningContent != "" {
				if !thinkingStarted {
					thinkingStarted = true
					send("content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
				}
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": chunk.ReasoningContent}})
			}
			if chunk.Content != "" {
				if thinkingStarted && !textStarted {
					send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
					blockIndex++
				}
				if !textStarted {
					textStarted = true
					send("content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "text", "text": ""}})
				}
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "text_delta", "text": chunk.Content}})
			}
			if chunk.Done {
				if !thinkingStarted && !textStarted {
					send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
					textStarted = true
				}
				send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
				send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": usageOrEmpty(chunk.Usage)})
				send("message_stop", map[string]any{"type": "message_stop"})
				return
			}
		}
		if textStarted || thinkingStarted {
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
		}
		send("message_stop", map[string]any{"type": "message_stop"})
}

func (h *Handler) VideoGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	writeError(w, http.StatusNotImplemented, "video provider adapter is not connected yet", "provider_error", "not_implemented")
}

func (h *Handler) VideoTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	id := path.Base(strings.TrimSuffix(r.URL.Path, "/"))
	if h.Jobs == nil {
		writeError(w, 503, "job store is unavailable", "server_error", "job_store_unavailable")
		return
	}
	job, err := h.Jobs.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, jobstore.ErrNotFound) || errors.Is(err, context.Canceled) {
			writeError(w, 404, "video task not found", "invalid_request_error", "not_found")
			return
		}
		writeError(w, 500, "failed to read video task", "server_error", "job_store_failed")
		return
	}
	if job.Kind != "video" && job.Type != "video" {
		writeError(w, http.StatusNotFound, "video task not found", "invalid_request_error", "not_found")
		return
	}
	writeJSON(w, 200, job)
}

func (h *Handler) Files(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if h.MediaStore == nil {
		writeError(w, 503, "file store is unavailable", "server_error", "file_store_unavailable")
		return
	}
	id := path.Base(strings.TrimPrefix(r.URL.Path, "/v1/files/"))
	f, err := h.MediaStore.Open(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", h.MediaStore.ContentType(id))
	http.ServeContent(w, r, id, time.Time{}, f)
}

func (h *Handler) TTSVoices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	byID := make(map[string]map[string]any)
	local := make(map[string]piper.Voice)
	if h.Dispatcher != nil && h.Dispatcher.Adapters != nil {
		if adapter, ok := h.Dispatcher.Adapters.Get("piper_local"); ok {
			if discovered, ok := adapter.(interface{ Voices() []piper.Voice }); ok {
				for _, voice := range discovered.Voices() {
					local[voice.ID] = voice
				}
			}
		}
	}
	if h.Models != nil {
		for _, spec := range h.Models.List() {
			if !strings.HasPrefix(spec.ID, "piper:") {
				continue
			}
			voiceID := strings.TrimPrefix(spec.ID, "piper:")
			enabled := h.Settings == nil || h.Settings.GetBool(r.Context(), "tts.voice."+voiceID+".enabled", true)
			byID[voiceID] = map[string]any{"voice_id": voiceID, "name": voiceID, "engine": "piper", "language": piperLanguage(voiceID), "available": enabled && h.modelAvailable(spec), "enabled": enabled}
		}
	}
	for voiceID, voice := range local {
		if _, ok := byID[voiceID]; !ok {
			enabled := h.Settings == nil || h.Settings.GetBool(r.Context(), "tts.voice."+voiceID+".enabled", true)
			byID[voiceID] = map[string]any{"voice_id": voiceID, "name": voice.Name, "engine": voice.Engine, "language": voice.Language, "available": enabled && h.modelAvailable(scheduler.ModelSpec{ID: "piper:" + voiceID, Provider: "piper_local"}), "enabled": enabled}
		}
	}
	if h.Workers != nil {
		for _, worker := range h.Workers.Snapshot() {
			for _, model := range append([]string{worker.Model}, worker.Models...) {
				if !strings.HasPrefix(model, "piper:") {
					continue
				}
				voiceID := strings.TrimPrefix(model, "piper:")
				if _, ok := byID[voiceID]; !ok {
					enabled := h.Settings == nil || h.Settings.GetBool(r.Context(), "tts.voice."+voiceID+".enabled", true)
					byID[voiceID] = map[string]any{"voice_id": voiceID, "name": voiceID, "engine": "piper", "language": piperLanguage(voiceID), "available": enabled && h.Workers.Available(model), "enabled": enabled, "remote": true}
				}
			}
		}
	}
	voiceIDs := make([]string, 0, len(byID))
	for voiceID := range byID {
		voiceIDs = append(voiceIDs, voiceID)
	}
	sort.Strings(voiceIDs)
	voices := make([]map[string]any, 0, len(byID)+3)
	for _, voiceID := range voiceIDs {
		voices = append(voices, byID[voiceID])
	}
	voices = append(voices,
		map[string]any{"voice_id": "vi_male", "name": "TikTok Vietnamese Male", "engine": "tiktok", "language": "vi", "available": h.tiktokReady()},
		map[string]any{"voice_id": "BV074_streaming", "name": "TikTok Vietnamese Female", "engine": "tiktok", "language": "vi", "available": h.tiktokReady()},
		map[string]any{"voice_id": "BV075_streaming", "name": "TikTok Vietnamese Male", "engine": "tiktok", "language": "vi", "available": h.tiktokReady()},
	)
	writeJSON(w, 200, map[string]any{"voices": voices})
}

func piperLanguage(voiceID string) string {
	lower := strings.ToLower(voiceID)
	switch {
	case strings.HasPrefix(lower, "th_"), strings.Contains(lower, "thai"):
		return "th"
	case strings.HasPrefix(lower, "khmer"), strings.Contains(lower, "km_"):
		return "km"
	default:
		return "vi"
	}
}

func (h *Handler) tiktokReady() bool {
	if h.Credentials == nil {
		return false
	}
	creds, err := h.Credentials.List(context.Background(), "tiktok_api")
	if err != nil {
		return false
	}
	for _, c := range creds {
		if c.Status == identity.StatusHealthy || c.Status == identity.StatusDegraded {
			return true
		}
	}
	return false
}

func (h *Handler) TTSGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	started := time.Now()
	var req TTSGenerateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Text == "" || req.VoiceID == "" {
		writeError(w, 400, "text and voice_id are required", "invalid_request_error", "missing_parameter")
		return
	}
	model := "piper:" + req.VoiceID
	if req.Engine == "tiktok" || strings.HasPrefix(req.VoiceID, "BV") || req.VoiceID == "vi_male" {
		model = "tiktok:" + req.VoiceID
	}
	if h.Models == nil {
		writeError(w, 503, "model registry is unavailable", "server_error", "models_unavailable")
		return
	}
	spec, configured := h.Models.Get(model)
	hasSpeech := false
	if configured {
		for _, cap := range spec.Capabilities {
			if cap == provider.CapabilitySpeech {
				hasSpeech = true
				break
			}
		}
	}
	remotePiper := strings.HasPrefix(model, "piper:") && h.Workers != nil && h.Workers.Available(model)
	if (!configured || !hasSpeech) && !remotePiper {
		writeError(w, 422, "requested TTS voice is not configured", "invalid_request_error", "voice_not_configured")
		return
	}
	if strings.HasPrefix(model, "piper:") && h.Settings != nil && !h.Settings.GetBool(r.Context(), "tts.voice."+req.VoiceID+".enabled", true) {
		writeError(w, 409, "requested Piper voice is disabled in settings", "invalid_request_error", "voice_disabled")
		return
	}
	if h.Jobs == nil {
		writeError(w, 503, "job store is unavailable", "server_error", "job_store_unavailable")
		return
	}
	h.recordVoiceRequest(req.VoiceID)
	payload, _ := json.Marshal(req)
	job := &jobstore.Job{Type: "tts", Kind: "tts", Model: model, Engine: req.Engine, Payload: payload, InputJSON: string(payload)}
	if err := h.Jobs.Create(r.Context(), job); err != nil {
		if h.AuditLog != nil {
			_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{
				RequestID: r.Header.Get("X-Request-ID"),
				EventType: "tts_job_create",
				Model:     model,
				Latency:   time.Since(started),
				ErrorKind: "job_store_failed",
				Detail:    err.Error(),
			})
		}
		writeError(w, 500, "failed to create TTS job", "server_error", "job_store_failed")
		return
	}
	if h.AuditLog != nil {
		_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{
			RequestID: r.Header.Get("X-Request-ID"),
			EventType: "tts_job_create",
			Model:     model,
			Latency:   time.Since(started),
			Detail:    "job_id=" + job.ID,
		})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "status": string(job.Status), "engine": req.Engine, "status_url": "/api/tts/jobs/" + job.ID})
}

func (h *Handler) TTSJob(w http.ResponseWriter, r *http.Request) {
	id := path.Base(strings.TrimSuffix(r.URL.Path, "/"))
	if strings.HasSuffix(r.URL.Path, "/audio") {
		id = path.Base(strings.TrimSuffix(r.URL.Path, "/audio"))
	}
	if h.Jobs == nil {
		writeError(w, 503, "job store is unavailable", "server_error", "job_store_unavailable")
		return
	}
	job, err := h.Jobs.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/audio") {
		if job.Status != jobstore.COMPLETED || job.OutputPath == "" || h.MediaStore == nil {
			writeError(w, http.StatusConflict, "audio is not ready", "invalid_request_error", "audio_not_ready")
			return
		}
		f, openErr := h.MediaStore.Open(job.OutputPath)
		if openErr != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", job.OutputMIME)
		http.ServeContent(w, r, id, time.Time{}, f)
		return
	}
	writeJSON(w, 200, job)
}

func (h *Handler) LLMJobCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	started := time.Now()
	var req AsyncJobRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Prompt == "" {
		writeError(w, 400, "prompt is required", "invalid_request_error", "missing_parameter")
		return
	}
	if h.Jobs == nil {
		writeError(w, 503, "job store is unavailable", "server_error", "job_store_unavailable")
		return
	}
	payload, _ := json.Marshal(req)
	job := &jobstore.Job{Type: "llm", Kind: "chat", Model: req.Model, Payload: payload, InputJSON: string(payload)}
	if err := h.Jobs.Create(r.Context(), job); err != nil {
		if h.AuditLog != nil {
			_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{
				RequestID: r.Header.Get("X-Request-ID"),
				EventType: "llm_job_create",
				Model:     req.Model,
				Latency:   time.Since(started),
				ErrorKind: "job_store_failed",
				Detail:    err.Error(),
			})
		}
		writeError(w, 500, "failed to create LLM job", "server_error", "job_store_failed")
		return
	}
	if h.AuditLog != nil {
		_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{
			RequestID: r.Header.Get("X-Request-ID"),
			EventType: "llm_job_create",
			Model:     req.Model,
			Latency:   time.Since(started),
			Detail:    "job_id=" + job.ID,
		})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "status": string(job.Status)})
}

func (h *Handler) LLMJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	id := path.Base(strings.TrimSuffix(r.URL.Path, "/"))
	if h.Jobs == nil {
		writeError(w, 503, "job store is unavailable", "server_error", "job_store_unavailable")
		return
	}
	job, err := h.Jobs.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, job)
}

func (h *Handler) WorkerWebSocket(w http.ResponseWriter, r *http.Request) {
	if h.Workers == nil {
		http.Error(w, "worker hub is unavailable", http.StatusServiceUnavailable)
		return
	}
	h.Workers.ServeHTTP(w, r)
}
