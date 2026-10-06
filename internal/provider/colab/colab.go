package colab

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/provider"
)

type Config struct {
	URL       string
	WorkerID  string
	APIKey    string
	Model     string
	Heartbeat time.Duration
}

type Adapter struct {
	cfg Config
	mu  sync.Mutex
}

func New(cfg Config) *Adapter {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.Model == "" {
		cfg.Model = "gemma-4-colab"
	}
	return &Adapter{cfg: cfg}
}
func (a *Adapter) Name() string { return "google_colab" }
func (a *Adapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityVision}
}
func (a *Adapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (a *Adapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}

func (a *Adapter) connect(ctx context.Context) (*websocket.Conn, error) {
	u, err := url.Parse(a.cfg.URL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("worker_id", a.cfg.WorkerID)
	q.Set("token", a.cfg.APIKey)
	u.RawQuery = q.Encode()
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	register := map[string]any{"type": "register", "worker_id": a.cfg.WorkerID, "model": a.cfg.Model}
	if err := conn.WriteJSON(register); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (a *Adapter) Chat(ctx context.Context, req provider.ChatRequest, _ []byte) (provider.ChatResponse, error) {
	text := ""
	if len(req.Messages) > 0 {
		text = req.Messages[len(req.Messages)-1].Content
	}
	conn, err := a.connect(ctx)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	defer conn.Close()
	requestID := fmt.Sprintf("req_%d", time.Now().UnixNano())
	msg := map[string]any{"type": "chat_request", "request_id": requestID, "stream": false, "messages": req.Messages, "temperature": req.Temperature, "max_tokens": req.MaxTokens, "top_p": nil, "text": text}
	if err := conn.WriteJSON(msg); err != nil {
		return provider.ChatResponse{}, err
	}
	for {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			return provider.ChatResponse{}, err
		}
		if event["type"] == "ping" {
			_ = conn.WriteJSON(map[string]any{"type": "pong"})
			continue
		}
		if fmt.Sprint(event["request_id"]) != requestID {
			continue
		}
		switch event["type"] {
		case "job_result":
			raw, _ := event["text"].(string)
			if strings.TrimSpace(fmt.Sprint(event["text"])) == "" {
				if raw == "" {
					return provider.ChatResponse{}, provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "colab worker job_result had no text")
				}
				return provider.ChatResponse{}, provider.NewEmptyResponseError(provider.EmptyReasonParserFailed, fmt.Sprintf("colab worker job_result text (len=%d) yielded empty content", len(raw)))
			}
			return provider.ChatResponse{ID: requestID, Model: req.Model, Content: fmt.Sprint(event["text"]), Usage: parseUsage(event["usage"])}, nil
		case "error":
			return provider.ChatResponse{}, errors.New(fmt.Sprint(event["error"]))
		}
	}
}

func (a *Adapter) StreamChat(ctx context.Context, req provider.ChatRequest, _ []byte) (<-chan provider.StreamChunk, error) {
	conn, err := a.connect(ctx)
	if err != nil {
		return nil, err
	}
	requestID := fmt.Sprintf("req_%d", time.Now().UnixNano())
	if err := conn.WriteJSON(map[string]any{"type": "chat_request", "request_id": requestID, "stream": true, "messages": req.Messages, "temperature": req.Temperature, "max_tokens": req.MaxTokens, "top_p": nil}); err != nil {
		conn.Close()
		return nil, err
	}
	out := make(chan provider.StreamChunk, 32)
	go func() {
		defer close(out)
		defer conn.Close()
		var emitted strings.Builder
		for {
			var event map[string]any
			if err := conn.ReadJSON(&event); err != nil {
				out <- provider.StreamChunk{Err: err}
				return
			}
			if event["type"] == "ping" {
				_ = conn.WriteJSON(map[string]any{"type": "pong"})
				continue
			}
			if fmt.Sprint(event["request_id"]) != requestID {
				continue
			}
			switch event["type"] {
			case "token":
				delta := fmt.Sprint(event["delta"])
				emitted.WriteString(delta)
				out <- provider.StreamChunk{Content: delta}
			case "token_end":
				if strings.TrimSpace(emitted.String()) == "" {
					out <- provider.StreamChunk{Err: provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "colab worker stream ended without text deltas")}
					return
				}
				out <- provider.StreamChunk{Done: true, Usage: usagePtr(event["usage"])}
				return
			case "job_result":
				rawText := fmt.Sprint(event["text"])
				emitted.WriteString(rawText)
				if strings.TrimSpace(emitted.String()) == "" {
					out <- provider.StreamChunk{Err: provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "colab worker stream job_result had no text")}
					return
				}
				out <- provider.StreamChunk{Content: rawText, Done: true, Usage: usagePtr(event["usage"])}
				return
			case "error":
				out <- provider.StreamChunk{Err: errors.New(fmt.Sprint(event["error"]))}
				return
			}
		}
	}()
	return out, nil
}

func parseUsage(v any) provider.Usage {
	var u provider.Usage
	if m, ok := v.(map[string]any); ok {
		u.PromptTokens = intNumber(m["prompt_tokens"])
		u.CompletionTokens = intNumber(m["completion_tokens"])
		u.TotalTokens = intNumber(m["total_tokens"])
	}
	return u
}
func usagePtr(v any) *provider.Usage { u := parseUsage(v); return &u }
func intNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}
