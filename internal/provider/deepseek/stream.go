package deepseek

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"sever_provider/internal/provider"
)

var thinkClosePattern = regexp.MustCompile(`(?i)</\s*think\s*>`)

// StreamParser phân tích SSE event stream từ DeepSeek Web API.
type StreamParser struct {
	ThinkingEnabled bool
	MessageID       int64
	hasContent      bool
	ChatSessionID   string
	currentType     string // "thinking" hoặc "text"
	thinkingDone    bool
}

// NewStreamParser tạo StreamParser mới.
func NewStreamParser(thinkingEnabled bool, chatSessionID string) *StreamParser {
	initialType := "text"
	if thinkingEnabled {
		initialType = "thinking"
	}
	return &StreamParser{
		ThinkingEnabled: thinkingEnabled,
		ChatSessionID:   chatSessionID,
		currentType:     initialType,
	}
}

// ParseStream đọc stream từ reader và bắn ra StreamChunk trên channel.
// ponytail: single reader goroutine; add buffer sharding if SSE throughput exceeds 10MB/s.
func (p *StreamParser) ParseStream(ctx context.Context, r io.Reader) <-chan provider.StreamChunk {
	out := make(chan provider.StreamChunk, 32)
	go func() {
		defer close(out)
		send := func(chunk provider.StreamChunk) bool {
			if chunk.Done && !p.hasContent {
				chunk = provider.StreamChunk{Err: fmt.Errorf("deepseek: empty completion")}
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		scanner := bufio.NewScanner(r)
		// Cho phép buffer lên tới 1MB cho payload lớn
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			if strings.HasPrefix(line, "event:") {
				evt := strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				if evt == "finish" || evt == "close" {
					if !send(provider.StreamChunk{
						Done:         true,
						FinishReason: "stop",
						Upstream:     provider.UpstreamState{ChatSessionID: p.ChatSessionID, MessageID: p.MessageID},
					}) {
						return
					}
					return
				}
				continue
			}

			if !strings.HasPrefix(line, "data:") {
				continue
			}

			dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if dataStr == "[DONE]" {
				if !send(provider.StreamChunk{
					Done:         true,
					FinishReason: "stop",
					Upstream:     provider.UpstreamState{ChatSessionID: p.ChatSessionID, MessageID: p.MessageID},
				}) {
					return
				}
				return
			}

			chunks, done := p.parseDataChunk([]byte(dataStr))
			for _, chunk := range chunks {
				if !send(chunk) {
					return
				}
			}
			if done {
				if !send(provider.StreamChunk{
					Done:         true,
					FinishReason: "stop",
					Upstream:     provider.UpstreamState{ChatSessionID: p.ChatSessionID, MessageID: p.MessageID},
				}) {
					return
				}
				return
			}
		}

		if err := scanner.Err(); err != nil && err != io.EOF {
			if !send(provider.StreamChunk{Err: err}) {
				return
			}
			return
		}

		send(provider.StreamChunk{Err: io.ErrUnexpectedEOF})
	}()
	return out
}

func (p *StreamParser) parseDataChunk(data []byte) ([]provider.StreamChunk, bool) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false
	}

	if v, ok := raw["v"].(map[string]any); ok {
		if response, ok := v["response"].(map[string]any); ok {
			if id, ok := response["message_id"].(float64); ok {
				p.MessageID = int64(id)
			}
		}
		if id, ok := v["response_message_id"].(float64); ok {
			p.MessageID = int64(id)
		}
	}
	path, _ := raw["p"].(string)
	if path == "response/message_id" {
		if id, ok := raw["v"].(float64); ok {
			p.MessageID = int64(id)
		}
	}
	v, hasV := raw["v"]
	if !hasV {
		return nil, false
	}

	// 1. Kiểm tra trạng thái kết thúc
	if p.isFinished(path, v) {
		return nil, true
	}

	// 2. Chuyển đổi trạng thái dựa theo path
	p.updateStateFromPath(path)

	// 3. Phân tích fragments nếu p == "response/fragments" hoặc v chứa response.fragments
	if chunks, done := p.handleFragments(path, raw, v); len(chunks) > 0 || done {
		return chunks, done
	}

	// 4. Nếu v là string trực tiếp (như {"v":"output"} hoặc {"p":"response/fragments/-1/content","v":"..."})
	if text, ok := v.(string); ok && text != "" {
		return p.emitText(text), false
	}

	return nil, false
}

