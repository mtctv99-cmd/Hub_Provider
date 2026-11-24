package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"sever_provider/internal/provider"
)

// maxResponseSchemaBytes caps the schema copied into the prompt; a huge schema
// would blow up every request's token budget.
const maxResponseSchemaBytes = 8192

// errStructuredTruncated marks a reply cut by max_tokens/stop before it became
// valid JSON, so the caller can fail instead of returning a broken document.
var errStructuredTruncated = errors.New("reply was cut before it was valid JSON")

// structuredFormat is the normalised, validated response_format.
type structuredFormat struct {
	kind   string         // "", "json_object", "json_schema"
	schema map[string]any // set only for json_schema
}

// resolveResponseFormat validates the client-supplied response_format. A
// non-zero status means the request must be rejected with that error.
func resolveResponseFormat(rf *ResponseFormat, hasTools bool) (format structuredFormat, status int, message, code string) {
	if rf == nil {
		return format, 0, "", ""
	}
	kind := strings.ToLower(strings.TrimSpace(rf.Type))
	switch kind {
	case "", "text":
		// OpenAI treats "text" as "no structured output"; keep it a no-op.
		return format, 0, "", ""
	case "json_object":
		format.kind = kind
	case "json_schema":
		format.kind = kind
		if rf.JSONSchema == nil || len(rf.JSONSchema.Schema) == 0 {
			return format, 400, "response_format.json_schema.schema is required", "invalid_response_format"
		}
		if len(rf.JSONSchema.Schema) > maxResponseSchemaBytes {
			return format, 400, "response_format.json_schema.schema exceeds 8192 bytes", "unsupported_option"
		}
		if err := json.Unmarshal(rf.JSONSchema.Schema, &format.schema); err != nil || format.schema == nil {
			return format, 400, "response_format.json_schema.schema must be a JSON object", "invalid_response_format"
		}
	default:
		return format, 400, fmt.Sprintf("response_format.type %q is not supported", kind), "invalid_response_format"
	}
	if hasTools {
		// The tool prompt and the JSON-only instruction contradict each other.
		return format, 400, "response_format cannot be combined with tools", "unsupported_option"
	}
	return format, 0, "", ""
}

// applyResponseFormat prepends a system message carrying the JSON-only
// instruction. Every web adapter puts system messages into the prompt.
func applyResponseFormat(messages []provider.Message, format structuredFormat) []provider.Message {
	if format.kind == "" {
		return messages
	}
	instruction := "Reply with ONLY one valid JSON value: no markdown fences, no prose before or after."
	if format.kind == "json_schema" {
		if encoded, err := json.Marshal(format.schema); err == nil {
			instruction += "\nThe JSON value MUST conform to this JSON Schema:\n" + string(encoded)
		}
	}
	return append([]provider.Message{{Role: "system", Content: instruction}}, messages...)
}

// schemaRootType returns "array" or "object" only when the schema root says so
// unambiguously. A missing type, a union, $ref or anything else returns "",
// meaning any valid JSON value is accepted.
// ponytail: structural check only; nested properties/$ref/oneOf are not
// enforced until a real validator is added (top-level required is checked).
func schemaRootType(format structuredFormat) string {
	if format.kind == "json_object" {
		return "object"
	}
	if format.schema == nil {
		return ""
	}
	switch typed := format.schema["type"].(type) {
	case string:
		if typed == "array" || typed == "object" {
			return typed
		}
	case []any:
		hasArray, hasObject := false, false
		for _, item := range typed {
			name, _ := item.(string)
			hasArray = hasArray || name == "array"
			hasObject = hasObject || name == "object"
		}
		if hasArray && !hasObject {
			return "array"
		}
		if hasObject && !hasArray {
			return "object"
		}
	}
	return ""
}

// decodeFirstJSONValue accepts a bare JSON document, a fenced document, or a
// document with short prose before/after it. Web models often wrap JSON in
// explanations despite the JSON-only instruction; rejecting those turned normal
// replies into invalid_response_format 502s.
func decodeFirstJSONValue(s string) (any, error) {
	var value any
	if err := json.Unmarshal([]byte(s), &value); err == nil {
		return value, nil
	}
	for i, r := range s {
		if r != '{' && r != '[' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(s[i:]))
		if err := decoder.Decode(&value); err == nil {
			return value, nil
		}
	}
	return nil, errors.New("no JSON value found")
}

// validateStructuredOutput checks that the reply is one JSON value of the
// expected root kind, and for json_schema roots also the top-level `required`
// names. It does not validate the rest of the schema.
func validateStructuredOutput(content string, format structuredFormat) error {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "```") {
		if idx := strings.Index(trimmed, "\n"); idx >= 0 {
			trimmed = strings.TrimSpace(trimmed[idx+1:])
		}
		if idx := strings.LastIndex(trimmed, "```"); idx >= 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
	}
	if trimmed == "" {
		return errors.New("reply was empty")
	}
	value, err := decodeFirstJSONValue(trimmed)
	if err != nil {
		// Only the parser's own message is surfaced, never the reply content.
		return fmt.Errorf("reply is not valid JSON: %w", err)
	}
	if value == nil {
		return errors.New("reply is JSON null")
	}
	switch schemaRootType(format) {
	case "array":
		if _, ok := value.([]any); !ok {
			return errors.New("reply is not a JSON array")
		}
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return errors.New("reply is not a JSON object")
		}
		// Only top-level `required`: a client-typed string must not panic, and
		// nested properties/$ref stay the client's job.
		if names, ok := format.schema["required"].([]any); ok {
			for _, item := range names {
				name, ok := item.(string)
				if !ok || name == "" {
					continue
				}
				if _, present := obj[name]; !present {
					return fmt.Errorf("reply is missing required field %q", name)
				}
			}
		}
	}
	return nil
}

// truncateForPrompt keeps a rejected reply short before it is fed back into the
// retry prompt.
func truncateForPrompt(text string) string {
	runes := []rune(text)
	if len(runes) <= 2000 {
		return text
	}
	return strings.TrimRightFunc(string(runes[:2000]), unicode.IsSpace)
}
