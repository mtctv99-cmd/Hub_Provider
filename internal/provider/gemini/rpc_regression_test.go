package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/provider"
)

func tokenHandlerRPC(t *testing.T, rpc http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"SNlM0e":"x","cfb2h":"test","FdrFJe":"f"}`))
	})
	mux.HandleFunc("/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate", rpc)
	return httptest.NewServer(mux)
}

func TestChatEmpty200Fails(t *testing.T) {
	srv := tokenHandlerRPC(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(")]}'\n[]\n"))
	})
	defer srv.Close()
	a := New(&http.Client{})
	a.SetBaseURL(srv.URL)
	if _, err := a.Chat(context.Background(), provider.ChatRequest{Model: "gemini-3.8-flash"}, nil); err == nil {
		t.Fatal("expected error")
	} else if !errors.Is(err, provider.ErrEmptyResponse) && !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err=%v", err)
	}
}

func TestGeminiDiagnoseEmptyResponseDistinction(t *testing.T) {
	// Case 1: upstream empty (heartbeat only or blank)
	err1 := DiagnoseEmptyResponse(")]}'\n\n")
	if !errors.Is(err1, provider.ErrEmptyResponse) {
		t.Fatalf("expected ErrEmptyResponse, got: %v", err1)
	}
	reason1, ok1 := provider.EmptyResponseReasonOf(err1)
	if !ok1 || reason1 != provider.EmptyReasonUpstreamEmpty {
		t.Fatalf("expected upstream_empty, got: %q", reason1)
	}

	// Case 2: payload has wrb.fr frames but parser could not extract text -> parser_failed
	rawWithFrames := `)]}'` + "\n" + `[["wrb.fr",null,"[1,2,3]"]]` + "\n"
	err2 := DiagnoseEmptyResponse(rawWithFrames)
	if !errors.Is(err2, provider.ErrEmptyResponse) {
		t.Fatalf("expected ErrEmptyResponse, got: %v", err2)
	}
	reason2, ok2 := provider.EmptyResponseReasonOf(err2)
	if !ok2 || reason2 != provider.EmptyReasonParserFailed {
		t.Fatalf("expected parser_failed, got: %q", reason2)
	}

	// Case 3: ensure no sensitive keywords in diagnostics
	if strings.Contains(err1.Error(), "cookie") || strings.Contains(err2.Error(), "cookie") {
		t.Fatal("diagnostics leaked cookie keyword")
	}
}

