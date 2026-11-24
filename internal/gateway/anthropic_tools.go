package gateway

import (
	"encoding/json"
	"strings"

	"sever_provider/internal/provider"
)

// AnthropicTool is a tool definition as the Claude SDK sends it on /v1/messages.
type AnthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// AnthropicToolChoice mirrors Anthropic's tool_choice: auto|any|tool|none.
type AnthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// anthropicToolsToProvider maps Anthropic tool definitions onto the provider
// tool shape the chat adapters already understand.
func anthropicToolsToProvider(in []AnthropicTool) []provider.Tool {
	var out []provider.Tool
	for _, tool := range in {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		out = append(out, provider.Tool{
			Type:     "function",
			Function: &provider.Function{Name: name, Description: tool.Description, Parameters: tool.InputSchema},
		})
	}
	return out
}

// anthropicToolChoiceToProvider maps Anthropic's tool_choice onto the values the
// adapters recognise: nil for auto, "required" for any, "none", or the OpenAI
// function map for a forced tool. provider.ToolChoiceRequiresCall and
// provider.FormatPromptWithTools read exactly these forms.
func anthropicToolChoiceToProvider(choice *AnthropicToolChoice) any {
	if choice == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(choice.Type)) {
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name := strings.TrimSpace(choice.Name); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return nil
}

// anthropicToolConfigured reports whether tool_choice names a declared tool.
func anthropicToolConfigured(tools []AnthropicTool, name string) bool {
	name = strings.TrimSpace(name)
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == name {
			return true
		}
	}
	return false
}

// toolInputObject normalises tool-call arguments into the JSON object that
// Anthropic's tool_use.input requires. Anything else (empty, array, invalid
// JSON) degrades to {} so the SDK always gets a parsable object.
func toolInputObject(args string) map[string]any {
	trimmed := strings.TrimSpace(args)
	obj := map[string]any{}
	if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &obj) == nil {
		return obj
	}
	return map[string]any{}
}

// anthropicToolUseBlock renders a provider tool call as an Anthropic tool_use
// content block.
func anthropicToolUseBlock(tc provider.ToolCall) map[string]any {
	return map[string]any{
		"type":  "tool_use",
		"id":    tc.ID,
		"name":  tc.Function.Name,
		"input": toolInputObject(tc.Function.Arguments),
	}
}

// anthropicMessages decodes Anthropic message content blocks (text, tool_use,
// tool_result) while still accepting the OpenAI-ish fields this gateway already
// supported (content_parts, tool_calls, tool_call_id, images).
type anthropicMessages []provider.Message

type anthropicBlock struct {
	Type      string             `json:"type"`
	Text      string             `json:"text"`
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Input     json.RawMessage    `json:"input"`
	ToolUseID string             `json:"tool_use_id"`
	Content   json.RawMessage    `json:"content"`
	ImageURL  *provider.ImageURL `json:"image_url"`
}

func (m *anthropicMessages) UnmarshalJSON(data []byte) error {
	if strings.TrimSpace(string(data)) == "null" {
		*m = nil
		return nil
	}
	var raw []struct {
		Role             string                  `json:"role"`
		Content          json.RawMessage         `json:"content"`
		ContentParts     []provider.ContentPart  `json:"content_parts"`
		ToolCalls        []provider.ToolCall     `json:"tool_calls"`
		ToolCallID       string                  `json:"tool_call_id"`
		Name             string                  `json:"name"`
		Images           []provider.MessageImage `json:"images"`
		ReasoningContent string                  `json:"reasoning_content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make(anthropicMessages, 0, len(raw))
	for _, in := range raw {
		base := provider.Message{
			Role:             in.Role,
			Name:             in.Name,
			ToolCallID:       in.ToolCallID,
			ToolCalls:        in.ToolCalls,
			Parts:            in.ContentParts,
			Images:           in.Images,
			ReasoningContent: in.ReasoningContent,
		}
		trimmed := strings.TrimSpace(string(in.Content))
		if trimmed == "" || trimmed == "null" {
			if !anthropicSkipEmpty(base) {
				out = append(out, base)
			}
			continue
		}
		var plain string
		if json.Unmarshal(in.Content, &plain) == nil {
			base.Content = plain
			if !anthropicSkipEmpty(base) {
				out = append(out, base)
			}
			continue
		}
		var blocks []anthropicBlock
		if json.Unmarshal(in.Content, &blocks) != nil {
			// Unknown content shape: keep the message fields decoded so far
			// rather than rejecting the request.
			if !anthropicSkipEmpty(base) {
				out = append(out, base)
			}
			continue
		}
		out = append(out, anthropicMessageFromBlocks(base, blocks)...)
	}
	*m = out
	return nil
}

// anthropicMessageFromBlocks turns one Anthropic message's content blocks into
// provider messages. Text keeps its position relative to tool_result blocks, so
// a user turn of [tool_result, text] still reaches the prompt in that order.
func anthropicMessageFromBlocks(base provider.Message, blocks []anthropicBlock) []provider.Message {
	var out []provider.Message
	assistant := strings.EqualFold(strings.TrimSpace(base.Role), "assistant")
	var texts []string
	flushText := func() {
		if len(texts) == 0 {
			return
		}
		msg := base
		msg.Role = "user"
		if assistant {
			msg.Role = "assistant"
		}
		msg.Content = strings.Join(texts, "\n")
		msg.ToolCalls = nil
		texts = nil
		out = append(out, msg)
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				texts = append(texts, block.Text)
			}
		case "tool_use":
			if assistant {
				base.ToolCalls = append(base.ToolCalls, provider.ToolCall{
					ID:       block.ID,
					Type:     "function",
					Function: provider.FunctionCallDesc{Name: block.Name, Arguments: anthropicToolUseArguments(block.Input)},
				})
			}
		case "tool_result":
			flushText()
			out = append(out, provider.Message{
				Role:       "tool",
				ToolCallID: block.ToolUseID,
				Content:    anthropicToolResultText(block.Content),
			})
		case "image_url":
			// OpenAI-style image part inside an Anthropic content array: keep it
			// at its position so HasImages still sees it.
			if block.ImageURL != nil {
				flushText()
				out = append(out, provider.Message{Role: "user", Parts: []provider.ContentPart{{Type: "image_url", ImageURL: block.ImageURL}}})
			}
		}
		// thinking/redacted_thinking/image and unknown block types are dropped.
	}
	if assistant {
		base.Content = strings.Join(texts, "\n")
		if !anthropicSkipEmpty(base) {
			out = append(out, base)
		}
		return out
	}
	flushText()
	return out
}

// anthropicSkipEmpty drops messages that carry nothing the prompt formatter can
// use, e.g. a user turn made only of dropped blocks.
func anthropicSkipEmpty(msg provider.Message) bool {
	return msg.Content == "" && len(msg.Parts) == 0 && len(msg.ToolCalls) == 0 && msg.ToolCallID == "" && msg.Name == ""
}

// anthropicToolUseArguments renders a tool_use.input as arguments JSON; a
// missing or non-object input becomes {}.
func anthropicToolUseArguments(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return "{}"
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// anthropicToolResultText flattens tool_result.content (a string or an array of
// text blocks) into plain text for the web-LLM prompt.
func anthropicToolResultText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}
