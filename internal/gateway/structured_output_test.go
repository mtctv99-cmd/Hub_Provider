package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

// scriptedAdapter replays a fixed list of chat responses and records every
// provider request, so retry behaviour can be asserted.
type scriptedAdapter struct {
	mu        sync.Mutex
	responses []provider.ChatResponse
	requests  []provider.ChatRequest
}

func (a *scriptedAdapter) Name() string { return "scripted_mock" }
func (a *scriptedAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}
}

func (a *scriptedAdapter) next(req provider.ChatRequest) provider.ChatResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, req)
	if len(a.responses) == 0 {
		return provider.ChatResponse{ID: "scripted-0", Content: "{}"}
	}
	resp := a.responses[0]
	if len(a.responses) > 1 {
		a.responses = a.responses[1:]
	}
	return resp
}

func (a *scriptedAdapter) Chat(_ context.Context, req provider.ChatRequest, _ []byte) (provider.ChatResponse, error) {
	return a.next(req), nil
}

func (a *scriptedAdapter) StreamChat(_ context.Context, req provider.ChatRequest, _ []byte) (<-chan provider.StreamChunk, error) {
	resp := a.next(req)
	ch := make(chan provider.StreamChunk, 2)
	go func() {
		defer close(ch)
		ch <- provider.StreamChunk{Content: resp.Content}
		ch <- provider.StreamChunk{Done: true}
	}()
	return ch, nil
}

func (a *scriptedAdapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (a *scriptedAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}
func (a *scriptedAdapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

func (a *scriptedAdapter) recorded() []provider.ChatRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]provider.ChatRequest(nil), a.requests...)
}

