package gemini

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"sever_provider/internal/provider"
)

func (a *Adapter) Name() string { return "gemini_web" }
func (a *Adapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityVision, provider.CapabilityImage}
}
func (a *Adapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (a *Adapter) Chat(ctx context.Context, req provider.ChatRequest, secret []byte) (provider.ChatResponse, error) {
	prompt := formatPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	var cred struct {
		Cookie string `json:"cookie"`
	}
	_ = json.Unmarshal(secret, &cred)
	rawCookie := cred.Cookie
	if rawCookie == "" && len(strings.TrimSpace(string(secret))) > 0 {
		var s string
		if json.Unmarshal(secret, &s) == nil {
			rawCookie = s
		} else {
			rawCookie = string(secret)
		}
	}
	var cookie string
	if strings.TrimSpace(rawCookie) != "" {
		clean, err := SanitizeCookie(rawCookie)
		if err != nil {
			return provider.ChatResponse{}, fmt.Errorf("gemini cookie: %w", err)
		}
		cookie = clean
	}
	a.syncTokenBaseURL()
	tokens, err := a.Tokens.Get(ctx, cookie)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	var files []UploadedFile
	if req.HasImages() {
		files, err = a.ExtractAndUploadFiles(ctx, req.Messages, cookie, tokens)
		if err != nil {
			return provider.ChatResponse{}, fmt.Errorf("gemini vision: %w", err)
		}
	}
	metadata := ConversationMetadata{ConversationID: req.Upstream.ConversationID, ResponseID: req.Upstream.ResponseID, ChoiceID: req.Upstream.ChoiceID}
	text, returned, err := a.GenerateWithFiles(ctx, prompt, req.Model, cookie, metadata, files)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	cleanText := text
	var toolCalls []provider.ToolCall
	finishReason := "stop"
	if len(req.Tools) > 0 && req.ToolChoice != "none" {
		cleanText, toolCalls = ParseToolCalls(text)
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
	}
	if strings.TrimSpace(cleanText) == "" && len(toolCalls) == 0 {
		return provider.ChatResponse{}, provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "gemini chat returned empty content with no tool calls")
	}
	return provider.ChatResponse{
		ID:           "gemini-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Model:        req.Model,
		Content:      cleanText,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Upstream: provider.UpstreamState{
			ConversationID: returned.ConversationID,
			ResponseID:     returned.ResponseID,
			ChoiceID:       returned.ChoiceID,
		},
	}, nil
}
func (a *Adapter) StreamChat(ctx context.Context, req provider.ChatRequest, secret []byte) (<-chan provider.StreamChunk, error) {
	prompt := formatPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	var cred struct {
		Cookie string `json:"cookie"`
	}
	_ = json.Unmarshal(secret, &cred)
	rawCookie := cred.Cookie
	if rawCookie == "" && len(strings.TrimSpace(string(secret))) > 0 {
		var s string
		if json.Unmarshal(secret, &s) == nil {
			rawCookie = s
		} else {
			rawCookie = string(secret)
		}
	}
	var cookie string
	if strings.TrimSpace(rawCookie) != "" {
		clean, err := SanitizeCookie(rawCookie)
		if err != nil {
			return nil, fmt.Errorf("gemini cookie: %w", err)
		}
		cookie = clean
	}
	model := req.Model
	if model == "" {
		model = a.DefaultModel
	}
	a.syncTokenBaseURL()
	tokens, err := a.Tokens.Get(ctx, cookie)
	if err != nil {
		return nil, err
	}
	if tokens.XSRF == "" {
		return nil, fmt.Errorf("gemini page did not expose XSRF token")
	}
	var files []UploadedFile
	if req.HasImages() {
		var upErr error
		files, upErr = a.ExtractAndUploadFiles(ctx, req.Messages, cookie, tokens)
		if upErr != nil {
			return nil, fmt.Errorf("gemini vision: %w", upErr)
		}
	}
	metadata := ConversationMetadata{ConversationID: req.Upstream.ConversationID, ResponseID: req.Upstream.ResponseID, ChoiceID: req.Upstream.ChoiceID}
	body := BuildPayload(prompt, modelName(model), tokens.XSRF, "", metadata, files)
	u, _ := url.Parse(a.BaseURL + "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate")
	q := u.Query()
	q.Set("bl", tokens.BL)
	q.Set("hl", "en")
	q.Set("_reqid", strconv.FormatInt(time.Now().Unix()%1000000, 10))
	q.Set("rt", "c")
	if tokens.FSID != "" {
		q.Set("f.sid", tokens.FSID)
	}
	u.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range headers(cookie, SapisidFromCookie(cookie), time.Now()) {
		httpReq.Header[key] = value
	}
	httpReq.Header.Set("x-goog-ext-525001261-jspb", `[1,null,null,null,"71c2d248d3b102ff"]`)

	streamTimeout := streamDeadline(a.Client)
	streamClient := &http.Client{Transport: a.Client.Transport, Timeout: streamTimeout}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("gemini RPC HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	ch := make(chan provider.StreamChunk, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)
		var prevText string
		var lastMeta ConversationMetadata
		sawValidLine := false
		sawValidEnd := false
		for {
			line, readErr := reader.ReadString('\n')
			if line != "" {
				text, meta, ok := ExtractResponseLine(line)
				if ok {
					sawValidLine = true
					if meta.ConversationID != "" {
						lastMeta = meta
					}
					if text != prevText {
						var delta string
						if strings.HasPrefix(text, prevText) {
							delta = text[len(prevText):]
						} else {
							delta = text
						}
						prevText = text
						if strings.TrimSpace(text) != "" {
							sawValidEnd = true
						}
						if delta != "" {
							select {
							case <-ctx.Done():
								ch <- provider.StreamChunk{Err: ctx.Err()}
								return
							case ch <- provider.StreamChunk{Content: delta}:
							}
						}
					}
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					ch <- provider.StreamChunk{Err: readErr}
					return
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					ch <- provider.StreamChunk{Err: ctxErr}
					return
				}
				if !sawValidLine || !sawValidEnd {
					if !sawValidLine {
						ch <- provider.StreamChunk{Err: provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "gemini stream ended without valid response frames")}
					} else {
						ch <- provider.StreamChunk{Err: provider.NewEmptyResponseError(provider.EmptyReasonParserFailed, "gemini stream had valid frames but extracted no end text")}
					}
					return
				}
				break
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			ch <- provider.StreamChunk{Err: ctxErr}
			return
		}
		finishReason := "stop"
		var toolCalls []provider.ToolCall
		if len(req.Tools) > 0 && req.ToolChoice != "none" {
			_, toolCalls = ParseToolCalls(prevText)
			if len(toolCalls) > 0 {
				finishReason = "tool_calls"
			}
		}
		if strings.TrimSpace(prevText) == "" && len(toolCalls) == 0 {
			if !sawValidLine {
				ch <- provider.StreamChunk{Err: provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "gemini stream ended without valid response frames")}
			} else {
				ch <- provider.StreamChunk{Err: provider.NewEmptyResponseError(provider.EmptyReasonParserFailed, "gemini stream had valid frames but extracted no text")}
			}
			return
		}
		ch <- provider.StreamChunk{
			Done:         true,
			ToolCalls:    toolCalls,
			FinishReason: finishReason,
			Upstream: provider.UpstreamState{
				ConversationID: lastMeta.ConversationID,
				ResponseID:     lastMeta.ResponseID,
				ChoiceID:       lastMeta.ChoiceID,
			},
		}
	}()
	return ch, nil
}

