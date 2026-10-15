package gemini

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const geminiPayloadSlots = 97

type webModelConfig struct {
	hex  string
	mode int
}

var webModels = map[string]webModelConfig{
	"gemini-3.8-flash":      {hex: "56fdd199312815e2", mode: 1},
	"gemini-3.5-flash-lite": {hex: "cf41b0e0dd7d53e5", mode: 6},
	"gemini-3.1-pro":        {hex: "9d8ca3786ebdfbea", mode: 3},
	"gemini-4-argon":        {hex: "95a4c0abcba24364", mode: 8},
}

type ConversationMetadata struct {
	ConversationID string
	ResponseID     string
	ChoiceID       string
}

type UploadedFile struct {
	ID   string
	Name string
}

func BuildPayload(prompt, modelName, xsrf, conversationUUID string, metadata ConversationMetadata, files []UploadedFile) string {
	return BuildPayloadWithThinking(prompt, modelName, xsrf, conversationUUID, metadata, files, "")
}

func BuildPayloadWithThinking(prompt, modelName, xsrf, conversationUUID string, metadata ConversationMetadata, files []UploadedFile, reasoningEffort string) string {
	inner := make([]any, geminiPayloadSlots)
	if len(files) > 0 {
		refs := make([]any, 0, len(files))
		for _, file := range files {
			refs = append(refs, []any{[]any{file.ID}, file.Name})
		}
		inner[0] = []any{prompt, 0, nil, refs, nil, nil, 0}
		inner[18] = 1
	} else {
		inner[0] = []any{prompt}
		inner[18] = 0
	}
	inner[1] = []any{"en"}
	inner[2] = []any{metadata.ConversationID, metadata.ResponseID, metadata.ChoiceID, nil, nil, nil, nil, nil, nil, ""}
	inner[3] = modelName
	inner[6] = []any{1}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []any{[]any{0}}
	inner[27] = 1
	inner[30] = []any{4}
	inner[41] = []any{1}
	inner[53] = 0
	if conversationUUID == "" {
		conversationUUID = newUUID()
	}
	inner[59] = conversationUUID
	inner[61] = []any{}
	inner[68] = 2
	config := webModels[modelName]
	if config.mode > 0 {
		inner[79] = config.mode
	}
	think, err := thinkingLevel(reasoningEffort)
	if err != nil {
		// Keep the legacy payload helper source-compatible; request paths validate
		// the value before building the payload.
		think = 1
	}
	inner[80] = think

	innerJSON, _ := json.Marshal(inner)
	outerJSON, _ := json.Marshal([]any{nil, string(innerJSON)})
	values := url.Values{}
	values.Set("f.req", string(outerJSON))
	if xsrf != "" {
		values.Set("at", xsrf)
	}
	return values.Encode()
}

func newUUID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return hex.EncodeToString(buf[:4]) + "-" + hex.EncodeToString(buf[4:6]) + "-" + hex.EncodeToString(buf[6:8]) + "-" + hex.EncodeToString(buf[8:10]) + "-" + hex.EncodeToString(buf[10:])
}

func modelName(model string) string {
	if model == "" {
		return "gemini-3.8-flash"
	}
	return model
}

func chatRPCHeader(model, reasoningEffort string) (string, error) {
	config, ok := webModels[modelName(model)]
	if !ok {
		return "", fmt.Errorf("unsupported Gemini Web model %q", model)
	}
	think, err := thinkingLevel(reasoningEffort)
	if err != nil {
		return "", err
	}
	fields := make([]any, 17)
	fields[0] = 1
	fields[4] = config.hex
	fields[7] = 0
	fields[8] = []int{4, 5, 6, 8}
	fields[11] = 1
	fields[14] = config.mode
	fields[15] = think
	fields[16] = newUUID()
	b, err := json.Marshal(fields)
	return string(b), err
}

// Gemini Web currently exposes standard and extended thinking in its request
// contract. Low and medium use the standard/default setting; high selects
// extended thinking. The public API's three distinct budgets are not available
// through this reverse-engineered web endpoint.
func thinkingLevel(effort string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "low", "medium":
		return 1, nil
	case "high":
		return 2, nil
	default:
		return 0, fmt.Errorf("unsupported Gemini reasoning_effort %q; expected low, medium, or high", effort)
	}
}
