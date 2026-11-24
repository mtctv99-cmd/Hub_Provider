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
	"time"

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
func (mockAdapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, nil
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

// longStreamAdapter trả một chunk 1200 rune để đo trần pacing.
type longStreamAdapter struct{ mockAdapter }

func (longStreamAdapter) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 2)
	ch <- provider.StreamChunk{Content: strings.Repeat("a", 1200)}
	ch <- provider.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func TestStreamPacingBudgetCapsDelay(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "mock-chat", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}, TimeoutSec: 10})
	reg := provider.NewRegistry()
	reg.Register(longStreamAdapter{})
	d := &scheduler.Dispatcher{Models: models, Credentials: mockSource{}, Adapters: reg, Leases: scheduler.NewLeaseManager()}
	h := NewHandler(d, models)
	body := strings.NewReader(`{"model":"mock-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	start := time.Now()
	NewRouter(h, "").ServeHTTP(rr, r)
	elapsed := time.Since(start)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var content strings.Builder
	finish := ""
	for _, line := range strings.Split(rr.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(line) == "data: [DONE]" {
			continue
		}
		var chunk ChatCompletionResponse
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("bad SSE line %q: %v", line, err)
		}
		for _, c := range chunk.Choices {
			if c.Delta != nil {
				content.WriteString(c.Delta.Content)
			}
			if c.FinishReason != nil {
				finish = *c.FinishReason
			}
		}
	}
	if content.String() != strings.Repeat("a", 1200) {
		t.Fatalf("stream changed content: got %d runes", content.Len())
	}
	if finish != "stop" {
		t.Fatalf("finish_reason=%q, want stop", finish)
	}
	if elapsed > 700*time.Millisecond {
		t.Fatalf("stream pacing not capped: %v", elapsed)
	}
}

func TestSplitStreamText(t *testing.T) {
	if got := splitStreamText("short answer", 8); len(got) != 1 || got[0] != "short answer" {
		t.Fatalf("short text=%q", got)
	}
	text := "Dưới đây là đoạn văn dài hơn đủ để chia nhỏ thành những phần dễ đọc."
	parts := splitStreamText(text, 8)
	if len(parts) < 3 {
		t.Fatalf("expected chunks for long text, got %q", parts)
	}
	if strings.Join(parts, "") != text {
		t.Fatalf("split changed text: %q != %q", strings.Join(parts, ""), text)
	}
}

func TestSSEHeadersDisableProxyBufferingAndCompression(t *testing.T) {
	rr := httptest.NewRecorder()
	setSSEHeaders(rr)
	h := rr.Header()
	if got := h.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type=%q", got)
	}
	if got := h.Get("Cache-Control"); !strings.Contains(got, "no-transform") || !strings.Contains(got, "no-cache") {
		t.Fatalf("Cache-Control=%q", got)
	}
	if got := h.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering=%q", got)
	}
	if got := h.Get("Content-Encoding"); got != "identity" {
		t.Fatalf("Content-Encoding=%q", got)
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
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", r.StatusCode)
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
func (f failingAdapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, f.err
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
func (deepseekMockAdapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

func TestGatewayDeepSeekDispatchChatAndMessages(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.DefaultDeepSeekChatModelSpec())
	_ = models.Register(scheduler.DefaultDeepSeekReasonerModelSpec())
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
	_ = models.Register(scheduler.DefaultDeepSeekChatModelSpec())
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

type emptyMockAdapter struct{}

func (emptyMockAdapter) Name() string { return "empty_mock" }
func (emptyMockAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}
}
func (emptyMockAdapter) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	return provider.ChatResponse{ID: "mock-empty", Content: ""}, nil
}
func (emptyMockAdapter) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}
func (emptyMockAdapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (emptyMockAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}
func (emptyMockAdapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

func TestGatewayEmptyResponseHandledCorrectly(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{
		ID:           "empty-model",
		Provider:     "empty_mock",
		Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming},
		TimeoutSec:   5,
	})
	reg := provider.NewRegistry()
	reg.Register(emptyMockAdapter{})
	leases := scheduler.NewLeaseManager()
	d := &scheduler.Dispatcher{
		Models:      models,
		Credentials: mockSource{},
		Adapters:    reg,
		Leases:      leases,
	}
	h := NewHandler(d, models)
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()

	// 1. Non-streaming empty response
	{
		body := strings.NewReader(`{"model":"empty-model","messages":[{"role":"user","content":"hi"}]}`)
		r, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusBadGateway {
			t.Fatalf("expected 502 Bad Gateway for empty response, got %d", r.StatusCode)
		}
		var env ErrorEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Fatalf("decode error envelope failed: %v", err)
		}
		if env.Error.Code != "empty_response" {
			t.Fatalf("expected error code empty_response, got %q", env.Error.Code)
		}
	}

	// 2. Streaming empty response
	{
		body := strings.NewReader(`{"model":"empty-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
		r, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", body)
		if err != nil {
			t.Fatalf("stream request failed: %v", err)
		}
		defer r.Body.Close()
		data, _ := io.ReadAll(r.Body)
		raw := string(data)
		if strings.Contains(raw, "data: [DONE]") {
			t.Fatalf("expected stream to NOT contain [DONE] on empty response, got: %s", raw)
		}
		if !strings.Contains(raw, "empty_response") {
			t.Fatalf("expected stream to contain empty_response error, got: %s", raw)
		}
	}
}

