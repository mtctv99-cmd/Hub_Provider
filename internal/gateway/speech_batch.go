package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/provider"
	"sever_provider/internal/workerhub"
)

const (
	MaxBatchSizeCeiling  = 100
	DefaultFallbackSlots = 5
)

// BatchSpeechItem represents a single item inside a batch speech request.
type BatchSpeechItem struct {
	ID             string  `json:"id,omitempty"`
	Model          string  `json:"model,omitempty"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice,omitempty"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float64 `json:"speed,omitempty"`
}

// BatchSpeechRequest supports both format 1 (model + inputs list) and format 2 (items list).
type BatchSpeechRequest struct {
	Model          string            `json:"model,omitempty"`
	Voice          string            `json:"voice,omitempty"`
	ResponseFormat string            `json:"response_format,omitempty"`
	Speed          float64           `json:"speed,omitempty"`
	Inputs         []string          `json:"inputs,omitempty"`
	Items          []BatchSpeechItem `json:"items,omitempty"`
}

// BatchSpeechItemResponse represents the speech synthesis result for one item.
type BatchSpeechItemResponse struct {
	ID          string `json:"id"`
	Index       int    `json:"index"`
	AudioBase64 string `json:"audio_base64,omitempty"`
	B64JSON     string `json:"b64_json,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	MIMEType    string `json:"mime_type,omitempty"`
	Mime        string `json:"mime,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	LatencyMS   int64  `json:"latency_ms,omitempty"`
}

// BatchSpeechResponse represents the envelope format for batch speech results.
type BatchSpeechResponse struct {
	Results        []BatchSpeechItemResponse `json:"results"`
	TotalItems     int                       `json:"total_items"`
	BatchLatencyMS int64                     `json:"batch_latency_ms"`
}

// isSpeechWorker checks whether a connected worker offers speech/TTS capabilities.
func isSpeechWorker(w workerhub.WorkerSnapshot) bool {
	for _, cap := range w.Capabilities {
		if cap == "audio_speech" || cap == "tts" {
			return true
		}
	}
	models := append([]string{w.Model}, w.Models...)
	for _, m := range models {
		mLower := strings.ToLower(m)
		if strings.HasPrefix(mLower, "piper:") || strings.HasPrefix(mLower, "tiktok:") || strings.Contains(mLower, "speech") || strings.Contains(mLower, "tts") {
			return true
		}
	}
	return false
}

// MaxBatchSize calculates maximum batch size based on online worker slots (capacity * 2 or safe ceiling 100).
func (h *Handler) MaxBatchSize() int {
	slots := 0
	if h.Workers != nil {
		for _, w := range h.Workers.Snapshot() {
			if isSpeechWorker(w) {
				s := w.MaxConcurrency
				if s <= 0 {
					s = 1
				}
				slots += s
			}
		}
	}
	// ponytail: Fallback to local TikTok or default slots when no remote workers online. Upgrade when dynamic slot registry lands.
	if slots == 0 {
		if h.TikTokAsyncSlots > 0 {
			slots = h.TikTokAsyncSlots
		} else {
			slots = DefaultFallbackSlots
		}
	}
	maxBatch := slots * 2
	if maxBatch > MaxBatchSizeCeiling {
		maxBatch = MaxBatchSizeCeiling
	}
	if maxBatch < 1 {
		maxBatch = 1
	}
	return maxBatch
}

// BatchAudioSpeech handles POST /v1/audio/speech/batch.
func (h *Handler) BatchAudioSpeech(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if h.Dispatcher == nil {
		writeError(w, http.StatusServiceUnavailable, "dispatcher is unavailable", "server_error", "dispatcher_unavailable")
		return
	}
	started := time.Now()
	var req BatchSpeechRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Inputs == nil && req.Items == nil {
		writeError(w, http.StatusBadRequest, "inputs or items are required", "invalid_request_error", "missing_parameter")
		return
	}
	if (req.Inputs != nil && len(req.Inputs) == 0 && req.Items == nil) ||
		(req.Items != nil && len(req.Items) == 0 && req.Inputs == nil) ||
		(len(req.Inputs) == 0 && len(req.Items) == 0) {
		writeError(w, http.StatusBadRequest, "batch cannot be empty", "invalid_request_error", "empty_batch")
		return
	}

	var items []BatchSpeechItem
	if len(req.Items) > 0 {
		items = make([]BatchSpeechItem, len(req.Items))
		for i, it := range req.Items {
			id := it.ID
			if id == "" {
				id = strconv.Itoa(i)
			}
			model := it.Model
			if model == "" {
				model = req.Model
			}
			voice := it.Voice
			if voice == "" {
				voice = req.Voice
			}
			format := it.ResponseFormat
			if format == "" {
				format = req.ResponseFormat
			}
			speed := it.Speed
			if speed == 0 {
				speed = req.Speed
			}
			items[i] = BatchSpeechItem{
				ID:             id,
				Model:          model,
				Input:          it.Input,
				Voice:          voice,
				ResponseFormat: format,
				Speed:          speed,
			}
		}
	} else if len(req.Inputs) > 0 {
		items = make([]BatchSpeechItem, len(req.Inputs))
		for i, text := range req.Inputs {
			items[i] = BatchSpeechItem{
				ID:             strconv.Itoa(i),
				Model:          req.Model,
				Input:          text,
				Voice:          req.Voice,
				ResponseFormat: req.ResponseFormat,
				Speed:          req.Speed,
			}
		}
	}

	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "batch cannot be empty", "invalid_request_error", "empty_batch")
		return
	}

	maxBatch := h.MaxBatchSize()
	if len(items) > maxBatch {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("batch size %d exceeds max allowed %d", len(items), maxBatch), "invalid_request_error", "batch_size_exceeded")
		return
	}

	for i, it := range items {
		if strings.TrimSpace(it.Model) == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("item %d: model is required", i), "invalid_request_error", "missing_parameter")
			return
		}
		if strings.TrimSpace(it.Input) == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("item %d: input is required", i), "invalid_request_error", "missing_parameter")
			return
		}
	}

	results := make([]BatchSpeechItemResponse, len(items))
	var wg sync.WaitGroup
	var totalAudioBytes int64
	var audioBytesMu sync.Mutex

		for i := range items {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				it := items[idx]
				speechReq := provider.SpeechRequest{
					Model:       it.Model,
					Input:       it.Input,
					Voice:       it.Voice,
					ResponseFmt: it.ResponseFormat,
				}
				itemStart := time.Now()
				data, mime, err := h.Dispatcher.Speech(r.Context(), speechReq)
				itemElapsed := time.Since(itemStart).Milliseconds()
				if err != nil {
					results[idx] = BatchSpeechItemResponse{
						ID:        it.ID,
						Index:     idx,
						Status:    "error",
						Error:     err.Error(),
						LatencyMS: itemElapsed,
					}
					return
				}
					b64 := base64.StdEncoding.EncodeToString(data)
					results[idx] = BatchSpeechItemResponse{
						ID:          it.ID,
						Index:       idx,
						AudioBase64: b64,
						B64JSON:     b64,
						ContentType: mime,
						MIMEType:    mime,
						Mime:        mime,
						Status:      "ok",
						LatencyMS:   itemElapsed,
					}
					audioBytesMu.Lock()
					totalAudioBytes += int64(len(data))
					audioBytesMu.Unlock()
				}(i)
			}

		wg.Wait()

		if h.AuditLog != nil {
			_ = h.AuditLog.Record(r.Context(), admin.AuditEvent{
				RequestID: r.Header.Get("X-Request-ID"),
				EventType: "speech_batch",
				Model:     req.Model,
				Latency:   time.Since(started),
				Detail:    fmt.Sprintf("total=%d", len(items)),
			})
		}
		if h.Traffic != nil && totalAudioBytes > 0 {
			sec := int64(totalAudioBytes / 44100)
			if sec == 0 {
				sec = 1
			}
			go func(s int64) { _ = h.Traffic.RecordMetrics(context.Background(), time.Now(), 0, 0, s) }(sec)
		}

		resp := BatchSpeechResponse{
			Results:        results,
			TotalItems:     len(results),
			BatchLatencyMS: time.Since(started).Milliseconds(),
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("[GATEWAY] failed to encode batch speech response: %v", err)
		}
	}
