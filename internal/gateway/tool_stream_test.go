package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/provider"
)

func postChatStream(t *testing.T, srv *httptest.Server, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// rawDataLines returns the SSE data payloads, skipping keepalive comments and
// the [DONE] sentinel.
func rawDataLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		out = append(out, payload)
	}
	return out
}

func TestSplitToolCallDeltas(t *testing.T) {
	index := 2
	// 9 + 89 + 2 = 100 runes, so 48/48/4 at maxRunes=48.
	args := `{"city":"` + strings.Repeat("a", 89) + `"}`
	frags := splitToolCallDeltas([]provider.ToolCall{{
		Index: &index, ID: "call_0", Type: "function",
		Function: provider.FunctionCallDesc{Name: "get_weather", Arguments: args},
	}}, 48)
	if len(frags) != 3 {
		t.Fatalf("fragments=%d, want 3", len(frags))
	}
	var joined strings.Builder
	for i, frag := range frags {
		if frag.Index == nil || *frag.Index != index {
			t.Fatalf("fragment %d index=%v, want %d", i, frag.Index, index)
		}
		if i == 0 {
			if frag.ID != "call_0" || frag.Type != "function" || frag.Function.Name != "get_weather" {
				t.Fatalf("first fragment lost metadata: %+v", frag)
			}
		} else if frag.ID != "" || frag.Type != "" || frag.Function.Name != "" {
			t.Fatalf("fragment %d must carry only index+arguments: %+v", i, frag)
		}
		joined.WriteString(frag.Function.Arguments)
	}
	if joined.String() != args {
		t.Fatalf("joined arguments mismatch: %q != %q", joined.String(), args)
	}
	if len([]rune(frags[0].Function.Arguments)) != 48 || len([]rune(frags[1].Function.Arguments)) != 48 || len([]rune(frags[2].Function.Arguments)) != 4 {
		t.Fatalf("fragment sizes=%d/%d/%d, want 48/48/4",
			len([]rune(frags[0].Function.Arguments)), len([]rune(frags[1].Function.Arguments)), len([]rune(frags[2].Function.Arguments)))
	}

	// Two calls in one chunk: each keeps its own index and only its first
	// fragment carries metadata. Empty arguments still ship their metadata.
	second := 3
	two := splitToolCallDeltas([]provider.ToolCall{
		{Index: &index, ID: "call_a", Type: "function", Function: provider.FunctionCallDesc{Name: "a", Arguments: "{}"}},
		{Index: &second, ID: "call_b", Type: "function", Function: provider.FunctionCallDesc{Name: "b"}},
	}, 48)
	if len(two) != 2 {
		t.Fatalf("two calls -> %d fragments, want 2", len(two))
	}
	if two[0].ID != "call_a" || two[0].Function.Name != "a" || *two[0].Index != index {
		t.Fatalf("first call fragment=%+v", two[0])
	}
	if two[1].ID != "call_b" || two[1].Function.Name != "b" || *two[1].Index != second || two[1].Function.Arguments != "" {
		t.Fatalf("second call fragment=%+v", two[1])
	}
}

