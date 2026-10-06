package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrCapabilityUnsupported = errors.New("provider does not support requested capability")
	ErrNoCapacity            = errors.New("no provider capacity available")
	ErrProviderUnavailable   = errors.New("provider unavailable")
	ErrEmptyResponse         = errors.New("empty_response")
)

// EmptyResponseReason names why a chat or stream turn produced no content.
// EmptyReasonUpstreamEmpty means the upstream produced no text at all;
// EmptyReasonParserFailed means the upstream payload had text-like content
// the parser could not extract. Keep values lowercase so gateway masking and
// error wrapping can match them without exposing credentials.
type EmptyResponseReason string

const (
	EmptyReasonUpstreamEmpty EmptyResponseReason = "upstream_empty"
	EmptyReasonParserFailed  EmptyResponseReason = "parser_failed"
)

// EmptyResponseError is a coded empty-content failure. Detail must carry a
// safe diagnosis (payload length, frame presence, counts) and never cookies,
// tokens, prompts, or chat text.
type EmptyResponseError struct {
	Reason EmptyResponseReason
	Detail string
}

func (e *EmptyResponseError) Error() string {
	if e == nil {
		return ErrEmptyResponse.Error()
	}
	reason := strings.TrimSpace(string(e.Reason))
	if reason == "" {
		reason = string(EmptyReasonUpstreamEmpty)
	}
	if detail := strings.TrimSpace(e.Detail); detail != "" {
		return fmt.Sprintf("empty_response: %s (%s)", reason, detail)
	}
	return fmt.Sprintf("empty_response: %s", reason)
}

// Is reports ErrEmptyResponse so errors.Is matches any empty response.
func (e *EmptyResponseError) Is(target error) bool { return target == ErrEmptyResponse }

// NewEmptyResponseError builds a coded empty-content error.
func NewEmptyResponseError(reason EmptyResponseReason, detail string) error {
	reason = EmptyResponseReason(strings.TrimSpace(string(reason)))
	if reason != EmptyReasonParserFailed {
		reason = EmptyReasonUpstreamEmpty
	}
	return &EmptyResponseError{Reason: reason, Detail: strings.TrimSpace(detail)}
}

// EmptyResponseReasonOf extracts the reason from any wrapped error.
func EmptyResponseReasonOf(err error) (EmptyResponseReason, bool) {
	var target *EmptyResponseError
	if errors.As(err, &target) {
		return target.Reason, true
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "parser_failed"):
		return EmptyReasonParserFailed, true
	case strings.Contains(lower, "upstream_empty"), strings.Contains(lower, "empty upstream response"), strings.Contains(lower, "empty response"):
		return EmptyReasonUpstreamEmpty, true
	}
	return "", false
}

type Capability string

const (
	CapabilityChat      Capability = "chat"
	CapabilityStreaming Capability = "streaming"
	CapabilityVision    Capability = "vision"
	CapabilitySpeech    Capability = "audio_speech"
	CapabilityImage     Capability = "image_generation"
	CapabilityReasoning Capability = "reasoning"
)

type Message struct {
	Role             string        `json:"role"`
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	Name             string        `json:"name,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall    `json:"tool_calls,omitempty"`
	Parts            []ContentPart `json:"content_parts,omitempty"`
}

type Tool struct {
	Type     string    `json:"type"`
	Function *Function `json:"function,omitempty"`
}

type Function struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function FunctionCallDesc `json:"function"`
}

type FunctionCallDesc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		Name             string          `json:"name"`
		ToolCallID       string          `json:"tool_call_id"`
		ToolCalls        []ToolCall      `json:"tool_calls"`
		ContentParts     []ContentPart   `json:"content_parts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.ReasoningContent = raw.ReasoningContent
	m.Name = raw.Name
	m.ToolCallID = raw.ToolCallID
	m.ToolCalls = raw.ToolCalls
	m.Parts = raw.ContentParts

	if len(raw.Content) > 0 {
		var s string
		if err := json.Unmarshal(raw.Content, &s); err == nil {
			m.Content = s
			return nil
		}
		var parts []ContentPart
		if err := json.Unmarshal(raw.Content, &parts); err == nil {
			m.Parts = append(m.Parts, parts...)
			return nil
		}
	}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	type rawMessage struct {
		Role             string     `json:"role"`
		Content          any        `json:"content"`
		ReasoningContent string     `json:"reasoning_content,omitempty"`
		Name             string     `json:"name,omitempty"`
		ToolCallID       string     `json:"tool_call_id,omitempty"`
		ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	}
	raw := rawMessage{
		Role:             m.Role,
		ReasoningContent: m.ReasoningContent,
		Name:             m.Name,
		ToolCallID:       m.ToolCallID,
		ToolCalls:        m.ToolCalls,
	}
	if len(m.Parts) > 0 {
		raw.Content = m.Parts
	} else {
		raw.Content = m.Content
	}
	return json.Marshal(raw)
}

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

