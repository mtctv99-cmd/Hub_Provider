package deepseek

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/provider"
	"sever_provider/internal/provider/deepseek/pow"
)

func mockDeepSeekServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case DefaultCreateSessionPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"biz_data": map[string]any{
						"id": "mock-sess-1",
					},
				},
			})
		case DefaultCreatePowPath:
			salt := "salttest"
			expireAt := int64(1800000000)
			prefix := pow.BuildPrefix(salt, expireAt)
			h := pow.DeepSeekHashV1([]byte(prefix + "10"))
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
							"difficulty":  1000,
							"signature":   "sig-test",
							"target_path": DefaultCompletionPath,
						},
					},
				},
			})
		case DefaultCompletionPath:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"v\":\"Đang giải bài toán...</think>\"}\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: {\"v\":\"Đáp án là 100.\"}\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestAdapterNameAndCapabilities(t *testing.T) {
	adapter := New(nil)
	if adapter.Name() != "deepseek_web" {
		t.Fatalf("adapter.Name() = %q, want deepseek_web", adapter.Name())
	}

	caps := adapter.Capabilities()
	hasChat := false
	hasStreaming := false
	hasReasoning := false
	for _, c := range caps {
		if c == provider.CapabilityChat {
			hasChat = true
		}
		if c == provider.CapabilityStreaming {
			hasStreaming = true
		}
		if c == provider.CapabilityReasoning {
			hasReasoning = true
		}
	}
	if !hasChat || !hasStreaming || !hasReasoning {
		t.Fatalf("expected chat, streaming, reasoning in capabilities: %+v", caps)
	}
}

func TestAdapterChatNonStream(t *testing.T) {
	srv := mockDeepSeekServer(t)
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	adapter := New(client)

	secret := []byte(`{"token":"user-token-abc","session_id":"sess-existing"}`)
	req := provider.ChatRequest{
		Model: "deepseek-reasoner",
		Messages: []provider.Message{
			{Role: "user", Content: "Tính 50 + 50"},
		},
	}

	resp, err := adapter.Chat(context.Background(), req, secret)
	if err != nil {
		t.Fatalf("adapter.Chat failed: %v", err)
	}

	if resp.Content != "Đáp án là 100." {
		t.Fatalf("resp.Content = %q, want 'Đáp án là 100.'", resp.Content)
	}
	if resp.ReasoningContent != "Đang giải bài toán..." {
		t.Fatalf("resp.ReasoningContent = %q, want 'Đang giải bài toán...'", resp.ReasoningContent)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("resp.FinishReason = %q, want stop", resp.FinishReason)
	}
}

func TestAdapterStreamChat(t *testing.T) {
	srv := mockDeepSeekServer(t)
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	adapter := New(client)

	secret := []byte("user-token-raw-string")
	req := provider.ChatRequest{
		Model: "deepseek-chat",
		Messages: []provider.Message{
			{Role: "user", Content: "Hello"},
		},
	}

	stream, err := adapter.StreamChat(context.Background(), req, secret)
	if err != nil {
		t.Fatalf("adapter.StreamChat failed: %v", err)
	}

	var content string
	var reasoning string
	done := false

	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		if chunk.Content != "" {
			content += chunk.Content
		}
		if chunk.ReasoningContent != "" {
			reasoning += chunk.ReasoningContent
		}
		if chunk.Done {
			done = true
		}
	}

	if !done {
		t.Fatal("expected done signal")
	}
	if content == "" && reasoning == "" {
		t.Fatal("expected content or reasoning, got empty")
	}
}

func TestAdapterLeaseManagement(t *testing.T) {
	adapter := New(nil)
	if adapter.ActiveLeases() != 0 {
		t.Fatalf("active leases = %d, want 0", adapter.ActiveLeases())
	}

	adapter.AcquireLease()
	if adapter.ActiveLeases() != 1 {
		t.Fatalf("active leases = %d, want 1", adapter.ActiveLeases())
	}

	adapter.ReleaseLease()
	if adapter.ActiveLeases() != 0 {
		t.Fatalf("active leases = %d, want 0", adapter.ActiveLeases())
	}
}

func TestAdapterCircuitBreaker(t *testing.T) {
	cb := NewCircuitBreaker(2, 50*time.Millisecond)
	now := time.Now()

	if !cb.Allow(now) {
		t.Fatal("initial state should be allowed")
	}
	if cb.State() != CircuitClosed {
		t.Fatalf("state = %v, want CLOSED", cb.State())
	}

	cb.RecordFailure(errors.New("fail 1"))
	if !cb.Allow(now) {
		t.Fatal("first failure should still allow")
	}

	cb.RecordFailure(errors.New("fail 2"))
	if cb.State() != CircuitOpen {
		t.Fatalf("state = %v, want OPEN after 2 failures", cb.State())
	}
	if cb.Allow(now) {
		t.Fatal("circuit open should reject immediately")
	}

	// Sau khi hết cooldown
	later := now.Add(100 * time.Millisecond)
	if !cb.Allow(later) {
		t.Fatal("should allow trial request after cooldown")
	}
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("state = %v, want HALF_OPEN", cb.State())
	}

	cb.RecordSuccess()
	if cb.State() != CircuitClosed {
		t.Fatalf("state = %v, want CLOSED after success", cb.State())
	}
}

