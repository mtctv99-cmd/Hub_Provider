package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"sever_provider/internal/provider"
)

// DiagnoseEmptyResponse builds an empty_response error that distinguishes an
// upstream that produced no text from a payload the parser could not extract.
// It only reports safe metadata (lengths, frame and candidate presence) and
// never cookies, tokens, prompts, or chat text.
func DiagnoseEmptyResponse(raw string) error {
	rawLen := len(raw)
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == ")]}'" {
		return provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, fmt.Sprintf("gemini upstream returned empty payload (raw_len=%d)", rawLen))
	}
	hasFrames := strings.Contains(raw, `"wrb.fr"`)
	if !hasFrames {
		return provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, fmt.Sprintf("gemini response had no wrb.fr candidate frames (raw_len=%d)", rawLen))
	}
	return provider.NewEmptyResponseError(provider.EmptyReasonParserFailed, fmt.Sprintf("gemini payload contained wrb.fr frames (raw_len=%d) but parser extracted no text", rawLen))
}

// validStructuredID only accepts IDs coming from JSON wrb.fr frames.
// ponytail: keep bounds tight; widen only if upstream changes ID format.
func validConversationID(s string) bool {
	if !strings.HasPrefix(s, "c_") || len(s) < 3 || len(s) > 128 {
		return false
	}
	for _, c := range s[2:] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validResponseID(s string) bool {
	if !strings.HasPrefix(s, "r_") || len(s) < 3 || len(s) > 128 {
		return false
	}
	for _, c := range s[2:] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validChoiceID(s string) bool {
	if !strings.HasPrefix(s, "rc_") || len(s) < 4 || len(s) > 128 {
		return false
	}
	for _, c := range s[3:] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ExtractResponseLine parses one newline-delimited Gemini wrb.fr snapshot.
// Gemini sends cumulative text snapshots; callers can diff successive results.
func ExtractResponseLine(line string) (string, ConversationMetadata, bool) {
	if !strings.Contains(line, `"wrb.fr"`) {
		return "", ConversationMetadata{}, false
	}
	var outer []any
	if json.Unmarshal([]byte(strings.TrimSpace(line)), &outer) != nil || len(outer) == 0 {
		return "", ConversationMetadata{}, false
	}
	row, ok := outer[0].([]any)
	if !ok || len(row) < 3 {
		return "", ConversationMetadata{}, false
	}
	innerText, ok := row[2].(string)
	if !ok {
		return "", ConversationMetadata{}, false
	}
	var inner []any
	if json.Unmarshal([]byte(innerText), &inner) != nil {
		return "", ConversationMetadata{}, false
	}
	meta := ConversationMetadata{}
	if len(inner) > 1 {
		if ids, ok := inner[1].([]any); ok && len(ids) >= 2 {
			if s, _ := ids[0].(string); validConversationID(s) {
				meta.ConversationID = s
			}
			if s, _ := ids[1].(string); validResponseID(s) {
				meta.ResponseID = s
			}
		}
	}
	if len(inner) <= 4 {
		return "", meta, false
	}
	candidates, ok := inner[4].([]any)
	if !ok || len(candidates) == 0 {
		return "", meta, false
	}
	candidate, ok := candidates[0].([]any)
	if !ok || len(candidate) <= 1 {
		return "", meta, false
	}
	if len(candidate) > 0 {
		if s, _ := candidate[0].(string); validChoiceID(s) {
			meta.ChoiceID = s
		}
	}
	var b strings.Builder
	if texts, ok := candidate[1].([]any); ok {
		for _, value := range texts {
			if s, ok := value.(string); ok {
				b.WriteString(s)
			}
		}
	} else if s, ok := candidate[1].(string); ok {
		b.WriteString(s)
	} else {
		return "", meta, false
	}
	return cleanText(b.String()), meta, true
}

func ExtractResponseText(raw string) string {
	best := ""
	for _, line := range strings.Split(raw, "\n") {
		text, _, ok := ExtractResponseLine(line)
		if ok && len(text) > len(best) {
			best = text
		}
	}
	if best != "" {
		return best
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, `"wrb.fr"`) {
			continue
		}
		var outer []any
		if json.Unmarshal([]byte(line), &outer) != nil || len(outer) == 0 {
			continue
		}
		row, ok := outer[0].([]any)
		if !ok || len(row) < 3 {
			continue
		}
		innerText, ok := row[2].(string)
		if !ok {
			continue
		}
		var inner []any
		if json.Unmarshal([]byte(innerText), &inner) != nil || len(inner) <= 4 {
			continue
		}
		candidates, ok := inner[4].([]any)
		if !ok || len(candidates) == 0 {
			continue
		}
		candidate, ok := candidates[0].([]any)
		if !ok || len(candidate) <= 1 {
			continue
		}
		var b strings.Builder
		if texts, ok := candidate[1].([]any); ok {
			for _, value := range texts {
				if s, ok := value.(string); ok {
					b.WriteString(s)
				}
			}
		} else if s, ok := candidate[1].(string); ok {
			b.WriteString(s)
		}
		if len(b.String()) > len(best) {
			best = b.String()
		}
	}
	return cleanText(best)
}

func ExtractConversationMetadata(raw string) ConversationMetadata {
	meta := ConversationMetadata{}
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, `"wrb.fr"`) {
			continue
		}
		var outer []any
		if json.Unmarshal([]byte(line), &outer) != nil || len(outer) == 0 {
			continue
		}
		row, ok := outer[0].([]any)
		if !ok || len(row) < 3 {
			continue
		}
		innerText, ok := row[2].(string)
		if !ok {
			continue
		}
		var inner []any
		if json.Unmarshal([]byte(innerText), &inner) != nil {
			continue
		}
		if len(inner) > 1 {
			if ids, ok := inner[1].([]any); ok && len(ids) >= 2 {
				if s, _ := ids[0].(string); validConversationID(s) {
					meta.ConversationID = s
				}
				if s, _ := ids[1].(string); validResponseID(s) {
					meta.ResponseID = s
				}
			}
		}
		if len(inner) > 4 {
			if candidates, ok := inner[4].([]any); ok && len(candidates) > 0 {
				if first, ok := candidates[0].([]any); ok && len(first) > 0 {
					if s, _ := first[0].(string); validChoiceID(s) {
						meta.ChoiceID = s
					}
				}
			}
		}
		if meta.ConversationID != "" && meta.ResponseID != "" && meta.ChoiceID != "" {
			return meta
		}
	}
	return meta
}

func cleanText(value string) string {
	// Keep cleaning deliberately conservative; provider-specific code cards are removed by marker.
	if idx := strings.Index(value, "```python?code_"); idx >= 0 {
		value = value[:idx]
	}
	value = strings.ReplaceAll(value, "http://googleusercontent.com/card_content/", "")
	return strings.TrimSpace(value)
}