func TestOutputLimiter(t *testing.T) {
	// A stop sequence split across chunks must be detected, not leaked.
	l := newOutputLimiter([]string{"green"}, 0)
	var got strings.Builder
	for _, s := range []string{"red, gr", "een, blue"} {
		got.WriteString(l.feed(s))
	}
	got.WriteString(l.flush())
	if strings.Contains(got.String(), "green") {
		t.Fatalf("stop sequence leaked: %q", got.String())
	}
	if strings.TrimSpace(got.String()) != "red," {
		t.Fatalf("unexpected output before stop: %q", got.String())
	}
	if !l.stopped || l.truncated {
		t.Fatalf("expected stopped=true truncated=false, got stopped=%v truncated=%v", l.stopped, l.truncated)
	}

	// max_tokens is approximated as 4 runes per token.
	l = newOutputLimiter(nil, 2)
	if out := l.feed("abcdefghij"); out != "abcdefgh" || !l.truncated {
		t.Fatalf("max_tokens not enforced: %q truncated=%v", out, l.truncated)
	}

	// No limit: a held tail is released by flush.
	l = newOutputLimiter([]string{"xx"}, 0)
	got.Reset()
	got.WriteString(l.feed("hello"))
	got.WriteString(l.flush())
	if got.String() != "hello" {
		t.Fatalf("flush lost text: %q", got.String())
	}
}

func TestChatRejectsUnsupportedParameters(t *testing.T) {
	h := testHandler()
	rejected := []struct{ param, body string }{
		{"logprobs", `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"logprobs":true}`},
		{"top_logprobs", `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"top_logprobs":3}`},
		{"parallel_tool_calls", `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":false}`},
	}
	for _, tc := range rejected {
		r := postChat(t, h, tc.body)
		if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "unsupported_parameter") || !strings.Contains(r.Body.String(), tc.param) {
			t.Fatalf("%s: status=%d body=%s", tc.param, r.Code, r.Body.String())
		}
	}

	// OpenAI's "off" values mean "not requested" and must keep working.
	allowed := []string{
		`{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"logprobs":false}`,
		`{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"top_logprobs":0}`,
		`{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":true}`,
		`{"model":"mock-chat","messages":[{"role":"user","content":"hi"}]}`,
	}
	for _, body := range allowed {
		if r := postChat(t, h, body); r.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s request=%s", r.Code, r.Body.String(), body)
		}
	}
}

func TestStopSequencesAreBounded(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()

	post := func(path, body string) (int, string) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	tooMany := `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"stop":["a","b","c","d","e"]}`
	if status, body := post("/v1/chat/completions", tooMany); status != http.StatusBadRequest || !strings.Contains(body, "invalid_stop") {
		t.Fatalf("too many stop: status=%d body=%s", status, body)
	}
	tooLong := `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"stop":["` + strings.Repeat("x", 257) + `"]}`
	if status, body := post("/v1/chat/completions", tooLong); status != http.StatusBadRequest || !strings.Contains(body, "invalid_stop") {
		t.Fatalf("long stop: status=%d body=%s", status, body)
	}
	okBody := `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"stop":["a","b","c","d"]}`
	if status, body := post("/v1/chat/completions", okBody); status != http.StatusOK {
		t.Fatalf("valid stop: status=%d body=%s", status, body)
	}

	anthropicMany := `{"model":"mock-chat","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stop_sequences":["a","b","c","d","e"]}`
	if status, body := post("/v1/messages", anthropicMany); status != http.StatusBadRequest || !strings.Contains(body, "invalid_stop") {
		t.Fatalf("anthropic too many stop: status=%d body=%s", status, body)
	}
}
