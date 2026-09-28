package provider

import (
	"strings"
	"testing"
)

func TestParseToolCalls_Single(t *testing.T) {
	input := "Tôi sẽ kiểm tra thời tiết cho bạn:\n```tool_call\n{\"name\": \"get_current_weather\", \"arguments\": {\"location\": \"Hà Nội\"}}\n```\nXin chờ một lát."
	clean, calls := ParseToolCalls(input)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_current_weather" {
		t.Errorf("expected get_current_weather, got %s", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "Hà Nội") {
		t.Errorf("arguments missing Hanoi: %s", calls[0].Function.Arguments)
	}
	if strings.Contains(clean, "tool_call") {
		t.Errorf("cleanText still contains tool_call fence: %s", clean)
	}
	if !strings.Contains(clean, "Tôi sẽ kiểm tra") || !strings.Contains(clean, "Xin chờ một lát.") {
		t.Errorf("cleanText removed surrounding text: %s", clean)
	}
}

func TestParseToolCalls_Multiple(t *testing.T) {
	input := "```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.txt\"}}\n```\n```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"b.txt\"}}\n```"
	clean, calls := ParseToolCalls(input)

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" || calls[1].Function.Name != "read_file" {
		t.Errorf("unexpected call names: %v, %v", calls[0].Function.Name, calls[1].Function.Name)
	}
	if clean != "" {
		t.Errorf("expected empty clean text, got: %q", clean)
	}
}

func TestParseToolCalls_ArrayBlock(t *testing.T) {
	input := "```json\n[\n  {\"name\": \"run_cmd\", \"arguments\": {\"command\": \"ls -la\"}},\n  {\"name\": \"run_cmd\", \"arguments\": {\"command\": \"pwd\"}}\n]\n```"
	clean, calls := ParseToolCalls(input)

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls from array, got %d", len(calls))
	}
	if calls[0].Function.Name != "run_cmd" {
		t.Errorf("expected run_cmd, got %s", calls[0].Function.Name)
	}
	if clean != "" {
		t.Errorf("expected empty clean text, got: %q", clean)
	}
}

func TestParseToolCalls_ArgumentsFirst(t *testing.T) {
	input := "```tool_call\n{\n  \"arguments\": {\"city\": \"Danang\"},\n  \"name\": \"find_hotel\"\n}\n```"
	clean, calls := ParseToolCalls(input)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "find_hotel" {
		t.Errorf("expected find_hotel, got %s", calls[0].Function.Name)
	}
	if clean != "" {
		t.Errorf("expected empty clean text, got: %q", clean)
	}
}

func TestParseToolCalls_NoTools(t *testing.T) {
	input := "Xin chào! Hôm nay tôi có thể giúp gì cho bạn?"
	clean, calls := ParseToolCalls(input)

	if len(calls) != 0 {
		t.Fatalf("expected 0 calls, got %d", len(calls))
	}
	if clean != input {
		t.Errorf("expected %q, got %q", input, clean)
	}
}

func TestFormatPromptWithTools_MultiTurn(t *testing.T) {
	tools := []Tool{
		{
			Type: "function",
			Function: &Function{
				Name:        "get_weather",
				Description: "Lấy thời tiết",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	messages := []Message{
		{Role: "user", Content: "Thời tiết Hà Nội hôm nay?"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{
					ID:   "call_0_get_weather",
					Type: "function",
					Function: FunctionCallDesc{
						Name:      "get_weather",
						Arguments: "{\"city\": \"Hanoi\"}",
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_0_get_weather",
			Content:    "{\"temperature\": 28, \"condition\": \"Sunny\"}",
		},
		{Role: "user", Content: "Vậy có mưa không?"},
	}

	prompt := FormatPromptWithTools(messages, tools, "auto")

	if !strings.Contains(prompt, "# Tools Available") {
		t.Errorf("prompt missing tools header")
	}
	if !strings.Contains(prompt, "get_weather") {
		t.Errorf("prompt missing tool name")
	}
	if !strings.Contains(prompt, "[Tool Result (call_0_get_weather)]") {
		t.Errorf("prompt missing tool result role")
	}
	if !strings.Contains(prompt, "```tool_call") {
		t.Errorf("prompt missing assistant tool call block")
	}
	if !strings.Contains(prompt, "Vậy có mưa không?") {
		t.Errorf("prompt missing final user message")
	}
}
