package gateway

import (
	"encoding/json"
	"net/http"

	"sever_provider/internal/provider"
)

type ChatCompletionRequest struct {
	Model           string             `json:"model"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
	Messages        []provider.Message `json:"messages"`
	Tools           []provider.Tool    `json:"tools,omitempty"`
	ToolChoice      any                `json:"tool_choice,omitempty"`
	Stream          bool               `json:"stream,omitempty"`
	StreamOptions   *StreamOptions     `json:"stream_options,omitempty"`
	Temperature     *float32           `json:"temperature,omitempty"`
	MaxTokens       int                `json:"max_tokens,omitempty"`
	Stop            []string           `json:"stop,omitempty"`
	N               int                `json:"n,omitempty"`
	Modalities      []string           `json:"modalities,omitempty"`
	ImageGeneration bool               `json:"image_generation,omitempty"`
	ResponseFormat  *ResponseFormat    `json:"response_format,omitempty"`
	// Logprobs/TopLogprobs/ParallelToolCalls are declared only so the gateway can
	// reject the values it cannot honour. false/0 mean "not requested", so only
	// the pointer on ParallelToolCalls is needed to catch an explicit false.
	Logprobs          bool  `json:"logprobs,omitempty"`
	TopLogprobs       int   `json:"top_logprobs,omitempty"`
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// ResponseFormat mirrors OpenAI's response_format. Web upstreams have no native
// structured output, so the gateway injects a prompt instruction and validates
// the reply itself (see docs/response-format.md for the limits).
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

type JSONSchemaSpec struct {
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema"`
	// Strict is accepted for OpenAI compatibility and has no effect: the web
	// upstreams cannot enforce schemas natively.
	Strict bool `json:"strict,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ChatCompletionResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []Choice       `json:"choices"`
	Usage   provider.Usage `json:"usage,omitempty"`
}

type Choice struct {
	Index        int               `json:"index"`
	Message      *provider.Message `json:"message,omitempty"`
	Delta        *provider.Message `json:"delta,omitempty"`
	FinishReason *string           `json:"finish_reason"`
}

type ImageGenerationRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	N              int    `json:"n,omitempty"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`
	Style          string `json:"style,omitempty"`
	Quality        string `json:"quality,omitempty"`
}

type VideoGenerationRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	Duration  int    `json:"duration,omitempty"`
	Size      string `json:"size,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type AnthropicMessageRequest struct {
	Model           string               `json:"model"`
	ReasoningEffort string               `json:"reasoning_effort,omitempty"`
	System          any                  `json:"system,omitempty"`
	Messages        anthropicMessages    `json:"messages"`
	Tools           []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice      *AnthropicToolChoice `json:"tool_choice,omitempty"`
	MaxTokens       int                  `json:"max_tokens,omitempty"`
	StopSequences   []string             `json:"stop_sequences,omitempty"`
	Stream          bool                 `json:"stream,omitempty"`
}

type TTSGenerateRequest struct {
	Text     string  `json:"text"`
	VoiceID  string  `json:"voice_id"`
	Engine   string  `json:"engine,omitempty"`
	Language string  `json:"language,omitempty"`
	Speed    float64 `json:"speed,omitempty"`
}

type AsyncJobRequest struct {
	Prompt      string         `json:"prompt"`
	Model       string         `json:"model,omitempty"`
	Temperature *float32       `json:"temperature,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	ExtraParams map[string]any `json:"extra_params,omitempty"`
}

type ErrorEnvelope struct {
	Error APIError `json:"error"`
}
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message, typ, code string) {
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	writeJSON(w, status, ErrorEnvelope{Error: APIError{Message: message, Type: typ, Code: code}})
}
func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
}