func (m Message) HasImages() bool {
	for _, p := range m.Parts {
		if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
			return true
		}
	}
	return false
}
func (m Message) TextContent() string {
	if m.Content != "" {
		return m.Content
	}
	var out string
	for _, p := range m.Parts {
		if p.Type == "text" {
			out += p.Text
		}
	}
	return out
}

// UpstreamState carries provider-native conversation continuity.
// For Gemini Web this maps to ConversationID/ResponseID/ChoiceID so a
// repeated X-Session-ID continues the same upstream conversation instead
// of starting a new one on every request.
type UpstreamState struct {
	ConversationID            string `json:"conversation_id,omitempty"`
	ResponseID                string `json:"response_id,omitempty"`
	ChoiceID                  string `json:"choice_id,omitempty"`
	ChatSessionID             string `json:"chat_session_id,omitempty"`
	InvocationID              int    `json:"invocation_id,omitempty"`
	MessageID                 int64  `json:"message_id,omitempty"`
	ConversationTransferToken string `json:"conversation_transfer_token,omitempty"`
	TurnState                 string `json:"turn_state,omitempty"`
}

func (u UpstreamState) Empty() bool {
	return u.ConversationID == "" && u.ResponseID == "" && u.ChoiceID == "" && u.ChatSessionID == "" && u.InvocationID == 0 && u.MessageID == 0 && u.ConversationTransferToken == "" && u.TurnState == ""
}

type ChatRequest struct {
	Model       string        `json:"model"`
	SessionID   string        `json:"session_id,omitempty"`
	Messages    []Message     `json:"messages"`
	Tools       []Tool        `json:"tools,omitempty"`
	ToolChoice  any           `json:"tool_choice,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
	Temperature *float32      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Upstream    UpstreamState `json:"upstream,omitempty"`
}

func (r ChatRequest) HasImages() bool {
	for _, message := range r.Messages {
		if message.HasImages() {
			return true
		}
	}
	return false
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// EstimateTokens provides a lightweight approximation (~4 characters per token).
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	r := []rune(text)
	tok := len(r) / 4
	if tok == 0 && len(r) > 0 {
		tok = 1
	}
	return tok
}

// ComputeUsage calculates or estimates token usage for chat completions.
func ComputeUsage(messages []Message, tools []Tool, content string, reasoning string, toolCalls []ToolCall, existing *Usage) Usage {
	if existing != nil && existing.TotalTokens > 0 {
		return *existing
	}
	promptChars := 0
	for _, m := range messages {
		promptChars += len([]rune(m.Content)) + len([]rune(m.ReasoningContent)) + 4
		for _, p := range m.Parts {
			promptChars += len([]rune(p.Text))
		}
	}
	for _, t := range tools {
		promptChars += len([]rune(t.Function.Name)) + len([]rune(t.Function.Description)) + 10
	}
	compChars := len([]rune(content)) + len([]rune(reasoning))
	for _, tc := range toolCalls {
		compChars += len([]rune(tc.Function.Name)) + len([]rune(tc.Function.Arguments))
	}

	pTokens := (promptChars + 3) / 4
	if pTokens == 0 && len(messages) > 0 {
		pTokens = 1
	}
	cTokens := (compChars + 3) / 4
	if cTokens == 0 && (len(content) > 0 || len(reasoning) > 0 || len(toolCalls) > 0) {
		cTokens = 1
	}
	return Usage{
		PromptTokens:     pTokens,
		CompletionTokens: cTokens,
		TotalTokens:      pTokens + cTokens,
	}
}

type ChatResponse struct {
	ID               string        `json:"id"`
	Model            string        `json:"model"`
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall    `json:"tool_calls,omitempty"`
	FinishReason     string        `json:"finish_reason,omitempty"`
	Usage            Usage         `json:"usage"`
	Upstream         UpstreamState `json:"upstream,omitempty"`
}

type StreamChunk struct {
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall    `json:"tool_calls,omitempty"`
	FinishReason     string        `json:"finish_reason,omitempty"`
	Done             bool          `json:"done"`
	Usage            *Usage        `json:"usage,omitempty"`
	Err              error         `json:"err,omitempty"`
	Upstream         UpstreamState `json:"upstream,omitempty"`
}

type SpeechRequest struct {
	Model       string `json:"model"`
	Input       string `json:"input"`
	Voice       string `json:"voice"`
	ResponseFmt string `json:"response_format,omitempty"`
}

type ImageRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	Size      string `json:"size,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type ImageResponse struct {
	URL  string `json:"url,omitempty"`
	Data []byte `json:"-"`
	MIME string `json:"-"`
}

type Adapter interface {
	Name() string
	Capabilities() []Capability
	Chat(ctx context.Context, req ChatRequest, credentialSecret []byte) (ChatResponse, error)
	StreamChat(ctx context.Context, req ChatRequest, credentialSecret []byte) (<-chan StreamChunk, error)
	Speech(ctx context.Context, req SpeechRequest, credentialSecret []byte) ([]byte, string, error)
	Image(ctx context.Context, req ImageRequest, credentialSecret []byte) (ImageResponse, error)
}

type Result struct {
	Provider     string
	Credential   string
	StartedAt    time.Time
	FinishedAt   time.Time
	CanonicalErr string
}