type Adapter struct {
	Client        *http.Client
	Tokens        *TokenProvider
	BaseURL       string
	UploadBaseURL string
	DefaultModel  string
}

func New(client *http.Client) *Adapter {
	if client == nil {
		client = &http.Client{Timeout: 180 * time.Second}
	}
	return &Adapter{
		Client:        client,
		Tokens:        NewTokenProvider(client),
		BaseURL:       "https://gemini.google.com",
		UploadBaseURL: "https://push.clients6.google.com",
		DefaultModel:  "gemini-3.8-flash",
	}
}

func (a *Adapter) SetBaseURL(baseURL string) {
	a.BaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if a.Tokens != nil {
		a.Tokens.SetBaseURL(a.BaseURL)
	}
}

// streamDeadline keeps an explicit stream deadline instead of relying only on
// the dispatcher's ctx. ponytail: 120s per-stream ceiling, tune when server models change.
func streamDeadline(client *http.Client) time.Duration {
	if client != nil && client.Timeout > 0 {
		return client.Timeout
	}
	return 120 * time.Second
}

func (a *Adapter) syncTokenBaseURL() {
	if a.Tokens == nil {
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://gemini.google.com"
	}
	if a.Tokens.BaseURL != baseURL {
		a.Tokens.SetBaseURL(baseURL)
	}
}

func (a *Adapter) Generate(ctx context.Context, prompt, model, cookie string, metadata ConversationMetadata) (string, ConversationMetadata, error) {
	return a.GenerateWithFiles(ctx, prompt, model, cookie, metadata, nil)
}

