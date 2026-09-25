package gemini

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
)

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
	inner := make([]any, 69)
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
