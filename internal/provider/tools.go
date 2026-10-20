package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var toolBlockRegex = regexp.MustCompile("(?s)```(?:tool_call|tool_calls|json)?\\s*\\n?([^{`]*?\\{.*?\\}[^`]*?)\\n?```")

type rawToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Function  *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function,omitempty"`
}

// ParseToolCalls trích xuất các tool call block dạng JSON khỏi text phản hồi của LLM.
// Trả về cleanText (đã loại bỏ các block tool call) và danh sách ToolCall.
func ParseToolCalls(text string) (string, []ToolCall) {
	if text == "" {
		return "", nil
	}

	var calls []ToolCall
	matches := toolBlockRegex.FindAllStringSubmatch(text, -1)

	for i, match := range matches {
		if len(match) < 2 {
			continue
		}
		rawBlock := strings.TrimSpace(match[1])

		// 1. Thử unmarshal dạng single object
		var single rawToolCall
		if err := json.Unmarshal([]byte(rawBlock), &single); err == nil {
			name, args := extractNameAndArgs(single)
			if name != "" {
				idx := len(calls)
				calls = append(calls, ToolCall{
					Index: &idx,
					ID:    fmt.Sprintf("call_%d_%s", i, name),
					Type:  "function",
					Function: FunctionCallDesc{
						Name:      name,
						Arguments: args,
					},
				})
				continue
			}
		}

		// 2. Thử unmarshal dạng array of objects
		var list []rawToolCall
		if err := json.Unmarshal([]byte(rawBlock), &list); err == nil && len(list) > 0 {
			for j, item := range list {
				name, args := extractNameAndArgs(item)
				if name != "" {
					idx := len(calls)
					calls = append(calls, ToolCall{
						Index: &idx,
						ID:    fmt.Sprintf("call_%d_%d_%s", i, j, name),
						Type:  "function",
						Function: FunctionCallDesc{
							Name:      name,
							Arguments: args,
						},
					})
				}
			}
		}
	}

	if len(calls) == 0 {
		return strings.TrimSpace(text), nil
	}

	cleaned := toolBlockRegex.ReplaceAllString(text, "")
	return strings.TrimSpace(cleaned), calls
}

func extractNameAndArgs(c rawToolCall) (string, string) {
	name := c.Name
	rawArgs := c.Arguments
	if c.Function != nil && c.Function.Name != "" {
		name = c.Function.Name
		if len(c.Function.Arguments) > 0 {
			rawArgs = c.Function.Arguments
		}
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return "", "{}"
	}

	argsStr := "{}"
	if len(rawArgs) > 0 {
		trimmed := strings.TrimSpace(string(rawArgs))
		if strings.HasPrefix(trimmed, "\"") {
			var unescaped string
			if err := json.Unmarshal(rawArgs, &unescaped); err == nil {
				argsStr = unescaped
			} else {
				argsStr = trimmed
			}
		} else if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			argsStr = trimmed
		}
	}

	return name, argsStr
}

// FormatPromptWithTools format tin nhắn và định nghĩa tools vào prompt duy nhất cho các Web LLM adapter.
// ponytail: single unified prompt formatter; add per-model dialect overrides if upstream introduces special tokens.
func FormatPromptWithTools(messages []Message, tools []Tool, toolChoice any) string {
	var parts []string

	hasTools := len(tools) > 0 && toolChoice != "none"
	if hasTools {
		var toolDefs []map[string]any
		for _, t := range tools {
			if t.Type == "function" && t.Function != nil {
				toolDefs = append(toolDefs, map[string]any{
					"name":        t.Function.Name,
					"description": t.Function.Description,
					"parameters":  t.Function.Parameters,
				})
			}
		}
		if len(toolDefs) > 0 {
			b, _ := json.MarshalIndent(toolDefs, "", "  ")
			toolIntro := "# Tools Available\n\nYou have access to the following tools:\n" + string(b) + "\n\n" +
				"## Tool Calling Instructions:\n" +
				"1. To call a tool, you MUST output a tool call code block in this exact JSON format:\n" +
				"```tool_call\n" +
				"{\"name\": \"function_name\", \"arguments\": {\"param\": \"value\"}}\n" +
				"```\n" +
				"2. For multiple tool calls, output each ```tool_call block sequentially.\n" +
				"3. When you decide to call a tool, output ONLY the ```tool_call blocks. Do not add introductory or concluding conversational text.\n" +
				"4. If no tool is needed, answer normally with regular text without code fences.\n"

			// Xử lý toolChoice bắt buộc
			if tcStr, ok := toolChoice.(string); ok && strings.EqualFold(tcStr, "required") {
				toolIntro += "5. IMPORTANT: You MUST call at least one tool for this request.\n"
			} else if tcMap, ok := toolChoice.(map[string]any); ok {
				if fn, ok := tcMap["function"].(map[string]any); ok {
					if fnName, ok := fn["name"].(string); ok && fnName != "" {
						toolIntro += fmt.Sprintf("5. IMPORTANT: You MUST call the tool %q for this request.\n", fnName)
					}
				}
			}

			parts = append(parts, toolIntro)
		}
	}

	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		content := m.TextContent()

		toolCallBlocks := ""
		if role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				toolCallBlocks += fmt.Sprintf("```tool_call\n{\"name\": %q, \"arguments\": %s}\n```\n", tc.Function.Name, tc.Function.Arguments)
			}
		}

		switch role {
		case "system":
			parts = append(parts, fmt.Sprintf("[System Instruction]: %s", content))
		case "assistant":
			line := "[Assistant]: " + strings.TrimSpace(content)
			if toolCallBlocks != "" {
				if strings.TrimSpace(content) == "" {
					line = "[Assistant]:\n" + strings.TrimSpace(toolCallBlocks)
				} else {
					line = "[Assistant]: " + strings.TrimSpace(content) + "\n\n" + strings.TrimSpace(toolCallBlocks)
				}
			}
			parts = append(parts, line)
		case "tool":
			toolName := m.Name
			if toolName == "" {
				toolName = m.ToolCallID
			}
			parts = append(parts, fmt.Sprintf("[Tool Result (%s)]: %s", toolName, content))
		default: // "user"
			parts = append(parts, fmt.Sprintf("[User]: %s", content))
		}
	}

	return strings.Join(parts, "\n\n")
}
