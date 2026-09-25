package gateway

import (
	"encoding/json"
	"net/http"

	"sever_provider/internal/provider"
)

type ChatCompletionRequest struct {
	Model       string             `json:"model"`
	Messages    []provider.Message `json:"messages"`
	Tools       []provider.Tool    `json:"tools,omitempty"`
	ToolChoice  any                `json:"tool_choice,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Temperature *float32           `json:"temperature,omitempty"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
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

type AnthropicMessageRequest struct {
	Model     string             `json:"model"`
	Messages  []provider.Message `json:"messages"`
	MaxTokens int                `json:"max_tokens,omitempty"`
	Stream    bool               `json:"stream,omitempty"`
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
	writeJSON(w, status, ErrorEnvelope{Error: APIError{Message: message, Type: typ, Code: code}})
}
func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
}