func (p *StreamParser) isFinished(path string, v any) bool {
	if s, ok := v.(string); ok {
		if strings.EqualFold(strings.TrimSpace(s), "FINISHED") {
			return true
		}
	}
	if path == "response/status" || path == "status" {
		if s, ok := v.(string); ok && strings.EqualFold(s, "FINISHED") {
			return true
		}
	}
	if path == "response" {
		if list, ok := v.([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					if m["p"] == "quasi_status" && m["v"] == "FINISHED" {
						return true
					}
					if m["p"] == "status" && m["v"] == "FINISHED" {
						return true
					}
				}
			}
		}
	}
	return false
}

func (p *StreamParser) updateStateFromPath(path string) {
	switch path {
	case "response/content":
		p.currentType = "text"
		p.thinkingDone = true
	case "response/thinking_content":
		if p.ThinkingEnabled && !p.thinkingDone {
			p.currentType = "thinking"
		}
	}
}

func (p *StreamParser) handleFragments(path string, raw map[string]any, v any) ([]provider.StreamChunk, bool) {
	var chunks []provider.StreamChunk

	// Phân tích initial response fragments envelope: {"v": {"response": {"fragments": [...]}}}
	if m, ok := v.(map[string]any); ok {
		if resp, ok := m["response"].(map[string]any); ok {
			if frags, ok := resp["fragments"].([]any); ok {
				for _, f := range frags {
					if fm, ok := f.(map[string]any); ok {
						chunks = append(chunks, p.parseFragmentObj(fm)...)
					}
				}
			}
		}
	}

	// Phân tích append fragment: {"p": "response/fragments", "o": "APPEND", "v": [...]}
	if path == "response/fragments" {
		if frags, ok := v.([]any); ok {
			for _, f := range frags {
				if fm, ok := f.(map[string]any); ok {
					chunks = append(chunks, p.parseFragmentObj(fm)...)
				}
			}
		}
	}

	return chunks, false
}

func (p *StreamParser) parseFragmentObj(fm map[string]any) []provider.StreamChunk {
	t, _ := fm["type"].(string)
	content, _ := fm["content"].(string)

	switch strings.ToUpper(t) {
	case "THINK", "THINKING":
		if p.ThinkingEnabled && !p.thinkingDone {
			p.currentType = "thinking"
		}
	case "RESPONSE":
		p.currentType = "text"
		p.thinkingDone = true
	}

	if content != "" {
		return p.emitText(content)
	}
	return nil
}

func (p *StreamParser) emitText(text string) []provider.StreamChunk {
	if text != "" {
		p.hasContent = true
	}
	if text == "" {
		return nil
	}

	// Nếu đang ở thinking mode và gặp </think>, cắt và chuyển mode
	if p.ThinkingEnabled && p.currentType == "thinking" && !p.thinkingDone {
		loc := thinkClosePattern.FindStringIndex(text)
		if loc != nil {
			before := text[:loc[0]]
			after := text[loc[1]:]
			p.currentType = "text"
			p.thinkingDone = true

			var res []provider.StreamChunk
			if before != "" {
				res = append(res, provider.StreamChunk{
					ReasoningContent: before,
					Upstream:         provider.UpstreamState{ChatSessionID: p.ChatSessionID, MessageID: p.MessageID},
				})
			}
			if after != "" {
				res = append(res, provider.StreamChunk{
					Content:  after,
					Upstream: provider.UpstreamState{ChatSessionID: p.ChatSessionID, MessageID: p.MessageID},
				})
			}
			return res
		}
		return []provider.StreamChunk{{
			ReasoningContent: text,
			Upstream:         provider.UpstreamState{ChatSessionID: p.ChatSessionID, MessageID: p.MessageID},
		}}
	}

	return []provider.StreamChunk{{
		Content:  text,
		Upstream: provider.UpstreamState{ChatSessionID: p.ChatSessionID, MessageID: p.MessageID},
	}}
}

// FormatClaudeBlock bóc tách StreamChunk cho Claude format (thinking block vs text block).
type ClaudeBlockEvent struct {
	Type  string         `json:"type"`
	Index int            `json:"index"`
	Delta map[string]any `json:"delta,omitempty"`
}

// StripThinkingTags loại bỏ các tag <think> và </think> nếu sót lại trong text.
func StripThinkingTags(s string) string {
	s = regexp.MustCompile(`(?i)<\s*think\s*>`).ReplaceAllString(s, "")
	s = thinkClosePattern.ReplaceAllString(s, "")
	return s
}
