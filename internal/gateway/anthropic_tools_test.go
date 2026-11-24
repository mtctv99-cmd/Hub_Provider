package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

// recordingAdapter captures the provider request the gateway built and replays a
// canned chat/stream response.
type recordingAdapter struct {
	mu     sync.Mutex
	last   provider.ChatRequest
	chat   provider.ChatResponse
	chunks []provider.StreamChunk
}

func (a *recordingAdapter) Name() string { return "rec_mock" }
func (a *recordingAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}
}
func (a *recordingAdapter) Chat(_ context.Context, req provider.ChatRequest, _ []byte) (provider.ChatResponse, error) {
	a.record(req)
	return a.chat, nil
}
func (a *recordingAdapter) StreamChat(_ context.Context, req provider.ChatRequest, _ []byte) (<-chan provider.StreamChunk, error) {
	a.record(req)
	ch := make(chan provider.StreamChunk, len(a.chunks))
	go func() {
		defer close(ch)
		for _, chunk := range a.chunks {
			ch <- chunk
		}
	}()
	return ch, nil
}
func (a *recordingAdapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (a *recordingAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}
func (a *recordingAdapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

func (a *recordingAdapter) record(req provider.ChatRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.last = req
}

func (a *recordingAdapter) request() provider.ChatRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

func newRecordingGateway(t *testing.T, adapter *recordingAdapter) *httptest.Server {
	t.Helper()
	models := scheduler.NewModelRegistry()
	if err := models.Register(scheduler.ModelSpec{ID: "rec-chat", Provider: "rec_mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}, TimeoutSec: 10}); err != nil {
		t.Fatalf("register model: %v", err)
	}
	reg := provider.NewRegistry()
	reg.Register(adapter)
	d := &scheduler.Dispatcher{Models: models, Credentials: mockSource{}, Adapters: reg, Leases: scheduler.NewLeaseManager()}
	srv := httptest.NewServer(NewRouter(NewHandler(d, models), ""))
	t.Cleanup(srv.Close)
	return srv
}

func postAnthropic(t *testing.T, srv *httptest.Server, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// normaliseMessages makes an empty decode compare equal to a nil expectation.
func normaliseMessages(in anthropicMessages) []provider.Message {
	if len(in) == 0 {
		return nil
	}
	return []provider.Message(in)
}

func TestAnthropicContentBlockMapping(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []provider.Message
	}{
		{
			name: "plain string content",
			body: `[{"role":"user","content":"xin chào"}]`,
			want: []provider.Message{{Role: "user", Content: "xin chào"}},
		},
		{
			name: "assistant text and tool_use",
			body: `[{"role":"assistant","content":[{"type":"text","text":"Để tôi kiểm tra."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Hanoi"}}]}]`,
			want: []provider.Message{{
				Role:    "assistant",
				Content: "Để tôi kiểm tra.",
				ToolCalls: []provider.ToolCall{{
					ID: "toolu_1", Type: "function",
					Function: provider.FunctionCallDesc{Name: "get_weather", Arguments: `{"city":"Hanoi"}`},
				}},
			}},
		},
		{
			name: "tool_use without input",
			body: `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"ping"}]}]`,
			want: []provider.Message{{
				Role: "assistant",
				ToolCalls: []provider.ToolCall{{
					ID: "toolu_2", Type: "function",
					Function: provider.FunctionCallDesc{Name: "ping", Arguments: "{}"},
				}},
			}},
		},
		{
			name: "user tool_result string",
			body: `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"31C sunny"}]}]`,
			want: []provider.Message{{Role: "tool", ToolCallID: "toolu_1", Content: "31C sunny"}},
		},
		{
			name: "user tool_result text blocks",
			body: `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}]`,
			want: []provider.Message{{Role: "tool", ToolCallID: "toolu_1", Content: "a\nb"}},
		},
		{
			name: "tool_result before text keeps order",
			body: `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"x"},{"type":"text","text":"cảm ơn"}]}]`,
			want: []provider.Message{
				{Role: "tool", ToolCallID: "toolu_1", Content: "x"},
				{Role: "user", Content: "cảm ơn"},
			},
		},
		{
			name: "thinking block dropped",
			body: `[{"role":"assistant","content":[{"type":"thinking","thinking":"bí mật"},{"type":"text","text":"ok"}]}]`,
			want: []provider.Message{{Role: "assistant", Content: "ok"}},
		},
		{
			name: "null content is skipped",
			body: `[{"role":"user","content":null}]`,
			want: nil,
		},
		{
			name: "content_parts regression",
			body: `[{"role":"user","content_parts":[{"type":"text","text":"hi"}]}]`,
			want: []provider.Message{{Role: "user", Parts: []provider.ContentPart{{Type: "text", Text: "hi"}}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got anthropicMessages
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(normaliseMessages(got), tc.want) {
				t.Fatalf("messages=%+v, want %+v", normaliseMessages(got), tc.want)
			}
		})
	}
}

func TestAnthropicToolsAndChoiceMapping(t *testing.T) {
	tools := anthropicToolsToProvider([]AnthropicTool{
		{Name: "get_weather", Description: "thời tiết", InputSchema: map[string]any{"type": "object"}},
		{Name: "   "},
	})
	if len(tools) != 1 || tools[0].Type != "function" || tools[0].Function == nil ||
		tools[0].Function.Name != "get_weather" || tools[0].Function.Description != "thời tiết" ||
		tools[0].Function.Parameters["type"] != "object" {
		t.Fatalf("tools=%+v", tools)
	}
	if anthropicToolsToProvider([]AnthropicTool{{Name: ""}}) != nil {
		t.Fatal("expected nil when every tool name is empty")
	}

	choices := []struct {
		name string
		in   *AnthropicToolChoice
		want any
	}{
		{"nil", nil, nil},
		{"auto", &AnthropicToolChoice{Type: "auto"}, nil},
		{"any", &AnthropicToolChoice{Type: "any"}, "required"},
		{"none", &AnthropicToolChoice{Type: "none"}, "none"},
		{"tool", &AnthropicToolChoice{Type: "tool", Name: "get_weather"},
			map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}},
	}
	for _, tc := range choices {
		if got := anthropicToolChoiceToProvider(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: choice=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestToolInputObject(t *testing.T) {
	cases := map[string]map[string]any{
		`{"city": "Hanoi"}`: {"city": "Hanoi"},
		`[1,2]`:             {},
		`not json`:          {},
		``:                  {},
		`null`:              {},
	}
	for in, want := range cases {
		if got := toolInputObject(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("toolInputObject(%q)=%v, want %v", in, got, want)
		}
	}
}

func TestAnthropicNonStreamToolUse(t *testing.T) {
	adapter := &recordingAdapter{chat: provider.ChatResponse{
		ID: "rec-1",
		ToolCalls: []provider.ToolCall{{
			ID: "toolu_1", Type: "function",
			Function: provider.FunctionCallDesc{Name: "get_weather", Arguments: `{"city":"Hanoi"}`},
		}},
	}}
	srv := newRecordingGateway(t, adapter)
	status, raw := postAnthropic(t, srv, `{"model":"rec-chat","messages":[{"role":"user","content":"thời tiết Hà Nội?"}],"tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	var resp struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason=%q body=%s", resp.StopReason, raw)
	}
	if len(resp.Content) != 1 || resp.Content[0]["type"] != "tool_use" || resp.Content[0]["id"] != "toolu_1" || resp.Content[0]["name"] != "get_weather" {
		t.Fatalf("content=%+v", resp.Content)
	}
	input, _ := resp.Content[0]["input"].(map[string]any)
	if input["city"] != "Hanoi" {
		t.Fatalf("input=%v", resp.Content[0]["input"])
	}
	if _, ok := resp.Usage["input_tokens"]; !ok {
		t.Fatalf("usage=%v, want Anthropic input_tokens/output_tokens", resp.Usage)
	}
	if _, ok := resp.Usage["output_tokens"]; !ok {
		t.Fatalf("usage=%v, want Anthropic input_tokens/output_tokens", resp.Usage)
	}

	// The tools must reach the adapter and the prompt formatter.
	req := adapter.request()
	if len(req.Tools) != 1 || req.Tools[0].Function == nil || req.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("adapter tools=%+v", req.Tools)
	}
	if req.ToolChoice != nil {
		t.Fatalf("tool_choice=%v, want nil for auto", req.ToolChoice)
	}
	prompt := provider.FormatPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	if !strings.Contains(prompt, "```tool_call") || !strings.Contains(prompt, "get_weather") {
		t.Fatalf("tool definitions missing from prompt: %s", prompt)
	}
}

func TestAnthropicNonStreamToolResultReachesPrompt(t *testing.T) {
	adapter := &recordingAdapter{chat: provider.ChatResponse{ID: "rec-2", Content: "31°C"}}
	srv := newRecordingGateway(t, adapter)
	body := `{"model":"rec-chat","messages":[
		{"role":"assistant","content":[{"type":"text","text":"kiểm tra đã"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Hanoi"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"31C sunny"}]},
		{"role":"user","content":"tóm tắt"}]}`
	if status, raw := postAnthropic(t, srv, body); status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	req := adapter.request()
	prompt := provider.FormatPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	if !strings.Contains(prompt, "[Tool Result (toolu_1)]: 31C sunny") {
		t.Fatalf("tool_result missing from prompt: %s", prompt)
	}
	if !strings.Contains(prompt, "```tool_call") {
		t.Fatalf("assistant tool call missing from prompt: %s", prompt)
	}
}

func TestAnthropicNonStreamTextThenToolUse(t *testing.T) {
	adapter := &recordingAdapter{chat: provider.ChatResponse{
		ID:      "rec-3",
		Content: "Đã kiểm tra.",
		ToolCalls: []provider.ToolCall{{
			ID: "toolu_9", Type: "function",
			Function: provider.FunctionCallDesc{Name: "get_weather", Arguments: `{"city":"Hue"}`},
		}},
	}}
	srv := newRecordingGateway(t, adapter)
	status, raw := postAnthropic(t, srv, `{"model":"rec-chat","messages":[{"role":"user","content":"thời tiết?"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	var resp struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopReason != "tool_use" || len(resp.Content) != 2 {
		t.Fatalf("stop_reason=%q content=%+v", resp.StopReason, resp.Content)
	}
	if resp.Content[0]["type"] != "text" || resp.Content[0]["text"] != "Đã kiểm tra." {
		t.Fatalf("first block=%+v", resp.Content[0])
	}
	if resp.Content[1]["type"] != "tool_use" || resp.Content[1]["name"] != "get_weather" {
		t.Fatalf("second block=%+v", resp.Content[1])
	}
}

func TestAnthropicTruncationWinsOverToolUse(t *testing.T) {
	adapter := &recordingAdapter{chat: provider.ChatResponse{
		ID:      "rec-4",
		Content: strings.Repeat("x", 100),
		ToolCalls: []provider.ToolCall{{
			ID: "toolu_trunc", Type: "function",
			Function: provider.FunctionCallDesc{Name: "get_weather", Arguments: `{"city":"Hanoi"}`},
		}},
	}}
	srv := newRecordingGateway(t, adapter)
	status, raw := postAnthropic(t, srv, `{"model":"rec-chat","max_tokens":2,"messages":[{"role":"user","content":"thời tiết?"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	var resp struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopReason != "max_tokens" {
		t.Fatalf("stop_reason=%q, want max_tokens", resp.StopReason)
	}
	for _, block := range resp.Content {
		if block["type"] == "tool_use" {
			t.Fatalf("truncated turn emitted tool_use: %+v", resp.Content)
		}
	}
}

func TestAnthropicStreamToolUseBlocks(t *testing.T) {
	adapter := &recordingAdapter{chunks: []provider.StreamChunk{
		{Content: "Đã kiểm tra."},
		{Done: true, ToolCalls: []provider.ToolCall{{
			ID: "toolu_9", Type: "function",
			Function: provider.FunctionCallDesc{Name: "get_weather", Arguments: `{"city":"Hanoi"}`},
		}}},
	}}
	srv := newRecordingGateway(t, adapter)
	status, raw := postAnthropic(t, srv, `{"model":"rec-chat","stream":true,"messages":[{"role":"user","content":"thời tiết?"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	stream := string(raw)
	for _, want := range []string{
		`"type":"tool_use"`,
		`"partial_json":"{\"city\":\"Hanoi\"}"`,
		`"stop_reason":"tool_use"`,
		`event: content_block_stop`,
		`event: message_stop`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %s: %s", want, stream)
		}
	}
	if strings.Index(stream, `"type":"text"`) > strings.Index(stream, `"type":"tool_use"`) {
		t.Fatalf("text block must come before tool_use: %s", stream)
	}
	if strings.Index(stream, `"partial_json"`) > strings.Index(stream, `"stop_reason":"tool_use"`) {
		t.Fatalf("input_json_delta must precede message_delta: %s", stream)
	}
}

func TestAnthropicStreamWithoutToolsKeepsText(t *testing.T) {
	adapter := &recordingAdapter{chunks: []provider.StreamChunk{
		{ReasoningContent: "suy nghĩ"},
		{Content: "hello"},
		{Done: true},
	}}
	srv := newRecordingGateway(t, adapter)
	status, raw := postAnthropic(t, srv, `{"model":"rec-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	stream := string(raw)
	if !strings.Contains(stream, `"type":"signature_delta"`) {
		t.Fatalf("thinking block missing signature_delta: %s", stream)
	}
	if !strings.Contains(stream, "thinking_delta") || !strings.Contains(stream, "text_delta") {
		t.Fatalf("missing deltas: %s", stream)
	}
	if !strings.Contains(stream, `"stop_reason":"end_turn"`) {
		t.Fatalf("stop_reason should be end_turn: %s", stream)
	}
	if strings.Contains(stream, `"type":"tool_use"`) {
		t.Fatalf("unexpected tool_use block: %s", stream)
	}
}

func TestAnthropicToolChoiceUnknownToolRejected(t *testing.T) {
	adapter := &recordingAdapter{chat: provider.ChatResponse{ID: "rec-5", Content: "x"}}
	srv := newRecordingGateway(t, adapter)
	body := `{"model":"rec-chat","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather"}],"tool_choice":{"type":"tool","name":"nope"}}`
	status, raw := postAnthropic(t, srv, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if !strings.Contains(string(raw), "tool_not_found") {
		t.Fatalf("body=%s", raw)
	}
}

func TestAnthropicNonStreamThinkingCarriesSignature(t *testing.T) {
	adapter := &recordingAdapter{chat: provider.ChatResponse{ID: "rec-sig", Content: "ok", ReasoningContent: "nghĩ"}}
	srv := newRecordingGateway(t, adapter)
	status, raw := postAnthropic(t, srv, `{"model":"rec-chat","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if !strings.Contains(string(raw), `"type":"thinking"`) || !strings.Contains(string(raw), `"signature":""`) {
		t.Fatalf("thinking block missing signature: %s", raw)
	}
}
