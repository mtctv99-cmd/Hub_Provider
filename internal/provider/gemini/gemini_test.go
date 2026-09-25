package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/provider"
)

func TestSapisidHashFormat(t *testing.T) {
	got := SapisidHash("secret", time.Unix(1700000000, 0))
	if !strings.HasPrefix(got, "SAPISIDHASH 1700000000_") {
		t.Fatalf("unexpected hash: %s", got)
	}
}

func TestPayloadHasFReqAnd69Slots(t *testing.T) {
	body := BuildPayload("hello", "gemini-3.8-flash", "xsrf", "uuid", ConversationMetadata{}, nil)
	if !strings.Contains(body, "f.req=") || !strings.Contains(body, "at=xsrf") {
		t.Fatalf("payload missing required fields: %s", body)
	}
}

func TestFormatPromptPreservesMultiTurnContext(t *testing.T) {
	prompt := formatPrompt([]provider.Message{
		{Role: "user", Content: "What is 2+2?"},
		{Role: "assistant", Content: "4"},
		{Role: "user", Content: "And 3+3?"},
	})
	if !strings.Contains(prompt, "User: What is 2+2?") || !strings.Contains(prompt, "Model: 4") || !strings.HasSuffix(prompt, "And 3+3?") {
		t.Fatalf("unexpected multi-turn prompt: %q", prompt)
	}
}

func TestSanitizeCookie(t *testing.T) {
	cookie, err := SanitizeCookie("Cookie: __Secure-1PSID=session; SAPISID=sapisid\r\n")
	if err != nil {
		t.Fatalf("sanitize failed: %v", err)
	}
	if cookie != "__Secure-1PSID=session; SAPISID=sapisid" {
		t.Fatalf("cookie=%q", cookie)
	}
	if _, err := SanitizeCookie("SAPISID=only"); err == nil {
		t.Fatal("expected missing session cookie error")
	}
}

func TestParserExtractsTextAndMetadata(t *testing.T) {
	inner := []any{"ignored", []any{"c_123", "r_456"}, nil, nil, []any{[]any{"rc_789", []any{"hello", " world"}}}}
	innerJSON, _ := json.Marshal(inner)
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
	text := ExtractResponseText(string(outer))
	if text != "hello world" {
		t.Fatalf("text=%q", text)
	}
	meta := ExtractConversationMetadata(string(outer))
	if meta.ConversationID != "c_123" || meta.ResponseID != "r_456" || meta.ChoiceID != "rc_789" {
		t.Fatalf("meta=%+v", meta)
	}
}

func TestGenerateUsesInjectedRPCServer(t *testing.T) {
	inner := []any{"ignored", []any{"c_1", "r_1"}, nil, nil, []any{[]any{"rc_1", []any{"ok"}}}}
	innerJSON, _ := json.Marshal(inner)
	fixture, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"SNlM0e":"x","cfb2h":"test","FdrFJe":"f"}`))
			return
		}
		w.Write(fixture)
	}))
	defer server.Close()
	client := &http.Client{}
	a := New(client)
	a.SetBaseURL(server.URL)
	text, meta, err := a.Generate(context.Background(), "hello", "model", "", ConversationMetadata{})
	if err != nil {
		t.Fatalf("guest-compatible Generate failed: %v", err)
	}
	if text != "ok" || meta.ConversationID != "c_1" || meta.ResponseID != "r_1" || meta.ChoiceID != "rc_1" {
		t.Fatalf("unexpected response text=%q metadata=%+v", text, meta)
	}
}

func TestStreamChatUsesGuestEndpointAndReturnsMetadata(t *testing.T) {
	makeLine := func(text string) []byte {
		inner := []any{"ignored", []any{"c_stream", "r_stream"}, nil, nil, []any{[]any{"rc_stream", []any{text}}}}
		innerJSON, _ := json.Marshal(inner)
		line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
		return line
	}
	first := makeLine("hello")
	second := makeLine("hello world")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"SNlM0e":"xsrf","cfb2h":"test","FdrFJe":"f"}`))
			return
		}
		_, _ = w.Write(append(append(first, '\n'), append(second, '\n')...))
	}))
	defer server.Close()
	a := New(&http.Client{})
	a.SetBaseURL(server.URL)
	stream, err := a.StreamChat(context.Background(), provider.ChatRequest{Model: "gemini-3.5-flash-lite", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}
	var content string
	var done provider.StreamChunk
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		content += chunk.Content
		if chunk.Done {
			done = chunk
		}
	}
	if content != "hello world" {
		t.Fatalf("content=%q", content)
	}
	if done.Upstream.ConversationID != "c_stream" || done.Upstream.ResponseID != "r_stream" || done.Upstream.ChoiceID != "rc_stream" {
		t.Fatalf("upstream=%+v", done.Upstream)
	}
}
