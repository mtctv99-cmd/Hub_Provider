package gemini

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"sever_provider/internal/provider"
)

var (
	// imageGenerationMarker matches the text token Gemini emits next to a
	// generated image. It can be URI-escaped inside JSON payloads.
	imageGenerationMarker = regexp.MustCompile(`https?:(?:\\?/){2}googleusercontent\.com/image_generation_content/[^\s"'\\]+`)
	imageTagMarker        = regexp.MustCompile(`<Image[^>]*>`)
	// citationMarker is the web-grounding citation Gemini appends, e.g. "[cite: 1]".
	citationMarker = regexp.MustCompile(`\s*\[cite:\s*[0-9, ]+\]`)
	// followUpMarker starts Gemini's suggested-reply UI block, which some
	// session-continuation responses append after the answer. It always trails
	// the answer, so everything from the tag onward is dropped.
	followUpMarker = regexp.MustCompile(`(?s)\s*<FollowUp\b.*$`)
)

// hasImageGenerationMarker reports whether a raw Gemini RPC payload carries
// generated-image content (text marker, <Image/> tag, or candidate[12] attachment).
func hasImageGenerationMarker(raw string) bool {
	if imageGenerationMarker.MatchString(raw) || imageTagMarker.MatchString(raw) {
		return true
	}
	_, err := extractGeneratedImageURL(raw)
	return err == nil
}

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
	text, _, meta, hasFrame := parseFrame(line)
	return text, meta, hasFrame && text != ""
}

// parseFrame parses one wrb.fr snapshot into its answer text, thinking snapshot
// and ids. text and thoughts are independent: a frame that only carries the
// thinking chain (no answer yet) still reports hasFrame, so callers can stream it.
func parseFrame(line string) (text string, thoughts string, meta ConversationMetadata, hasFrame bool) {
	if !strings.Contains(line, `"wrb.fr"`) {
		return "", "", ConversationMetadata{}, false
	}
	var outer []any
	if json.Unmarshal([]byte(strings.TrimSpace(line)), &outer) != nil || len(outer) == 0 {
		return "", "", ConversationMetadata{}, false
	}
	row, ok := outer[0].([]any)
	if !ok || len(row) < 3 {
		return "", "", ConversationMetadata{}, false
	}
	innerText, ok := row[2].(string)
	if !ok {
		return "", "", ConversationMetadata{}, false
	}
	var inner []any
	if json.Unmarshal([]byte(innerText), &inner) != nil {
		return "", "", ConversationMetadata{}, false
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
	if len(inner) <= 4 {
		return "", "", meta, false
	}
	candidates, ok := inner[4].([]any)
	if !ok || len(candidates) == 0 {
		return "", "", meta, false
	}
	candidate, ok := candidates[0].([]any)
	if !ok || len(candidate) == 0 {
		return "", "", meta, false
	}
	if s, _ := candidate[0].(string); validChoiceID(s) {
		meta.ChoiceID = s
	}
	// candidate[1] carries the answer text; a frame may arrive with only the
	// thinking chain, so a missing or mistyped text slot yields empty text
	// instead of rejecting the whole frame.
	var b strings.Builder
	if len(candidate) > 1 {
		switch texts := candidate[1].(type) {
		case []any:
			for _, value := range texts {
				if s, ok := value.(string); ok {
					b.WriteString(s)
				}
			}
		case string:
			b.WriteString(texts)
		}
	}
	return cleanText(b.String()), candidateThoughts(candidate), meta, true
}

// candidateThoughts reads the cumulative "Show thinking" snapshot Gemini keeps
// in candidate[37][0][0]. Every level is type- and length-checked: upstream only
// emits this slot for thinking-capable models, and a missing/short slot must
// yield "" rather than panic inside the stream goroutine.
func candidateThoughts(candidate []any) string {
	if len(candidate) <= 37 {
		return ""
	}
	outer, ok := candidate[37].([]any)
	if !ok || len(outer) == 0 {
		return ""
	}
	inner, ok := outer[0].([]any)
	if !ok || len(inner) == 0 {
		return ""
	}
	s, ok := inner[0].(string)
	if !ok {
		return ""
	}
	// Run raw: cleanText strips grounding citations and tool fences that may be
	// part of the chain of thought. Only trailing space is trimmed.
	return strings.TrimSpace(s)
}

// ExtractResponseThoughts returns the thinking snapshot belonging to the frame
// that produced the longest answer, so text and reasoning come from the same
// turn. When every frame has empty text it falls back to the longest snapshot.
// ponytail: slot 37 is documented upstream but not live-captured in this repo;
// probe with GEMINI_PROBE_COOKIE_FILE before trusting it in production.
func ExtractResponseThoughts(raw string) string {
	bestText, bestThoughts := -1, ""
	for _, line := range strings.Split(raw, "\n") {
		text, thoughts, _, hasFrame := parseFrame(line)
		if !hasFrame {
			continue
		}
		if len(text) > bestText || (len(text) == bestText && len(thoughts) > len(bestThoughts)) {
			bestText, bestThoughts = len(text), thoughts
		}
	}
	return bestThoughts
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
	// Gemini's code-interpreter emits fences tagged with "?code_" (tool source and
	// stdout). Truncating at the first one used to drop the whole answer when the
	// tool ran before the prose, so strip just those blocks.
	value = stripToolCodeBlocks(value)
	value = strings.ReplaceAll(value, "http://googleusercontent.com/card_content/", "")
	value = imageGenerationMarker.ReplaceAllString(value, "")
	value = imageTagMarker.ReplaceAllString(value, "")
	value = citationMarker.ReplaceAllString(value, "")
	value = followUpMarker.ReplaceAllString(value, "")
	return strings.TrimSpace(value)
}

// stripToolCodeBlocks removes code-interpreter fences (```python?code_reference…,
// ```text?code_stdout…) and keeps the prose before and after them. Ordinary
// fenced code blocks have no "?code_" tag and are preserved untouched.
func stripToolCodeBlocks(value string) string {
	const fence = "```"
	if !strings.Contains(value, "?code_") {
		return value
	}
	var out strings.Builder
	rest := value
	for {
		idx := strings.Index(rest, "?code_")
		if idx < 0 {
			out.WriteString(rest)
			break
		}
		open := strings.LastIndex(rest[:idx], fence)
		if open < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:open])
		closing := strings.Index(rest[idx:], fence)
		if closing < 0 {
			break
		}
		rest = rest[idx+closing+len(fence):]
	}
	return strings.TrimSpace(out.String())
}
