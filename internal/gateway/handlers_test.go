package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/deepseek"
	"sever_provider/internal/provider/deepseek/pow"
	"sever_provider/internal/scheduler"
)

type mockAdapter struct{}

func (mockAdapter) Name() string { return "mock" }
func (mockAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilitySpeech}
}
func (mockAdapter) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	return provider.ChatResponse{ID: "mock-1", Content: "hello"}, nil
}
func (mockAdapter) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 2)
	ch <- provider.StreamChunk{Content: "hello"}
	ch <- provider.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}
func (mockAdapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return []byte("RIFF-test"), "audio/wav", nil
}
func (mockAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, nil
}

type mockSource struct{}

func (mockSource) List(context.Context, string) ([]identity.Credential, error) {
	return []identity.Credential{{ID: "c", Domain: "mock", Status: identity.StatusHealthy, MaxConcurrency: 2}}, nil
}
func (mockSource) ReadSecret(context.Context, string) ([]byte, error) { return nil, nil }

func testHandler() *Handler {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "mock-chat", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}, TimeoutSec: 10})
	_ = models.Register(scheduler.ModelSpec{ID: "mock-tts", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilitySpeech}, TimeoutSec: 10})
	_ = models.Register(scheduler.ModelSpec{ID: "piper:ngochuyennew", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilitySpeech}, TimeoutSec: 10})
	reg := provider.NewRegistry()
	reg.Register(mockAdapter{})
	leases := scheduler.NewLeaseManager()
	d := &scheduler.Dispatcher{Models: models, Credentials: mockSource{}, Adapters: reg, Leases: leases}
	return NewHandler(d, models)
}

func TestGatewayHealthModelsAndChat(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()
	r, err := http.Get(srv.URL + "/health")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("health: %v %d", err, r.StatusCode)
	}
	r, err = http.Get(srv.URL + "/v1/models")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("models: %v %d", err, r.StatusCode)
	}
	body := strings.NewReader(`{"model":"mock-chat","messages":[{"role":"user","content":"hi"}]}`)
	r, err = http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("chat: %v %d", err, r.StatusCode)
	}
}

func TestGatewayRejectsOversizedBody(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(NewRouterWithLimits(h, "", 32))
	defer srv.Close()
	body := strings.NewReader(`{"model":"mock-chat","messages":[{"role":"user","content":"this body is larger than the configured limit"}]}`)
	r, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413", r.StatusCode)
	}
}

func TestGatewayMasksProviderErrors(t *testing.T) {
	h := testHandler()
	h.Dispatcher.Adapters = failingRegistry{err: errors.New("secret upstream cookie token details")}
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()
	body := strings.NewReader(`{"model":"mock-chat","messages":[{"role":"user","content":"hi"}]}`)
	r, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r.Body)
	if strings.Contains(string(data), "secret upstream") {
		t.Fatalf("raw provider error leaked: %s", data)
	}
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", r.StatusCode)
	}
}

type failingRegistry struct{ err error }

func (f failingRegistry) Get(string) (provider.Adapter, bool) {
	return failingAdapter{err: f.err}, true
}

type failingAdapter struct{ err error }

func (f failingAdapter) Name() string { return "mock" }
func (f failingAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat}
}
func (f failingAdapter) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, f.err
}
func (f failingAdapter) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	return nil, f.err
}
func (f failingAdapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", f.err
}
func (f failingAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, f.err
}