func newScriptedGateway(t *testing.T, adapter *scriptedAdapter) *httptest.Server {
	t.Helper()
	models := scheduler.NewModelRegistry()
	if err := models.Register(scheduler.ModelSpec{ID: "scripted-chat", Provider: "scripted_mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}, TimeoutSec: 10}); err != nil {
		t.Fatalf("register model: %v", err)
	}
	reg := provider.NewRegistry()
	reg.Register(adapter)
	d := &scheduler.Dispatcher{Models: models, Credentials: mockSource{}, Adapters: reg, Leases: scheduler.NewLeaseManager()}
	srv := httptest.NewServer(NewRouter(NewHandler(d, models), ""))
	t.Cleanup(srv.Close)
	return srv
}

func postScriptedChat(t *testing.T, srv *httptest.Server, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

const schemaBody = `"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","required":["answer"],"properties":{"answer":{"type":"integer"}}}}}`

func TestChatStructuredOutputValidFirstTry(t *testing.T) {
	adapter := &scriptedAdapter{responses: []provider.ChatResponse{{ID: "s1", Content: `{"answer":42}`}}}
	srv := newScriptedGateway(t, adapter)
	status, raw := postScriptedChat(t, srv, `{"model":"scripted-chat","messages":[{"role":"user","content":"give me json"}],`+schemaBody+`}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if calls := len(adapter.recorded()); calls != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls)
	}
	req := adapter.recorded()[0]
	if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
		t.Fatalf("schema instruction not injected: %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[0].Content, "JSON Schema") || !strings.Contains(req.Messages[0].Content, `"required"`) {
		t.Fatalf("schema instruction missing schema: %q", req.Messages[0].Content)
	}
}

func TestChatStructuredOutputRetriesOnceThenSucceeds(t *testing.T) {
	adapter := &scriptedAdapter{responses: []provider.ChatResponse{
		{ID: "s1", Content: "Sure! Here is the JSON you asked for."},
		{ID: "s2", Content: `{"answer":42}`},
	}}
	srv := newScriptedGateway(t, adapter)
	status, raw := postScriptedChat(t, srv, `{"model":"scripted-chat","messages":[{"role":"user","content":"give me json"}],`+schemaBody+`}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	reqs := adapter.recorded()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls=%d, want 2", len(reqs))
	}
	retry := reqs[1].Messages
	if len(retry) < 3 {
		t.Fatalf("retry messages=%+v", retry)
	}
	last := retry[len(retry)-1]
	assistant := retry[len(retry)-2]
	if assistant.Role != "assistant" || !strings.Contains(assistant.Content, "Sure!") {
		t.Fatalf("retry lost previous reply: %+v", assistant)
	}
	if last.Role != "user" || !strings.Contains(last.Content, "rejected") {
		t.Fatalf("retry lost correction prompt: %+v", last)
	}
	if retry[0].Role != "system" || !strings.Contains(retry[0].Content, "JSON Schema") {
		t.Fatalf("retry lost schema instruction: %+v", retry[0])
	}
}

func TestChatStructuredOutputFailsAfterRetry(t *testing.T) {
	adapter := &scriptedAdapter{responses: []provider.ChatResponse{
		{ID: "s1", Content: "still prose"},
		{ID: "s2", Content: "prose again"},
	}}
	srv := newScriptedGateway(t, adapter)
	status, raw := postScriptedChat(t, srv, `{"model":"scripted-chat","messages":[{"role":"user","content":"give me json"}],`+schemaBody+`}`)
	if status != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if !strings.Contains(string(raw), "invalid_response_format") {
		t.Fatalf("body=%s", raw)
	}
	if calls := len(adapter.recorded()); calls != 2 {
		t.Fatalf("upstream calls=%d, want exactly one retry", calls)
	}
}

func TestChatStructuredOutputRetriesMissingRequiredField(t *testing.T) {
	adapter := &scriptedAdapter{responses: []provider.ChatResponse{
		{ID: "s1", Content: `{}`},
		{ID: "s2", Content: `{"answer":42}`},
	}}
	srv := newScriptedGateway(t, adapter)
	status, raw := postScriptedChat(t, srv, `{"model":"scripted-chat","messages":[{"role":"user","content":"give me json"}],`+schemaBody+`}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if calls := len(adapter.recorded()); calls != 2 {
		t.Fatalf("upstream calls=%d, want one retry", calls)
	}
}

func TestChatStructuredOutputMissingRequiredFieldFailsAfterRetry(t *testing.T) {
	adapter := &scriptedAdapter{responses: []provider.ChatResponse{
		{ID: "s1", Content: `{}`},
		{ID: "s2", Content: `{}`},
	}}
	srv := newScriptedGateway(t, adapter)
	status, raw := postScriptedChat(t, srv, `{"model":"scripted-chat","messages":[{"role":"user","content":"give me json"}],`+schemaBody+`}`)
	if status != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if !strings.Contains(string(raw), "invalid_response_format") {
		t.Fatalf("body=%s", raw)
	}
	if calls := len(adapter.recorded()); calls != 2 {
		t.Fatalf("upstream calls=%d, want exactly one retry", calls)
	}
}

func TestChatStructuredOutputRejectsBadRequestShapes(t *testing.T) {
	cases := map[string]string{
		"unknown type":     `"response_format":{"type":"banana"}`,
		"schema is string": `"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":"nope"}}`,
		"schema is null":   `"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":null}}`,
		"schema missing":   `"response_format":{"type":"json_schema","json_schema":{"name":"x"}}`,
		"tools conflict":   `"tools":[{"type":"function","function":{"name":"f"}}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}`,
	}
	for name, fragment := range cases {
		t.Run(name, func(t *testing.T) {
			adapter := &scriptedAdapter{}
			srv := newScriptedGateway(t, adapter)
			status, raw := postScriptedChat(t, srv, `{"model":"scripted-chat","messages":[{"role":"user","content":"hi"}],`+fragment+`}`)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", status, raw)
			}
			if calls := len(adapter.recorded()); calls != 0 {
				t.Fatalf("upstream called %d times for an invalid request", calls)
			}
		})
	}
}

func TestChatResponseFormatTextIsNoOp(t *testing.T) {
	adapter := &scriptedAdapter{responses: []provider.ChatResponse{{ID: "s1", Content: "plain answer"}}}
	srv := newScriptedGateway(t, adapter)
	status, raw := postScriptedChat(t, srv, `{"model":"scripted-chat","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	req := adapter.recorded()[0]
	if req.Messages[0].Role == "system" {
		t.Fatalf("text format should not inject an instruction: %+v", req.Messages)
	}
}

