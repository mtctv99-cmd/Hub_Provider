package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestWebModelCatalogMatchesUpstreamPicker(t *testing.T) {
	want := map[string]struct {
		hex   string
		mode  int
		image bool
	}{
		"gemini-4-hydrogen":     {"56fdd199312815e2", 1, true},
		"gemini-3.8-flash":      {"56fdd199312815e2", 1, true},
		"gemini-3.6-flash":      {"56fdd199312815e2", 1, true},
		"gemini-3.5-flash-lite": {"8c46e95b1a07cecc", 6, false},
		"gemini-3.1-pro":        {"e6fa609c3fa255c0", 3, false},
		"gemini-4-carbon":       {"797f3d0293f288ad", 3, false},
		"gemini-4-argon":        {"95a4c0abcba24364", 8, false},
	}
	for id, expect := range want {
		got, ok := webModels[id]
		if !ok {
			t.Fatalf("%s missing from webModels", id)
		}
		if got.hex != expect.hex || got.mode != expect.mode || got.image != expect.image {
			t.Fatalf("%s = %+v, want %+v", id, got, expect)
		}
	}
}

func TestArgonPayloadAndThinkingEffort(t *testing.T) {
	for _, tc := range []struct {
		effort string
		want   int
	}{{"low", 1}, {"medium", 1}, {"high", 2}} {
		header, err := chatRPCHeader("gemini-4-argon", tc.effort)
		if err != nil {
			t.Fatalf("chatRPCHeader(%q): %v", tc.effort, err)
		}
		var headerFields []any
		if err := json.Unmarshal([]byte(header), &headerFields); err != nil {
			t.Fatal(err)
		}
		if headerFields[4] != "95a4c0abcba24364" || headerFields[14] != float64(8) || headerFields[15] != float64(tc.want) {
			t.Fatalf("header for %s = %s", tc.effort, header)
		}

		body := BuildPayloadWithThinking("hello", "gemini-4-argon", "", "uuid", ConversationMetadata{}, nil, tc.effort)
		form, err := url.ParseQuery(body)
		if err != nil {
			t.Fatal(err)
		}
		var outer []any
		if err := json.Unmarshal([]byte(form.Get("f.req")), &outer); err != nil {
			t.Fatal(err)
		}
		var inner []any
		if err := json.Unmarshal([]byte(outer[1].(string)), &inner); err != nil {
			t.Fatal(err)
		}
		if len(inner) != geminiPayloadSlots || inner[3] != "gemini-4-argon" || inner[79] != float64(8) || inner[80] != float64(tc.want) {
			t.Fatalf("payload for %s has unexpected model/thinking slots", tc.effort)
		}
	}
	if _, err := chatRPCHeader("gemini-4-argon", "maximum"); err == nil {
		t.Fatal("expected unsupported thinking effort to be rejected")
	}
}