func TestStreamChatSplitsToolCallArguments(t *testing.T) {
	index := 0
	args := `{"city":"` + strings.Repeat("x", 90) + `"}`
	adapter := &recordingAdapter{chunks: []provider.StreamChunk{{
		Done:         true,
		FinishReason: "tool_calls",
		ToolCalls: []provider.ToolCall{{
			Index: &index, ID: "call_0", Type: "function",
			Function: provider.FunctionCallDesc{Name: "get_weather", Arguments: args},
		}},
	}}}
	srv := newRecordingGateway(t, adapter)
	status, raw := postChatStream(t, srv, `{"model":"rec-chat","stream":true,"messages":[{"role":"user","content":"thời tiết?"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}

	var fragments []provider.ToolCall
	finishCount := 0
	lastFinish := ""
	for _, payload := range rawDataLines(string(raw)) {
		var chunk ChatCompletionResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decode chunk: %v payload=%s", err, payload)
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				finishCount++
				lastFinish = *choice.FinishReason
			}
			if choice.Delta == nil {
				continue
			}
			fragments = append(fragments, choice.Delta.ToolCalls...)
		}
	}
	if len(fragments) < 2 {
		t.Fatalf("got %d tool fragments, want arguments split", len(fragments))
	}
	want := (len([]rune(args)) + 47) / 48
	if len(fragments) != want {
		t.Fatalf("fragments=%d, want %d", len(fragments), want)
	}
	var joined strings.Builder
	for i, frag := range fragments {
		if frag.Index == nil || *frag.Index != index {
			t.Fatalf("fragment %d index=%v, want %d", i, frag.Index, index)
		}
		if i == 0 && (frag.ID != "call_0" || frag.Type != "function" || frag.Function.Name != "get_weather") {
			t.Fatalf("first fragment metadata=%+v", frag)
		}
		joined.WriteString(frag.Function.Arguments)
	}
	if joined.String() != args {
		t.Fatalf("joined arguments mismatch: %q", joined.String())
	}
	if finishCount != 1 || lastFinish != "tool_calls" {
		t.Fatalf("finish_reason emitted %d times, last=%q", finishCount, lastFinish)
	}

	// Later fragments must omit id/type/name on the wire, per the OpenAI shape.
	seen := 0
	for _, payload := range rawDataLines(string(raw)) {
		if !strings.Contains(payload, `"tool_calls"`) {
			continue
		}
		var wire map[string]any
		if err := json.Unmarshal([]byte(payload), &wire); err != nil {
			t.Fatalf("decode wire: %v", err)
		}
		choices, _ := wire["choices"].([]any)
		if len(choices) == 0 {
			t.Fatalf("chunk without choices: %s", payload)
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		if len(calls) == 0 {
			continue
		}
		call, _ := calls[0].(map[string]any)
		fn, _ := call["function"].(map[string]any)
		if seen > 0 {
			for _, key := range []string{"id", "type"} {
				if _, ok := call[key]; ok {
					t.Fatalf("fragment %d leaked %q: %s", seen, key, payload)
				}
			}
			if _, ok := fn["name"]; ok {
				t.Fatalf("fragment %d leaked function.name: %s", seen, payload)
			}
		}
		seen++
	}
	if seen != want {
		t.Fatalf("wire fragments=%d, want %d", seen, want)
	}
}

func TestStreamChatTextOnlyStillEmitsOneFinish(t *testing.T) {
	adapter := &recordingAdapter{chunks: []provider.StreamChunk{
		{Content: "hello world"},
		{Done: true},
	}}
	srv := newRecordingGateway(t, adapter)
	status, raw := postChatStream(t, srv, `{"model":"rec-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	finishCount := 0
	lastFinish := ""
	for _, payload := range rawDataLines(string(raw)) {
		var chunk ChatCompletionResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				finishCount++
				lastFinish = *choice.FinishReason
			}
		}
		if strings.Contains(payload, `"tool_calls"`) {
			t.Fatalf("unexpected tool_calls in text-only stream: %s", payload)
		}
	}
	if finishCount != 1 || lastFinish != "stop" {
		t.Fatalf("finish_reason emitted %d times, last=%q", finishCount, lastFinish)
	}
}

// TestToolCallMarshalKeepsFields proves the omitempty tags do not strip
// metadata from the messages forwarded to a Colab/vLLM worker.
func TestToolCallMarshalKeepsFields(t *testing.T) {
	full := provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{
		ID: "call_1", Type: "function",
		Function: provider.FunctionCallDesc{Name: "get_weather", Arguments: "{}"},
	}}}
	encoded, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"id":"call_1"`, `"type":"function"`, `"name":"get_weather"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("worker payload lost %s: %s", want, encoded)
		}
	}
	// An empty-field call (only possible for hand-built values) loses the keys;
	// that is the accepted trade-off of the streaming tags.
	empty, err := json.Marshal(provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{
		Function: provider.FunctionCallDesc{Arguments: "{}"},
	}}})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	for _, unwanted := range []string{`"id":""`, `"type":""`, `"name":""`} {
		if strings.Contains(string(empty), unwanted) {
			t.Fatalf("empty tool call kept %s: %s", unwanted, empty)
		}
	}
}
