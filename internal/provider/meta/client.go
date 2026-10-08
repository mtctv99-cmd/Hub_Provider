package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sever_provider/internal/provider"
)

const (
	museBaseURL   = "https://muse.ai"
	defaultUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	requestTimout = 180 * time.Second
)

type Client struct {
	http *http.Client
}

func NewClient() *Client {
	return &Client{
		http: &http.Client{
			Timeout: requestTimout,
		},
	}
}

type Reply struct {
	Text      string
	SessionID string
	TurnID    string
}

// SanitizeCredential validates cookies/token format early.
func SanitizeCredential(cookies, token string) error {
	if strings.TrimSpace(cookies) == "" && strings.TrimSpace(token) == "" {
		return fmt.Errorf("meta cookies or token is required")
	}
	return nil
}

func buildHeaders(cookies, token string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", defaultUA)
	h.Set("Accept", "application/json, text/event-stream")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", museBaseURL)
	h.Set("Referer", museBaseURL+"/")
	if cookies != "" {
		h.Set("Cookie", cookies)
	}
	if token != "" {
		t := strings.TrimSpace(token)
		t = strings.TrimPrefix(t, "Bearer ")
		h.Set("Authorization", "Bearer "+t)
	}
	return h
}

func joinMessages(req provider.ChatRequest) string {
	var parts []string
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		if role == "system" {
			parts = append(parts, "[system] "+m.TextContent())
			continue
		}
		if m.Content != "" {
			parts = append(parts, "["+role+"] "+m.Content)
			continue
		}
		text := m.TextContent()
		if text != "" {
			parts = append(parts, "["+role+"] "+text)
		}
	}
	return strings.Join(parts, "\n")
}

func extractImages(req provider.ChatRequest) []string {
	var out []string
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
				out = append(out, p.ImageURL.URL)
			}
		}
	}
	return out
}

// SendMessage performs a single non-streaming turn.
func (c *Client) SendMessage(ctx context.Context, req provider.ChatRequest, cookies, token string) (Reply, error) {
	if err := SanitizeCredential(cookies, token); err != nil {
		return Reply{}, err
	}

	ch, err := c.StreamMessage(ctx, req, cookies, token)
	if err != nil {
		return Reply{}, err
	}

	var sb strings.Builder
	var sessionID, turnID string
	for chunk := range ch {
		if chunk.Err != nil {
			return Reply{}, chunk.Err
		}
		sb.WriteString(chunk.Content)
		if chunk.Upstream.ChatSessionID != "" {
			sessionID = chunk.Upstream.ChatSessionID
		}
		if chunk.Upstream.TurnState != "" {
			turnID = chunk.Upstream.TurnState
		}
	}

	return Reply{Text: sb.String(), SessionID: sessionID, TurnID: turnID}, nil
}

// StreamMessage issues the chat stream and parses NDJSON/SSE chunks.
func (c *Client) StreamMessage(ctx context.Context, req provider.ChatRequest, cookies, token string) (<-chan provider.StreamChunk, error) {
	if err := SanitizeCredential(cookies, token); err != nil {
		return nil, err
	}

	body, _ := json.Marshal(map[string]any{
		"prompt":     joinMessages(req),
		"model":      req.Model,
		"session_id": sessionIDFor(req),
		"stream":     true,
		"images":     extractImages(req),
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, museBaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header = buildHeaders(cookies, token)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("meta chat request failed: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return nil, fmt.Errorf("meta auth expired (status %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("meta chat request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	out := make(chan provider.StreamChunk, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		parseStreamBody(resp.Body, sessionIDFor(req), out)
	}()

	return out, nil
}

func sessionIDFor(req provider.ChatRequest) string {
	if strings.TrimSpace(req.SessionID) != "" {
		return strings.TrimSpace(req.SessionID)
	}
	if strings.TrimSpace(req.Upstream.ChatSessionID) != "" {
		return strings.TrimSpace(req.Upstream.ChatSessionID)
	}
	return ""
}

// SynthesizeSpeech requests TTS audio from Meta voice pipeline.
func (c *Client) SynthesizeSpeech(ctx context.Context, req provider.SpeechRequest, cookies, token string) ([]byte, string, error) {
	if err := SanitizeCredential(cookies, token); err != nil {
		return nil, "", err
	}

	body, _ := json.Marshal(map[string]any{
		"text":  req.Input,
		"voice": voiceFor(req),
		"model": req.Model,
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, museBaseURL+"/api/voice", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	httpReq.Header = buildHeaders(cookies, token)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("meta speech request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, "", fmt.Errorf("meta auth expired (status %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("meta speech failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 25<<20))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("meta speech returned empty audio")
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "audio/mpeg"
		if len(data) > 4 && string(data[:4]) == "RIFF" {
			mime = "audio/wav"
		}
	}
	return data, mime, nil
}

func voiceFor(req provider.SpeechRequest) string {
	if strings.TrimSpace(req.Voice) != "" {
		return strings.TrimSpace(req.Voice)
	}
	return "avocado_v2:MAI_01"
}

// GenerateImage requests image generation and returns the CDN URL.
func (c *Client) GenerateImage(ctx context.Context, req provider.ImageRequest, cookies, token string) (string, error) {
	if err := SanitizeCredential(cookies, token); err != nil {
		return "", err
	}

	body, _ := json.Marshal(map[string]any{
		"prompt":     req.Prompt,
		"model":      req.Model,
		"session_id": strings.TrimSpace(req.SessionID),
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, museBaseURL+"/api/artifacts", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header = buildHeaders(cookies, token)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("meta image request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("meta auth expired (status %d)", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("meta image failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	imageURL, err := extractImageURL(data)
	if err != nil {
		return "", err
	}
	return imageURL, nil
}

// GenerateVideo requests video generation and returns the CDN URL.
func (c *Client) GenerateVideo(ctx context.Context, req provider.VideoRequest, cookies, token string) (string, error) {
	if err := SanitizeCredential(cookies, token); err != nil {
		return "", err
	}

	body, _ := json.Marshal(map[string]any{
		"prompt":     req.Prompt,
		"model":      req.Model,
		"duration":   req.Duration,
		"session_id": strings.TrimSpace(req.SessionID),
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, museBaseURL+"/api/artifacts", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header = buildHeaders(cookies, token)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("meta video request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("meta auth expired (status %d)", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("meta video failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	videoURL, err := extractVideoURL(data)
	if err != nil {
		return "", err
	}
	return videoURL, nil
}
