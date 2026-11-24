package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/provider"
	"sever_provider/internal/workerhub"
)

func maskColabError(err error) (int, string, string) {
	switch {
	case errors.Is(err, workerhub.ErrNoWorker), errors.Is(err, workerhub.ErrHubClosed), errors.Is(err, workerhub.ErrWorkerDisconnected):
		return http.StatusServiceUnavailable, "Colab worker unavailable", "worker_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "Colab worker timed out", "upstream_timeout"
	default:
		return http.StatusBadGateway, "Colab worker failed to complete request", "provider_unavailable"
	}
}

func (h *Handler) streamColab(w http.ResponseWriter, ctx context.Context, req provider.ChatRequest) {
	startedAt := time.Now()
	ttftTimer := time.Now()
	var ttft time.Duration
	firstToken := true
	outcome := "error"
	errKind := "provider_unavailable"
	detail := "worker stream failed"
	defer func() {
		if h.AuditLog == nil {
			return
		}
		_ = h.AuditLog.Record(ctx, admin.AuditEvent{
			RequestID: w.Header().Get("X-Request-ID"),
			EventType: "chat_completion_stream",
			Model:     req.Model,
			Latency:   time.Since(startedAt),
			TTFT:      ttft,
			Outcome:   outcome,
			ErrorKind: errKind,
			Detail:    detail,
		})
	}()
	if h.Workers == nil {
		writeError(w, http.StatusServiceUnavailable, "Colab worker unavailable", "provider_error", "worker_unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported", "server_error", "streaming_unsupported")
		return
	}
	stream, err := h.Workers.StreamChat(ctx, req)
	if err != nil {
		log.Printf("[GATEWAY] colab streamChat failed: %v", err)
		status, msg, code := maskColabError(err)
		writeError(w, status, msg, "provider_error", code)
		return
	}
	setSSEHeaders(w)
	id := "chatcmpl-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	created := time.Now().Unix()
	send := func(v any) { data, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", data); flusher.Flush() }
	completed := false
	var streamContent strings.Builder
	var streamReasoning strings.Builder
	var streamToolCalls []provider.ToolCall
	var streamUsage *provider.Usage
	var paced time.Duration
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				errKind = "upstream_timeout"
				detail = "Colab worker timed out"
				send(ErrorEnvelope{Error: APIError{Message: "Colab worker timed out", Type: "provider_error", Code: "upstream_timeout"}})
			} else {
				outcome = "cancelled"
				errKind = "request_cancelled"
				detail = "request cancelled"
			}
			return
		case chunk, open := <-stream:
			if !open {
				if !completed {
					errKind = "provider_unavailable"
					detail = "Colab worker failed to complete request"
					send(ErrorEnvelope{Error: APIError{Message: "Colab worker failed to complete request", Type: "provider_error", Code: "provider_unavailable"}})
					return
				}
				outcome = "success"
				errKind = ""
				detail = "success"
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			if chunk.Err != nil {
				log.Printf("[GATEWAY] colab chunk error: %v", chunk.Err)
				_, msg, code := maskColabError(chunk.Err)
				errKind = code
				detail = msg
				send(ErrorEnvelope{Error: APIError{Message: msg, Type: "provider_error", Code: code}})
				return
			}
			finish := (*string)(nil)
			if chunk.FinishReason != "" {
				finish = &chunk.FinishReason
			} else if chunk.Done {
				value := "stop"
				finish = &value
				completed = true
			}
			streamContent.WriteString(chunk.Content)
			streamReasoning.WriteString(chunk.ReasoningContent)
			if firstToken && (chunk.Content != "" || chunk.ReasoningContent != "" || len(chunk.ToolCalls) > 0) {
				ttft = time.Since(ttftTimer)
				firstToken = false
			}
			if len(chunk.ToolCalls) > 0 {
				for i := range chunk.ToolCalls {
					if chunk.ToolCalls[i].Index == nil {
						idx := len(streamToolCalls) + i
						chunk.ToolCalls[i].Index = &idx
					}
				}
				streamToolCalls = append(streamToolCalls, chunk.ToolCalls...)
			}
			if chunk.Usage != nil {
				streamUsage = chunk.Usage
			}
			if chunk.Done && strings.TrimSpace(streamContent.String()) == "" && strings.TrimSpace(streamReasoning.String()) == "" && len(streamToolCalls) == 0 {
				// Defensive guard: Hub already converts empty streams, but an
				// empty Done must surface empty_response instead of [DONE].
				_, msg, code := maskProviderError(provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "colab worker stream completed without text content or tool calls"))
				errKind = code
				detail = msg
				send(ErrorEnvelope{Error: APIError{Message: msg, Type: "provider_error", Code: code}})
				return
			}
			var finalUsage provider.Usage
			if chunk.Usage != nil {
				finalUsage = *chunk.Usage
			}
			if chunk.Done {
				toks := provider.ComputeUsage(req.Messages, req.Tools, streamContent.String(), streamReasoning.String(), streamToolCalls, streamUsage)
				h.AITokens.Add(uint64(toks.TotalTokens))
				if h.Traffic != nil && toks.TotalTokens > 0 {
					go func(tok int) { _ = h.Traffic.RecordMetrics(context.Background(), time.Now(), 0, int64(tok), 0) }(toks.TotalTokens)
				}
				if finalUsage == (provider.Usage{}) {
					finalUsage = toks
				}
			}
			parts := splitStreamText(chunk.Content, 8)
			if len(parts) == 0 {
				parts = []string{""}
			}
			// The worker releases tool calls whole; split arguments into fragments
			// and attach the call once, not to every text fragment.
			tcDeltas := splitToolCallDeltas(chunk.ToolCalls, 48)
			if len(tcDeltas) > 0 && chunk.Content == "" && chunk.ReasoningContent == "" {
				parts = nil
			}
			for i, part := range parts {
				delta := &provider.Message{Role: "assistant", Content: part, ReasoningContent: chunk.ReasoningContent}
				partFinish := (*string)(nil)
				if i == len(parts)-1 && len(tcDeltas) == 0 {
					partFinish = finish
				}
				send(ChatCompletionResponse{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model, Choices: []Choice{{Index: 0, Delta: delta, FinishReason: partFinish}}, Usage: finalUsage})
				if i < len(parts)-1 {
					if !paceStream(ctx, &paced) {
						return
					}
				}
			}
			for i, tc := range tcDeltas {
				delta := &provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{tc}}
				partFinish := (*string)(nil)
				if i == len(tcDeltas)-1 {
					partFinish = finish
				}
				send(ChatCompletionResponse{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model, Choices: []Choice{{Index: 0, Delta: delta, FinishReason: partFinish}}, Usage: finalUsage})
			}
			if chunk.Done {
				outcome = "success"
				errKind = ""
				detail = "success"
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
		}
	}
}
