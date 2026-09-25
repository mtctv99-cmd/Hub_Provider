package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

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
		status, msg, code := maskColabError(err)
		writeError(w, status, msg, "provider_error", code)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	id := "chatcmpl-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	created := time.Now().Unix()
	send := func(v any) { data, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", data); flusher.Flush() }
	completed := false
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, open := <-stream:
			if !open {
				if !completed {
					send(ErrorEnvelope{Error: APIError{Message: "Colab worker failed to complete request", Type: "provider_error", Code: "provider_unavailable"}})
					return
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			if chunk.Err != nil {
				_, msg, code := maskColabError(chunk.Err)
				send(ErrorEnvelope{Error: APIError{Message: msg, Type: "provider_error", Code: code}})
				return
			}
			finish := (*string)(nil)
			if chunk.Done {
				value := "stop"
				finish = &value
				completed = true
			}
			send(ChatCompletionResponse{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model, Choices: []Choice{{Index: 0, Delta: &provider.Message{Role: "assistant", Content: chunk.Content}, FinishReason: finish}}, Usage: usageOrEmpty(chunk.Usage)})
		}
	}
}
