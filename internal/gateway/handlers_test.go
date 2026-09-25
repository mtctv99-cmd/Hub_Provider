package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
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
	if !strings.Contains(string(data), "message_start") || !strings.Contains(string(data), "content_block_delta") {
		t.Fatalf("unexpected anthropic stream response: %s", data)
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