func TestGatewaySSE(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()
	body := strings.NewReader(`{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	r, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("stream: %v %d", err, r.StatusCode)
	}
}

func TestGatewayReadinessProbe(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()
	r, err := http.Get(srv.URL + "/ready")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("ready: %v %d", err, r.StatusCode)
	}
}

func TestGatewayLegacyTTSSynthesize(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()
	body := strings.NewReader(`{"text":"Xin chào","voice_id":"ngochuyennew"}`)
	r, err := http.Post(srv.URL+"/api/tts/synthesize", "application/json", body)
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("legacy tts: %v %d", err, r.StatusCode)
	}
	if r.Header.Get("Content-Type") != "audio/wav" {
		t.Errorf("expected audio/wav MIME, got %s", r.Header.Get("Content-Type"))
	}
}

func TestGatewayAnthropicStream(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()
	body := strings.NewReader(`{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	r, err := http.Post(srv.URL+"/v1/messages", "application/json", body)
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("anthropic stream: %v %d", err, r.StatusCode)
	}
	data, _ := io.ReadAll(r.Body)
	raw := string(data)
	if !strings.Contains(raw, "event: message_start\ndata: ") || !strings.Contains(raw, "content_block_delta") {
		t.Fatalf("unexpected anthropic stream response: %s", raw)
	}
	if strings.Contains(raw, "\\ndata:") {
		t.Fatalf("escaped newline found in SSE stream: %s", raw)
	}
}

func TestAnthropicRejectsUnsupportedImages(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()
	body := strings.NewReader(`{"model":"mock-chat","messages":[{"role":"user","content_parts":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	r, err := http.Post(srv.URL+"/v1/messages", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", r.StatusCode)
	}
		data, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(data), "unsupported_capability") {
			t.Fatalf("unexpected error body: %s", data)
		}
	}

type deepseekMockAdapter struct{}

func (deepseekMockAdapter) Name() string { return "deepseek_web" }
func (deepseekMockAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityReasoning}
}
func (deepseekMockAdapter) Chat(ctx context.Context, req provider.ChatRequest, secret []byte) (provider.ChatResponse, error) {
	if req.Model == "deepseek-reasoner" {
		return provider.ChatResponse{
			ID:               "ds-reasoner-1",
			Model:            req.Model,
			Content:          "Final answer: 42",
			ReasoningContent: "Step 1: calculate 40 + 2",
			FinishReason:     "stop",
		}, nil
	}
	return provider.ChatResponse{
		ID:           "ds-chat-1",
		Model:        req.Model,
		Content:      "Hello from DeepSeek V3",
		FinishReason: "stop",
	}, nil
}
func (deepseekMockAdapter) StreamChat(ctx context.Context, req provider.ChatRequest, secret []byte) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 4)
	go func() {
		defer close(ch)
		if req.Model == "deepseek-reasoner" {
			ch <- provider.StreamChunk{ReasoningContent: "Thinking step 1"}
		}
		ch <- provider.StreamChunk{Content: "Streamed answer"}
		ch <- provider.StreamChunk{Done: true, FinishReason: "stop"}
	}()
	return ch, nil
}
func (deepseekMockAdapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (deepseekMockAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}

func TestGatewayDeepSeekDispatchChatAndMessages(t *testing.T) {
	models := scheduler.NewModelRegistry()
	reg := provider.NewRegistry()
	reg.Register(deepseekMockAdapter{})
	leases := scheduler.NewLeaseManager()
	d := &scheduler.Dispatcher{Models: models, Credentials: mockSource{}, Adapters: reg, Leases: leases}
	h := NewHandler(d, models)
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()

	// 1. POST /v1/chat/completions with deepseek-chat
	{
		body := strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
		r, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("deepseek-chat completion failed: %v %d", err, r.StatusCode)
		}
		var resp ChatCompletionResponse
		_ = json.NewDecoder(r.Body).Decode(&resp)
		if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "Hello from DeepSeek V3" {
			t.Fatalf("unexpected deepseek-chat response: %+v", resp)
		}
	}

	// 2. POST /v1/chat/completions with deepseek-reasoner (reasoning_content check)
	{
		body := strings.NewReader(`{"model":"deepseek-reasoner","messages":[{"role":"user","content":"solve this"}]}`)
		r, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("deepseek-reasoner completion failed: %v %d", err, r.StatusCode)
		}
		var resp ChatCompletionResponse
		_ = json.NewDecoder(r.Body).Decode(&resp)
		if len(resp.Choices) == 0 {
			t.Fatal("expected choices in deepseek-reasoner response")
		}
		choice := resp.Choices[0]
		if choice.Message.Content != "Final answer: 42" {
			t.Errorf("content = %q, want 'Final answer: 42'", choice.Message.Content)
		}
		if choice.Message.ReasoningContent != "Step 1: calculate 40 + 2" {
			t.Errorf("reasoning_content = %q, want 'Step 1: calculate 40 + 2'", choice.Message.ReasoningContent)
		}
	}

	// 3. POST /v1/messages with deepseek-reasoner (Claude thinking block check)
	{
		body := strings.NewReader(`{"model":"deepseek-reasoner","messages":[{"role":"user","content":"solve this"}]}`)
		r, err := http.Post(srv.URL+"/v1/messages", "application/json", body)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("deepseek-reasoner anthropic message failed: %v %d", err, r.StatusCode)
		}
		var resp struct {
			Content []map[string]any `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&resp)
		if len(resp.Content) != 2 {
			t.Fatalf("expected 2 content blocks (thinking and text), got %d: %+v", len(resp.Content), resp.Content)
		}
		if resp.Content[0]["type"] != "thinking" || resp.Content[0]["thinking"] != "Step 1: calculate 40 + 2" {
			t.Errorf("unexpected thinking block: %+v", resp.Content[0])
		}
		if resp.Content[1]["type"] != "text" || resp.Content[1]["text"] != "Final answer: 42" {
			t.Errorf("unexpected text block: %+v", resp.Content[1])
		}
	}

	// 4. POST /v1/chat/completions with stream=true and deepseek-reasoner
	{
		body := strings.NewReader(`{"model":"deepseek-reasoner","messages":[{"role":"user","content":"stream please"}],"stream":true}`)
		r, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("deepseek stream completion failed: %v %d", err, r.StatusCode)
		}
		data, _ := io.ReadAll(r.Body)
		raw := string(data)
		if !strings.Contains(raw, "reasoning_content") || !strings.Contains(raw, "Thinking step 1") {
			t.Errorf("expected reasoning_content in stream chunk: %s", raw)
		}
		if !strings.Contains(raw, "Streamed answer") {
			t.Errorf("expected content in stream chunk: %s", raw)
		}
	}

	// 5. POST /v1/messages with stream=true and deepseek-reasoner
	{
		body := strings.NewReader(`{"model":"deepseek-reasoner","messages":[{"role":"user","content":"stream please"}],"stream":true}`)
		r, err := http.Post(srv.URL+"/v1/messages", "application/json", body)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("deepseek anthropic stream failed: %v %d", err, r.StatusCode)
		}
		data, _ := io.ReadAll(r.Body)
		raw := string(data)
		if !strings.Contains(raw, "thinking_delta") || !strings.Contains(raw, "Thinking step 1") {
			t.Errorf("expected thinking_delta in anthropic stream: %s", raw)
		}
			if !strings.Contains(raw, "text_delta") || !strings.Contains(raw, "Streamed answer") {
				t.Errorf("expected text_delta in anthropic stream: %s", raw)
			}
		}
	}

