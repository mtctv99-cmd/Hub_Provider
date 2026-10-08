package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

type mockZeroUsageAdapter struct{}

func (mockZeroUsageAdapter) Name() string { return "zero-usage" }
func (mockZeroUsageAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}
}
func (mockZeroUsageAdapter) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	return provider.ChatResponse{
		ID:      "test-1",
		Content: "This is a detailed test response with multiple words to check token counting.",
		Usage:   provider.Usage{}, // empty usage, simulates web reverse-engineered endpoints
	}, nil
}
func (mockZeroUsageAdapter) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 4)
	ch <- provider.StreamChunk{Content: "Hello "}
	ch <- provider.StreamChunk{Content: "streaming "}
	ch <- provider.StreamChunk{Content: "world!"}
	ch <- provider.StreamChunk{Done: true, FinishReason: "stop"}
	close(ch)
	return ch, nil
}
func (mockZeroUsageAdapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (mockZeroUsageAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}
func (mockZeroUsageAdapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

func TestGlobalAITokensCalculation(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{
		ID:           "test-gemini-web",
		Provider:     "zero-usage",
		Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming},
		TimeoutSec:   10,
	})
	reg := provider.NewRegistry()
	reg.Register(mockZeroUsageAdapter{})
	dispatcher := &scheduler.Dispatcher{
		Models:      models,
		Credentials: mockSource{},
		Adapters:    reg,
		Leases:      scheduler.NewLeaseManager(),
	}
	h := NewHandler(dispatcher, models)
	router := NewRouter(h, "")
	server := httptest.NewServer(router)
	defer server.Close()

	initialTokens := h.AITokens.Load()
	if initialTokens != 0 {
		t.Fatalf("expected 0 initial tokens, got %d", initialTokens)
	}

	// 1. Non-streaming OpenAI format chat completion
	reqBody := `{"model":"test-gemini-web","messages":[{"role":"user","content":"Explain quantum computing in simple terms"}]}`
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("chat request failed: %v, status: %d", err, resp.StatusCode)
	}
	var chatRes ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatRes); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if chatRes.Usage.TotalTokens <= 0 {
		t.Fatalf("expected estimated tokens > 0, got %d", chatRes.Usage.TotalTokens)
	}
	tokAfter1 := h.AITokens.Load()
	if tokAfter1 != uint64(chatRes.Usage.TotalTokens) {
		t.Fatalf("h.AITokens (%d) != chatRes.Usage.TotalTokens (%d)", tokAfter1, chatRes.Usage.TotalTokens)
	}

	// 2. Streaming OpenAI format chat completion
	streamReqBody := `{"model":"test-gemini-web","stream":true,"messages":[{"role":"user","content":"Stream to me"}]}`
	sResp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamReqBody))
	if err != nil || sResp.StatusCode != 200 {
		t.Fatalf("stream chat request failed: %v, status: %d", err, sResp.StatusCode)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(sResp.Body)
	_ = sResp.Body.Close()

	tokAfter2 := h.AITokens.Load()
	if tokAfter2 <= tokAfter1 {
		t.Fatalf("streaming chat did not increase h.AITokens: before=%d, after=%d", tokAfter1, tokAfter2)
	}

	// 3. Anthropic format non-streaming message
	anthropicReq := `{"model":"test-gemini-web","messages":[{"role":"user","content":"Hello Anthropic"}]}`
	aResp, err := http.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(anthropicReq))
	if err != nil || aResp.StatusCode != 200 {
		t.Fatalf("anthropic request failed: %v, status: %d", err, aResp.StatusCode)
	}
	var aRes map[string]any
	_ = json.NewDecoder(aResp.Body).Decode(&aRes)
	_ = aResp.Body.Close()

	tokAfter3 := h.AITokens.Load()
	if tokAfter3 <= tokAfter2 {
		t.Fatalf("anthropic message did not increase h.AITokens: before=%d, after=%d", tokAfter2, tokAfter3)
	}

	// 4. Anthropic format streaming message
	anthropicStreamReq := `{"model":"test-gemini-web","stream":true,"messages":[{"role":"user","content":"Hello stream"}]}`
	asResp, err := http.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(anthropicStreamReq))
	if err != nil || asResp.StatusCode != 200 {
		t.Fatalf("anthropic stream request failed: %v, status: %d", err, asResp.StatusCode)
	}
	buf.Reset()
	_, _ = buf.ReadFrom(asResp.Body)
	_ = asResp.Body.Close()

	tokAfter4 := h.AITokens.Load()
	if tokAfter4 <= tokAfter3 {
		t.Fatalf("anthropic stream did not increase h.AITokens: before=%d, after=%d", tokAfter3, tokAfter4)
	}

	t.Logf("Total AI tokens accurately tracked across all chat formats: %d", tokAfter4)
}