func (a *Adapter) GenerateWithFiles(ctx context.Context, prompt, model, cookie string, metadata ConversationMetadata, files []UploadedFile) (string, ConversationMetadata, error) {
	raw, err := a.generateRaw(ctx, prompt, model, cookie, metadata, files, 0)
	if err != nil {
		return "", ConversationMetadata{}, err
	}
	text := ExtractResponseText(raw)
	if strings.TrimSpace(text) == "" {
		return "", ConversationMetadata{}, DiagnoseEmptyResponse(raw)
	}
	return text, ExtractConversationMetadata(raw), nil
}

// generateRaw shares Gemini's token, payload, and StreamGenerate request path.
// A positive maxBytes bounds responses that need strict structural parsing.
func (a *Adapter) generateRaw(ctx context.Context, prompt, model, cookie string, metadata ConversationMetadata, files []UploadedFile, maxBytes int64) (string, error) {
	if model == "" {
		model = a.DefaultModel
	}
	a.syncTokenBaseURL()
	tokens, err := a.Tokens.Get(ctx, cookie)
	if err != nil {
		return "", err
	}
	if tokens.XSRF == "" {
		return "", fmt.Errorf("gemini page did not expose XSRF token")
	}
	body := BuildPayload(prompt, modelName(model), tokens.XSRF, "", metadata, files)
	u, _ := url.Parse(a.BaseURL + "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate")
	q := u.Query()
	q.Set("bl", tokens.BL)
	q.Set("hl", "en")
	q.Set("_reqid", strconv.FormatInt(time.Now().Unix()%1000000, 10))
	q.Set("rt", "c")
	if tokens.FSID != "" {
		q.Set("f.sid", tokens.FSID)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return "", err
	}
	for key, value := range headers(cookie, SapisidFromCookie(cookie), time.Now()) {
		req.Header[key] = value
	}
	req.Header.Set("x-goog-ext-525001261-jspb", `[1,null,null,null,"71c2d248d3b102ff"]`)
	resp, err := a.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var bodyReader io.Reader = resp.Body
	if maxBytes > 0 {
		bodyReader = io.LimitReader(resp.Body, maxBytes+1)
	}
	raw, err := io.ReadAll(bodyReader)
	if err != nil {
		return "", err
	}
	if maxBytes > 0 && int64(len(raw)) > maxBytes {
		return "", fmt.Errorf("gemini RPC response exceeded bounded maximum of %d bytes", maxBytes)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini RPC HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return string(raw), nil
}

func headers(cookie, sapisid string, now time.Time) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("Origin", "https://gemini.google.com")
	h.Set("Referer", "https://gemini.google.com/app")
	h.Set("X-Same-Domain", "1")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	if cookie != "" {
		h.Set("Cookie", cookie)
	}
	if sapisid != "" {
		h.Set("Authorization", SapisidHash(sapisid, now))
	}
	return h
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func formatPrompt(messages []provider.Message) string {
	if len(messages) == 0 {
		return ""
	}
	if len(messages) == 1 {
		return messages[0].TextContent()
	}
	var b strings.Builder
	for i, m := range messages {
		text := m.TextContent()
		if text == "" {
			continue
		}
		if i == len(messages)-1 {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(text)
		} else {
			role := strings.Title(strings.ToLower(m.Role))
			if role == "" {
				role = "User"
			}
			if role == "Assistant" {
				role = "Model"
			}
			b.WriteString(role)
			b.WriteString(": ")
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ValidateCookie verifies that the provided cookie is valid by attempting to fetch
// fresh page tokens from the Gemini web endpoint without using cache.
func (a *Adapter) ValidateCookie(ctx context.Context, cookie string) error {
	clean, err := SanitizeCookie(cookie)
	if err != nil {
		return err
	}
	a.syncTokenBaseURL()
	if a.Tokens != nil {
		a.Tokens.mu.Lock()
		delete(a.Tokens.cache, clean)
		a.Tokens.mu.Unlock()
	}
	tokens, err := a.Tokens.Get(ctx, clean)
	if err != nil {
		return fmt.Errorf("gemini cookie probe: %w", err)
	}
	if tokens.XSRF == "" || !tokens.Authenticated {
		return ErrCookieInvalidOrExpired
	}
	return nil
}

func ParseToolCalls(text string) (string, []provider.ToolCall) {
	return provider.ParseToolCalls(text)
}

func formatPromptWithTools(messages []provider.Message, tools []provider.Tool, toolChoice any) string {
	return provider.FormatPromptWithTools(messages, tools, toolChoice)
}