type mockDeepSeekSource struct{}

func (mockDeepSeekSource) List(context.Context, string) ([]identity.Credential, error) {
	return []identity.Credential{{ID: "ds-cred-1", Domain: "deepseek_web", Status: identity.StatusHealthy, MaxConcurrency: 2}}, nil
}
func (mockDeepSeekSource) ReadSecret(context.Context, string) ([]byte, error) {
	return []byte(`{"token":"user-token-test"}`), nil
}

func TestGatewayDeepSeekRealAdapterIntegration(t *testing.T) {
	dsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case deepseek.DefaultCreateSessionPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"biz_data": map[string]any{"id": "sess-real-1"},
				},
			})
		case deepseek.DefaultCreatePowPath:
			salt := "salttest"
			expireAt := int64(1800000000)
			prefix := pow.BuildPrefix(salt, expireAt)
			h := pow.DeepSeekHashV1([]byte(prefix + "0"))
			target := hex.EncodeToString(h[:])

			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"biz_data": map[string]any{
						"challenge": map[string]any{
							"algorithm":   "DeepSeekHashV1",
							"challenge":   target,
							"salt":        salt,
							"expire_at":   expireAt,
							"difficulty":  100,
							"signature":   "sig-test",
							"target_path": deepseek.DefaultCompletionPath,
						},
					},
				},
			})
		case deepseek.DefaultCompletionPath:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"v\":\"Hello from real DeepSeek adapter!\"}\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	defer dsSrv.Close()

	client := deepseek.NewClient(dsSrv.URL, dsSrv.Client())
	adapter := deepseek.New(client)

	models := scheduler.NewModelRegistry()
	reg := provider.NewRegistry()
	reg.Register(adapter)
	leases := scheduler.NewLeaseManager()
	d := &scheduler.Dispatcher{Models: models, Credentials: mockDeepSeekSource{}, Adapters: reg, Leases: leases}
	h := NewHandler(d, models)
	gwSrv := httptest.NewServer(NewRouter(h, ""))
	defer gwSrv.Close()

	// Call /v1/chat/completions with deepseek-chat
	body := strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	r, err := http.Post(gwSrv.URL+"/v1/chat/completions", "application/json", body)
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("real adapter completion failed: %v %d", err, r.StatusCode)
	}
	var resp ChatCompletionResponse
	_ = json.NewDecoder(r.Body).Decode(&resp)
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "Hello from real DeepSeek adapter!" {
		t.Fatalf("unexpected completion response: %+v", resp)
	}
}
