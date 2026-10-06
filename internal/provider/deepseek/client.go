package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sever_provider/internal/provider/deepseek/pow"
)

const (
	DefaultBaseURL           = "https://chat.deepseek.com"
	DefaultCreatePowPath     = "/api/v0/chat/create_pow_challenge"
	DefaultCreateSessionPath = "/api/v0/chat_session/create"
	DefaultCompletionPath    = "/api/v0/chat/completion"

	AndroidUserAgent = "DeepSeek/2.0.4 Android/35"
	ClientPlatform   = "android"
	ClientVersion    = "2.0.4"
	ClientLocale     = "zh_CN"
)

// Client thực hiện các request HTTP tới chat.deepseek.com giả lập Android client.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient khởi tạo Client với baseURL và HTTP client tùy chọn.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: httpClient,
	}
}

// AndroidHeaders trả về map HTTP headers giả lập DeepSeek Android App (API level 35).
func (c *Client) AndroidHeaders(token string) http.Header {
	h := make(http.Header)
	h.Set("User-Agent", AndroidUserAgent)
	h.Set("x-client-platform", ClientPlatform)
	h.Set("x-client-version", ClientVersion)
	h.Set("x-client-locale", ClientLocale)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("accept-charset", "UTF-8")
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

// GetPowChallenge gọi endpoint tạo challenge PoW.
func (c *Client) GetPowChallenge(ctx context.Context, token, targetPath string) (*pow.Challenge, error) {
	if targetPath == "" {
		targetPath = DefaultCompletionPath
	}
	body, err := json.Marshal(map[string]any{
		"target_path": targetPath,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+DefaultCreatePowPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.AndroidHeaders(token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create pow challenge request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read pow challenge body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("create pow challenge HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	challenge, err := pow.ParseChallenge(respBytes)
	if err != nil {
		return nil, fmt.Errorf("parse pow challenge: %w", err)
	}
	return challenge, nil
}

// SolvePow giải PoW challenge và trả về header base64 x-ds-pow-response.
func (c *Client) SolvePow(ctx context.Context, challenge *pow.Challenge) (string, error) {
	if challenge == nil {
		return "", errors.New("nil challenge")
	}
	return pow.SolveAndBuildHeader(ctx, challenge)
}

// CreateSession tạo chat session mới trên DeepSeek Web.
func (c *Client) CreateSession(ctx context.Context, token string) (string, error) {
	body, _ := json.Marshal(map[string]any{"agent": "chat"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+DefaultCreateSessionPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header = c.AndroidHeaders(token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create session request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read create session response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create session HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var parsed map[string]any
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("unmarshal create session JSON: %w", err)
	}

	sessionID := extractSessionID(parsed)
	if sessionID == "" {
		return "", fmt.Errorf("failed to extract chat session id from response: %s", string(respBytes))
	}
	return sessionID, nil
}

func extractSessionID(resp map[string]any) string {
	data, _ := resp["data"].(map[string]any)
	if data == nil {
		data = resp
	}
	bizData, _ := data["biz_data"].(map[string]any)
	if bizData == nil {
		bizData = data
	}

	if id, ok := bizData["id"].(string); ok && strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	if chatSession, ok := bizData["chat_session"].(map[string]any); ok {
		if id, ok := chatSession["id"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

// SendPrompt giải PoW challenge tự động và gửi request completions trả về HTTP response stream.
// Caller có trách nhiệm đóng resp.Body.
func (c *Client) SendPrompt(ctx context.Context, token, chatSessionID, prompt string, thinkingEnabled, searchEnabled bool) (*http.Response, string, error) {
	return c.SendPromptWithParent(ctx, token, chatSessionID, prompt, thinkingEnabled, searchEnabled, 0)
}
func (c *Client) SendPromptWithParent(ctx context.Context, token, chatSessionID, prompt string, thinkingEnabled, searchEnabled bool, parentID int64) (*http.Response, string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, "", errors.New("deepseek: missing authentication token")
	}

	// 1. Tạo session nếu chưa có
	if strings.TrimSpace(chatSessionID) == "" {
		var err error
		chatSessionID, err = c.CreateSession(ctx, token)
		if err != nil {
			return nil, "", fmt.Errorf("deepseek create session: %w", err)
		}
	}

	// 2. Lấy PoW challenge và giải
	challenge, err := c.GetPowChallenge(ctx, token, DefaultCompletionPath)
	if err != nil {
		return nil, "", fmt.Errorf("deepseek get pow challenge: %w", err)
	}
	powHeader, err := c.SolvePow(ctx, challenge)
	if err != nil {
		return nil, "", fmt.Errorf("deepseek solve pow: %w", err)
	}

	// 3. Chuẩn bị payload completion
	payload := map[string]any{
		"chat_session_id":   chatSessionID,
		"parent_message_id": nil,
		"prompt":            prompt,
		"ref_file_ids":      []any{},
		"thinking_enabled":  thinkingEnabled,
		"search_enabled":    searchEnabled,
	}
	if parentID > 0 {
		payload["parent_message_id"] = parentID
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal completion payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+DefaultCompletionPath, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, "", err
	}
	req.Header = c.AndroidHeaders(token)
	req.Header.Set(pow.HeaderName, powHeader)

	// Dùng client streaming (timeout = 0) nếu có
	streamClient := c.HTTPClient
	if streamClient.Timeout > 0 {
		cloned := *streamClient
		cloned.Timeout = 0
		streamClient = &cloned
	}

	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("deepseek completion request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, "", fmt.Errorf("deepseek completion HTTP %d: %s", resp.StatusCode, string(body))
	}

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
		if err != nil {
			return nil, "", err
		}
		var result struct {
			Code int `json:"code"`
			Data struct {
				BizCode int `json:"biz_code"`
				BizData struct {
					IsMuted   int     `json:"is_muted"`
					MuteUntil float64 `json:"mute_until"`
				} `json:"biz_data"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &result) != nil {
			return nil, "", fmt.Errorf("deepseek: unexpected JSON completion")
		}
		if result.Data.BizData.IsMuted != 0 {
			return nil, "", &RateLimitError{Until: time.Unix(int64(result.Data.BizData.MuteUntil), 0)}
		}
		return nil, "", fmt.Errorf("deepseek: completion rejected code=%d biz_code=%d", result.Code, result.Data.BizCode)
	}
	return resp, chatSessionID, nil
}

type RateLimitError struct{ Until time.Time }

func (e *RateLimitError) Error() string         { return "deepseek: rate limited (account muted)" }
func (e *RateLimitError) RetryAfter() time.Time { return e.Until }
