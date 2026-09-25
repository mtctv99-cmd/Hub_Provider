package gemini

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	conversationRE = regexp.MustCompile(`\bc_[A-Za-z0-9_-]+\b`)
	responseRE     = regexp.MustCompile(`\br_[A-Za-z0-9_-]+\b`)
	choiceRE       = regexp.MustCompile(`\brc_[A-Za-z0-9_-]+\b`)
)

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
			meta.ConversationID, _ = ids[0].(string)
			meta.ResponseID, _ = ids[1].(string)
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
		meta.ChoiceID, _ = candidate[0].(string)
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
				meta.ConversationID, _ = ids[0].(string)
				meta.ResponseID, _ = ids[1].(string)
			}
		}
		if len(inner) > 4 {
			if candidates, ok := inner[4].([]any); ok && len(candidates) > 0 {
				if first, ok := candidates[0].([]any); ok && len(first) > 0 {
					meta.ChoiceID, _ = first[0].(string)
				}
			}
		}
		if meta.ConversationID != "" && meta.ResponseID != "" && meta.ChoiceID != "" {
			return meta
		}
	}
	if meta.ConversationID == "" {
		if m := conversationRE.FindString(raw); m != "" {
			meta.ConversationID = m
		}
	}
	if meta.ResponseID == "" {
		if m := responseRE.FindString(raw); m != "" {
			meta.ResponseID = m
		}
	}
	if meta.ChoiceID == "" {
		if m := choiceRE.FindString(raw); m != "" {
			meta.ChoiceID = m
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