func TestChatErrorIn200Fails(t *testing.T) {
	srv := tokenHandlerRPC(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(")]}'\n[[\"er.fr\",null,[\"blocked\"]]]\n"))
	})
	defer srv.Close()
	a := New(&http.Client{})
	a.SetBaseURL(srv.URL)
	if _, err := a.Chat(context.Background(), provider.ChatRequest{Model: "gemini-3.8-flash"}, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestStreamCutMidwayErrors(t *testing.T) {
	line := func(text string) []byte {
		inner := []any{"x", []any{"c_1", "r_1"}, nil, nil, []any{[]any{"rc_1", []any{text}}}}
		j, _ := json.Marshal(inner)
		l, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(j)}})
		return append(l, '\n')
	}
	full := append(line("hello"), line("hello world")...)
	srv := tokenHandlerRPC(t, func(w http.ResponseWriter, r *http.Request) {
		// Cut mid-frame: write only half of a valid response, no trailing newline.
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write(full[:len(full)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	defer srv.Close()
	a := New(&http.Client{})
	a.SetBaseURL(srv.URL)
	ch, err := a.StreamChat(context.Background(), provider.ChatRequest{Model: "gemini-3.8-flash"}, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	var done, failed bool
	for c := range ch {
		if c.Err != nil {
			failed = true
		}
		if c.Done {
			done = true
		}
	}
	if done || !failed {
		t.Fatalf("done=%v failed=%v", done, failed)
	}
}

func TestStreamContextCancelErrors(t *testing.T) {
	line := func(text string) []byte {
		inner := []any{"x", []any{"c_1", "r_1"}, nil, nil, []any{[]any{"rc_1", []any{text}}}}
		j, _ := json.Marshal(inner)
		l, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(j)}})
		return append(l, '\n')
	}
	srv := tokenHandlerRPC(t, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		for i := 0; i < 50; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			_, _ = w.Write(line("chunk"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	defer srv.Close()
	a := New(&http.Client{})
	a.SetBaseURL(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := a.StreamChat(ctx, provider.ChatRequest{Model: "gemini-3.8-flash"}, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	cancel()
	var failed bool
	for c := range ch {
		if c.Err != nil {
			failed = true
		}
		if c.Done {
			t.Fatal("canceled stream must not emit Done")
		}
	}
	if !failed {
		t.Fatal("canceled stream must emit Err")
	}
}

func TestMetadataOnlyFromStructuredFrames(t *testing.T) {
	got := ExtractConversationMetadata("text c_fake r_x rc_y tail")
	if got != (ConversationMetadata{}) {
		t.Fatalf("meta=%+v", got)
	}
}

func TestSanitizeCookieFailFast(t *testing.T) {
	a := New(&http.Client{})
	badSecret := []byte(`{"cookie":"broken-cookie-without-session"}`)
	if _, err := a.Chat(context.Background(), provider.ChatRequest{Model: "gemini-3.8-flash"}, badSecret); err == nil {
		t.Fatal("Chat must fail-fast on invalid cookie")
	} else if !strings.Contains(err.Error(), "gemini cookie:") {
		t.Fatalf("unexpected err: %v", err)
	}

	if _, err := a.StreamChat(context.Background(), provider.ChatRequest{Model: "gemini-3.8-flash"}, badSecret); err == nil {
		t.Fatal("StreamChat must fail-fast on invalid cookie")
	} else if !strings.Contains(err.Error(), "gemini cookie:") {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestStreamDeadlineExplicit(t *testing.T) {
	c1 := &http.Client{Timeout: 45 * 1000000000}
	if d := streamDeadline(c1); d != 45*1000000000 {
		t.Fatalf("expected 45s, got %v", d)
	}
	c2 := &http.Client{}
	if d := streamDeadline(c2); d != 120*1000000000 {
		t.Fatalf("expected default 120s, got %v", d)
	}
}

func TestStreamChat_ToolCallsBufferedAndIndexed(t *testing.T) {
	line := func(text string) []byte {
		inner := []any{"x", []any{"c_1", "r_1"}, nil, nil, []any{[]any{"rc_1", []any{text}}}}
		j, _ := json.Marshal(inner)
		l, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(j)}})
		return append(l, '\n')
	}
	responseBody := "```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Hanoi\"}}\n```"
	srv := tokenHandlerRPC(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(line(responseBody[:15]))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write(line(responseBody))
	})
	defer srv.Close()

	a := New(&http.Client{})
	a.SetBaseURL(srv.URL)
	req := provider.ChatRequest{
		Model: "gemini-3.8-flash",
		Tools: []provider.Tool{
			{Type: "function", Function: &provider.Function{Name: "get_weather"}},
		},
		ToolChoice: "auto",
	}
	ch, err := a.StreamChat(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	var chunks []provider.StreamChunk
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("unexpected chunk err: %v", c.Err)
		}
		chunks = append(chunks, c)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	// In tool-call mode, intermediate tool_call markdown must NOT be streamed to content
	for _, c := range chunks {
		if strings.Contains(c.Content, "tool_call") {
			t.Fatalf("streamed chunk leaked raw tool_call fence: %q", c.Content)
		}
	}
	last := chunks[len(chunks)-1]
	if !last.Done {
		t.Fatal("last chunk must be Done")
	}
	if len(last.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(last.ToolCalls))
	}
	tc := last.ToolCalls[0]
	if tc.Index == nil || *tc.Index != 0 {
		t.Fatalf("expected tool call index 0, got %v", tc.Index)
	}
	if tc.Function.Name != "get_weather" {
		t.Fatalf("expected get_weather, got %s", tc.Function.Name)
	}
}

func TestStreamChat_ToolChoiceRequiredFailsWhenNoCalls(t *testing.T) {
	line := func(text string) []byte {
		inner := []any{"x", []any{"c_1", "r_1"}, nil, nil, []any{[]any{"rc_1", []any{text}}}}
		j, _ := json.Marshal(inner)
		l, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(j)}})
		return append(l, '\n')
	}
	srv := tokenHandlerRPC(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(line("Plain text answer without tool call."))
	})
	defer srv.Close()

	a := New(&http.Client{})
	a.SetBaseURL(srv.URL)
	req := provider.ChatRequest{
		Model: "gemini-3.8-flash",
		Tools: []provider.Tool{
			{Type: "function", Function: &provider.Function{Name: "get_weather"}},
		},
		ToolChoice: "required",
	}
	ch, err := a.StreamChat(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	var errFound bool
	for c := range ch {
		if c.Err != nil {
			errFound = true
		}
	}
	if !errFound {
		t.Fatal("expected error when required tool choice emitted no tool call")
	}
}
