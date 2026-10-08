package meta

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"sever_provider/internal/provider"
)

var cdnImageURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+\.(?:png|jpg|jpeg|webp)`)
var cdnVideoURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+\.(?:mp4|webm|mov)`)

func extractImageURL(data []byte) (string, error) {
	var payload struct {
		URL   string   `json:"url"`
		URLs  []string `json:"urls"`
		Image string   `json:"image"`
		Data  struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) == nil {
		if payload.URL != "" {
			return payload.URL, nil
		}
		if len(payload.URLs) > 0 && payload.URLs[0] != "" {
			return payload.URLs[0], nil
		}
		if payload.Image != "" {
			return payload.Image, nil
		}
		if payload.Data.URL != "" {
			return payload.Data.URL, nil
		}
	}
	str := string(data)
		if m := cdnImageURLPattern.FindString(str); m != "" {
			return m, nil
		}
		return "", errors.New("no image url found in meta response")
	}

func extractVideoURL(data []byte) (string, error) {
	var payload struct {
		URL   string   `json:"url"`
		URLs  []string `json:"urls"`
		Video string   `json:"video"`
		Data  struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) == nil {
		if payload.URL != "" {
			return payload.URL, nil
		}
		if len(payload.URLs) > 0 && payload.URLs[0] != "" {
			return payload.URLs[0], nil
		}
		if payload.Video != "" {
			return payload.Video, nil
		}
		if payload.Data.URL != "" {
			return payload.Data.URL, nil
		}
	}
	str := string(data)
	if m := cdnVideoURLPattern.FindString(str); m != "" {
		return m, nil
	}
	return "", errors.New("no video url found in meta response")
}

func parseStreamBody(r io.Reader, sessionID string, out chan<- provider.StreamChunk) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	var lastTurnID string

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "[DONE]" {
			out <- provider.StreamChunk{
				Done: true,
				Upstream: provider.UpstreamState{
					ChatSessionID: sessionID,
					TurnState:     lastTurnID,
				},
			}
			return
		}

		var event struct {
			Text      string `json:"text"`
			Content   string `json:"content"`
			Delta     string `json:"delta"`
			Chunk     string `json:"chunk"`
			SessionID string `json:"session_id"`
			TurnID    string `json:"turn_id"`
			Done      bool   `json:"done"`
		}

		if err := json.Unmarshal([]byte(line), &event); err == nil {
			text := event.Text
			if text == "" {
				text = event.Content
			}
			if text == "" {
				text = event.Delta
			}
			if text == "" {
				text = event.Chunk
			}
			if event.SessionID != "" {
				sessionID = event.SessionID
			}
			if event.TurnID != "" {
				lastTurnID = event.TurnID
			}
			if text != "" || event.Done {
				out <- provider.StreamChunk{
					Content: text,
					Done:    event.Done,
					Upstream: provider.UpstreamState{
						ChatSessionID: sessionID,
						TurnState:     lastTurnID,
					},
				}
			}
			if event.Done {
				return
			}
			continue
		}

		// Fallback nếu dòng là raw text
		out <- provider.StreamChunk{
			Content: line,
			Upstream: provider.UpstreamState{
				ChatSessionID: sessionID,
				TurnState:     lastTurnID,
			},
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		out <- provider.StreamChunk{Err: err}
	}
}