func TestAdapterInvalidSecret(t *testing.T) {
	adapter := New(nil)
	req := provider.ChatRequest{Model: "deepseek-chat"}

	_, err := adapter.Chat(context.Background(), req, nil)
	if err == nil {
		t.Fatal("expected error for empty secret, got nil")
	}

	_, err = adapter.StreamChat(context.Background(), req, []byte{})
	if err == nil {
		t.Fatal("expected error for empty secret stream, got nil")
	}
}

func mockToolCallServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case DefaultCreateSessionPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"biz_data": map[string]any{"id": "mock-sess-tool"},
				},
			})
		case DefaultCreatePowPath:
			salt := "salttest"
			expireAt := int64(1800000000)
			prefix := pow.BuildPrefix(salt, expireAt)
			h := pow.DeepSeekHashV1([]byte(prefix + "10"))
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
							"difficulty":  1000,
							"signature":   "sig-test",
							"target_path": DefaultCompletionPath,
						},
					},
				},
			})
		case DefaultCompletionPath:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"v\":\"```tool_call\\n{\\\"name\\\": \\\"get_current_weather\\\", \\\"arguments\\\": {\\\"location\\\": \\\"Hà Nội\\\"}}\\n```\"}\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestAdapterChat_ToolCalling(t *testing.T) {
	srv := mockToolCallServer(t)
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	adapter := New(client)

	secret := []byte(`{"token":"user-token-test","session_id":"mock-sess-tool"}`)
	req := provider.ChatRequest{
		Model: "deepseek-chat",
		Messages: []provider.Message{
			{Role: "user", Content: "Thời tiết Hà Nội ra sao?"},
		},
		Tools: []provider.Tool{
			{
				Type: "function",
				Function: &provider.Function{
					Name:        "get_current_weather",
					Description: "Lấy thời tiết hiện tại",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}

	resp, err := adapter.Chat(context.Background(), req, secret)
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want 'tool_calls'", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Function.Name != "get_current_weather" {
		t.Errorf("tc.Function.Name = %q, want 'get_current_weather'", tc.Function.Name)
	}
	if !strings.Contains(tc.Function.Arguments, "Hà Nội") {
		t.Errorf("tc.Function.Arguments = %q, want to contain 'Hà Nội'", tc.Function.Arguments)
	}
	if resp.Content != "" {
		t.Errorf("resp.Content = %q, want empty string", resp.Content)
	}
}

func TestAdapterStreamChat_ToolCalling(t *testing.T) {
	srv := mockToolCallServer(t)
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	adapter := New(client)

	secret := []byte(`{"token":"user-token-test","session_id":"mock-sess-tool"}`)
	req := provider.ChatRequest{
		Model: "deepseek-chat",
		Messages: []provider.Message{
			{Role: "user", Content: "Thời tiết Hà Nội ra sao?"},
		},
		Tools: []provider.Tool{
			{
				Type: "function",
				Function: &provider.Function{
					Name: "get_current_weather",
				},
			},
		},
	}

	stream, err := adapter.StreamChat(context.Background(), req, secret)
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	var chunks []provider.StreamChunk
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("chunk err: %v", chunk.Err)
		}
		chunks = append(chunks, chunk)
	}

	if len(chunks) == 0 {
		t.Fatal("expected chunks from stream")
	}

	// Kiểm tra chunk cuối cùng phải có ToolCalls và FinishReason = "tool_calls"
	last := chunks[len(chunks)-1]
	if !last.Done {
		t.Error("last chunk should have Done = true")
	}
	if last.FinishReason != "tool_calls" {
		t.Errorf("last chunk FinishReason = %q, want 'tool_calls'", last.FinishReason)
	}
	if len(last.ToolCalls) != 1 {
		t.Fatalf("last chunk len(ToolCalls) = %d, want 1", len(last.ToolCalls))
	}
	if last.ToolCalls[0].Function.Name != "get_current_weather" {
		t.Errorf("function name = %q, want 'get_current_weather'", last.ToolCalls[0].Function.Name)
	}

	// Đảm bảo không chunk nào làm rò rỉ cú pháp ```tool_call ra Content
	for i, c := range chunks {
		if strings.Contains(c.Content, "```tool_call") {
			t.Errorf("chunk %d leaked ```tool_call into Content: %q", i, c.Content)
		}
	}
}