func TestChatStructuredOutputTruncationFailsWithoutRetry(t *testing.T) {
	adapter := &scriptedAdapter{responses: []provider.ChatResponse{{ID: "s1", Content: `{"answer":42}`}}}
	srv := newScriptedGateway(t, adapter)
	status, raw := postScriptedChat(t, srv, `{"model":"scripted-chat","max_tokens":1,"messages":[{"role":"user","content":"give me json"}],`+schemaBody+`}`)
	if status != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if calls := len(adapter.recorded()); calls != 1 {
		t.Fatalf("truncated output retried: calls=%d", calls)
	}
}

func TestChatStructuredOutputStreamIsBestEffort(t *testing.T) {
	adapter := &scriptedAdapter{responses: []provider.ChatResponse{{ID: "s1", Content: `{"answer":42}`}}}
	srv := newScriptedGateway(t, adapter)
	status, raw := postScriptedChat(t, srv, `{"model":"scripted-chat","stream":true,"messages":[{"role":"user","content":"hi"}],`+schemaBody+`}`)
	if status != http.StatusOK {
		t.Fatalf("stream + response_format must not be rejected: status=%d body=%s", status, raw)
	}
	if !strings.Contains(string(raw), "data: [DONE]") {
		t.Fatalf("expected SSE stream: %s", raw)
	}
}

func TestValidateStructuredOutput(t *testing.T) {
	object := structuredFormat{kind: "json_schema", schema: map[string]any{"type": "object"}}
	array := structuredFormat{kind: "json_schema", schema: map[string]any{"type": []any{"array", "null"}}}
	union := structuredFormat{kind: "json_schema", schema: map[string]any{"type": []any{"object", "null"}}}
	unknown := structuredFormat{kind: "json_schema", schema: map[string]any{}}
	jsonObject := structuredFormat{kind: "json_object"}
	required := structuredFormat{kind: "json_schema", schema: map[string]any{"type": "object", "required": []any{"answer"}}}
	badRequired := structuredFormat{kind: "json_schema", schema: map[string]any{"type": "object", "required": "answer"}}

	valid := map[string]struct {
		format structuredFormat
		body   string
	}{
		"object":            {object, `{"a":1}`},
		"fenced object":     {object, "```json\n{\"a\":1}\n```"},
		"prose before":      {object, `Here is the result: {"a":1}`},
		"prose after":       {object, `{"a":1} Let me know if you need anything else.`},
		"fenced and prose":  {object, "Sure:\n```json\n{\"a\":1}\n```\nDone."},
		"array":             {array, `[1,2]`},
		"union object":      {union, `{"a":1}`},
		"unknown any value": {unknown, `"x"`},
		"unknown array":     {unknown, `[1]`},
		"json_object":       {jsonObject, `{"a":1}`},
		"required present":  {required, `{"answer":1}`},
		"required null":     {required, `{"answer":null}`},
		"bad required type": {badRequired, `{}`},
	}
	for name, tc := range valid {
		if err := validateStructuredOutput(tc.body, tc.format); err != nil {
			t.Fatalf("%s: unexpected error %v", name, err)
		}
	}

	invalid := map[string]struct {
		format structuredFormat
		body   string
	}{
		"prose":         {object, "here you go"},
		"array as obj":  {object, `[1]`},
		"obj as array":  {array, `{"a":1}`},
		"null as array": {array, "null"},
		"null as union": {union, "null"},
		"union string":  {union, `"x"`},
		"empty":         {object, "   "},
		"json null":     {object, "null"},
		"json_object":   {jsonObject, `"string"`},
		"missing field": {required, `{}`},
	}
	for name, tc := range invalid {
		if err := validateStructuredOutput(tc.body, tc.format); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}