func TestReasoningEffortModelSuffixes(t *testing.T) {
	for _, tc := range []struct {
		base   string
		effort string
		think  float64
	}{
		{"gemini-3.8-flash", "low", 1},
		{"gemini-3.1-pro", "medium", 1},
		{"gemini-4-argon", "high", 2},
	} {
		alias := tc.base + "-" + tc.effort
		header, err := chatRPCHeader(alias, "")
		if err != nil {
			t.Fatalf("chatRPCHeader(%q): %v", alias, err)
		}
		var headerFields []any
		if err := json.Unmarshal([]byte(header), &headerFields); err != nil {
			t.Fatal(err)
		}
		if headerFields[4] != webModels[tc.base].hex || headerFields[15] != tc.think {
			t.Fatalf("suffix alias %q mapped to wrong upstream configuration: %s", alias, header)
		}
		form, err := url.ParseQuery(BuildPayloadWithThinking("hello", alias, "", "uuid", ConversationMetadata{}, nil, ""))
		if err != nil {
			t.Fatal(err)
		}
		var outer []any
		if err := json.Unmarshal([]byte(form.Get("f.req")), &outer); err != nil {
			t.Fatal(err)
		}
		var inner []any
		if err := json.Unmarshal([]byte(outer[1].(string)), &inner); err != nil {
			t.Fatal(err)
		}
		if inner[3] != tc.base || inner[80] != tc.think {
			t.Fatalf("payload suffix alias %q mapped incorrectly: model=%v effort=%v", alias, inner[3], inner[80])
		}
	}
	if _, err := chatRPCHeader("gemini-3.8-flash-low", "high"); err == nil {
		t.Fatal("conflicting explicit effort should be rejected")
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

func TestParserStripsGroundingCitations(t *testing.T) {
	inner := []any{"ignored", []any{"c_1", "r_1"}, nil, nil, []any{[]any{"rc_1", []any{"Mèo con màu cam", "[cite: 1]", " trên chăn.", "[cite: 2, 3]"}}}}
	innerJSON, _ := json.Marshal(inner)
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
	if text := ExtractResponseText(string(outer)); text != "Mèo con màu cam trên chăn." {
		t.Fatalf("citations leaked: %q", text)
	}
}

func TestCleanTextStripsFollowUpSuggestions(t *testing.T) {
	raw := "Your name is Probe123.\n\n<FollowUp label=\"Want me to remember something else?\">Remember</FollowUp>"
	if got := cleanText(raw); got != "Your name is Probe123." {
		t.Fatalf("follow-up block leaked: %q", got)
	}
}

func TestCleanTextKeepsProseAfterToolCodeBlocks(t *testing.T) {
	// Shape observed live: code-interpreter fences come first, the answer after.
	raw := "\n\n```python?code_reference&code_event_index=1\n" +
		"revenue = 3 * 48 * 12\nprint(revenue)\n" +
		"```\n\n```text?code_stdout&code_event_index=1\nRevenue: 29,808,000 VND\n" +
		"```\n\n**Tổng doanh thu: 29.808.000 đồng**\n\n* Tổng số gói: 1728"
	got := cleanText(raw)
	if strings.Contains(got, "?code_") || strings.Contains(got, "print(revenue)") {
		t.Fatalf("tool fences leaked: %q", got)
	}
	if !strings.Contains(got, "29.808.000") || !strings.Contains(got, "Tổng số gói") {
		t.Fatalf("answer prose lost: %q", got)
	}

	// A normal fenced code block must survive untouched.
	plain := "Đây là code:\n```python\nprint('hi')\n```\nXong."
	if out := cleanText(plain); !strings.Contains(out, "print('hi')") {
		t.Fatalf("normal code fence removed: %q", out)
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
	text, meta, err := a.Generate(context.Background(), "hello", "gemini-3.8-flash", "", ConversationMetadata{})
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

// thinkingFrame builds one synthetic wrb.fr snapshot: text in candidate[1] and,
// when non-empty, the thinking chain in candidate[37][0][0]. The slot shape comes
// from the documented protocol, not from a captured upstream frame.
func thinkingFrame(text, thoughts string) []byte {
	candidate := make([]any, 38)
	candidate[0] = "rc_1"
	candidate[1] = []any{text}
	if thoughts != "" {
		candidate[37] = []any{[]any{thoughts}}
	}
	inner := []any{"ignored", []any{"c_1", "r_1"}, nil, nil, []any{candidate}}
	innerJSON, _ := json.Marshal(inner)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
	return line
}

func TestExtractResponseThoughtsFromThinkingSlot(t *testing.T) {
	raw := string(thinkingFrame("Đáp án", "Bước 1"))
	if got := ExtractResponseThoughts(raw); got != "Bước 1" {
		t.Fatalf("thoughts=%q", got)
	}
	if got := ExtractResponseText(raw); got != "Đáp án" {
		t.Fatalf("text=%q", got)
	}
	if text, meta, ok := ExtractResponseLine(raw); !ok || text != "Đáp án" || meta.ChoiceID != "rc_1" {
		t.Fatalf("line ok=%v text=%q meta=%+v", ok, text, meta)
	}
}

func TestCandidateThoughtsGuards(t *testing.T) {
	cases := map[string][]any{
		"frame too short":  make([]any, 3),
		"empty outer":      withSlot(37, []any{}),
		"nil outer":        withSlot(37, nil),
		"string outer":     withSlot(37, "str"),
		"empty inner":      withSlot(37, []any{[]any{}}),
		"scalar inner":     withSlot(37, []any{"x"}),
		"non-string value": withSlot(37, []any{[]any{42}}),
	}
	for name, candidate := range cases {
		if got := candidateThoughts(candidate); got != "" {
			t.Fatalf("%s: thoughts=%q", name, got)
		}
	}
}

func withSlot(index int, value any) []any {
	candidate := make([]any, 38)
	candidate[index] = value
	return candidate
}

func TestThinkingFrameWithoutAnswerTextStillYieldsThoughts(t *testing.T) {
	candidate := withSlot(37, []any{[]any{"Suy nghĩ"}})
	inner := []any{"ignored", []any{"c_1", "r_1"}, nil, nil, []any{candidate}}
	innerJSON, _ := json.Marshal(inner)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
	if got := ExtractResponseThoughts(string(line)); got != "Suy nghĩ" {
		t.Fatalf("thoughts=%q", got)
	}
	if text, _, ok := ExtractResponseLine(string(line)); ok || text != "" {
		t.Fatalf("text=%q ok=%v", text, ok)
	}
}

func TestExtractResponseThoughtsFollowsTextFrame(t *testing.T) {
	// Older turn (empty thoughts) plus the answering frame: reasoning must come
	// from the frame that produced the longest text.
	raw := string(thinkingFrame("ngắn", "")) + "\n" + string(thinkingFrame("câu trả lời dài hơn", "chuỗi suy nghĩ"))
	if got := ExtractResponseThoughts(raw); got != "chuỗi suy nghĩ" {
		t.Fatalf("thoughts=%q", got)
	}
}
