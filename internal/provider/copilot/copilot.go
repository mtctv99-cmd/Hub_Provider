package copilot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/provider"
)

const recordSeparator = "\x1e"

type Account struct {
	ID       string `json:"id"`
	TokenURL string `json:"token_url"`
}

func (a *Account) UnmarshalJSON(data []byte) error {
	type alias Account
	var aux alias
	if err := json.Unmarshal(data, &aux); err == nil && aux.TokenURL != "" {
		*a = Account(aux)
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if v, ok := m["token_url"].(string); ok {
		a.TokenURL = v
	} else if v, ok := m["tokenUrl"].(string); ok {
		a.TokenURL = v
	} else if v, ok := m["TokenURL"].(string); ok {
		a.TokenURL = v
	}
	if v, ok := m["id"].(string); ok {
		a.ID = v
	}
	return nil
}
type Session struct {
	ConversationID            string
	ChatSessionID             string
	InvocationID              int
	TurnCount                 int
	ConversationTransferToken string
	TurnState                 string
}
type Adapter struct {
	HTTPClient *http.Client
	Template   map[string]any
	Origin     string
}

func New(template map[string]any) *Adapter {
	if template == nil {
		template = DefaultTemplate()
	}
	return &Adapter{HTTPClient: &http.Client{Timeout: 60 * time.Second}, Template: template, Origin: "https://m365.cloud.microsoft"}
}

func DefaultTemplate() map[string]any {
	if data, err := os.ReadFile("data/copilot/ws_template.json"); err == nil {
		var tpl map[string]any
		if json.Unmarshal(data, &tpl) == nil {
			return tpl
		}
	}
	return map[string]any{
		"arguments": []any{
			map[string]any{
				"source": "officeweb",
				"optionsSets": []string{
					"search_result_progress_messages_with_search_queries",
					"enable_search_result_progress_messages",
					"update_textdoc_response_after_streaming",
					"deepleo_networking_timeout_10minutes_canmore",
					"cwc_flux_image",
					"cwc_code_interpreter",
					"cwc_code_interpreter_amsfix",
					"cwcfluxgptv",
					"flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch",
					"gptvnorm2048",
					"cwc_code_interpreter_citation_fix",
					"code_interpreter_interactive_charts",
					"cwc_code_interpreter_interactive_charts_inline_image",
					"code_interpreter_matplotlib_patching",
					"cwc_fileupload_odb",
					"update_memory_plugin",
					"add_custom_instructions",
					"cwc_flux_v3",
					"flux_v3_progress_messages",
					"enable_batch_token_processing",
					"enable_gg_gpt",
					"async_client_interaction",
					"enable_inferred_memory_read",
					"flux_v3_references",
					"flux_v3_references_entities",
					"flux_v3_references_ci",
					"add_filestore_filetype",
					"cwc_code_interpreter_citation_sourceannotations",
					"cdxcwc_code_interpreter_hallucinated_url_filter",
					"flux_v3_image_gen_enable_dimensions",
					"flux_v3_image_gen_enable_non_watermarked_storage",
					"flux_v3_image_gen_enable_icon_dimensions",
					"flux_v3_image_gen_enable_system_text_with_params",
					"flux_v3_image_gen_enable_designer_dimensions_meta_prompting_in_system_prompts",
					"flux_v3_image_gen_enable_story",
					"rich_responses",
				},
				"allowedMessageTypes": []string{
					"Chat", "Suggestion", "InternalSearchQuery", "Disengaged",
					"InternalLoaderMessage", "Progress", "GeneratedCode", "RenderCardRequest",
					"AdsQuery", "SemanticSerp", "GenerateContentQuery", "GenerateGraphicArt",
					"SearchQuery", "ConfirmationCard", "AuthError", "DeveloperLogs",
					"TriggerPlugin", "HintInvocation", "MemoryUpdate", "EndOfRequest",
					"TriggerConfirmation", "ResumeInvokeAction", "ResumeUserInputRequest",
					"TriggerUserInputRequest", "EscapeHatch", "TriggerPluginAuth",
					"ResumePluginAuth", "SideBySide", "ReferencesListComplete",
					"SwitchRespondingEndpoint",
				},
				"clientInfo": map[string]any{
					"ProductCategory":        "Chat",
					"clientAppName":          "Office",
					"clientAppType":         "Web",
					"clientEntrypoint":      "mcmcopilot-officeweb",
					"clientPlatform":        "mcmcopilot-web",
					"clientPlatformVersion": "10",
					"deviceOS":              "Windows",
					"deviceType":            "Desktop",
					"productEntryPoint":     "ChatPanel",
				},
				"streamingMode": "ConciseWithPadding",
				"plugins": []any{
					map[string]any{"Id": "BingWebSearch", "Source": "BuiltIn"},
				},
				"isStartOfSession": true,
				"message": map[string]any{
					"author":      "user",
					"inputMethod": "Keyboard",
					"text":        "",
					"messageType": "Chat",
				},
			},
		},
		"invocationId": "0",
		"target":       "chat",
		"type":         4,
	}
}
func (a *Adapter) Name() string { return "copilot_ws" }
func (a *Adapter) Capabilities() []provider.Capability {
	return []provider.Capability{
		provider.CapabilityChat,
		provider.CapabilityStreaming,
		provider.CapabilityImage,
		provider.CapabilityReasoning,
		provider.CapabilityVision,
	}
}
func (a *Adapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (a *Adapter) Image(ctx context.Context, req provider.ImageRequest, credentialSecret []byte) (provider.ImageResponse, error) {
	var account Account
	if err := json.Unmarshal(credentialSecret, &account); err != nil || account.TokenURL == "" {
		return provider.ImageResponse{}, errors.New("copilot credential requires token_url")
	}
	if !IsTokenValid(account.TokenURL, 30) {
		return provider.ImageResponse{}, errors.New("copilot token is expired or invalid")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return provider.ImageResponse{}, errors.New("image prompt is required")
	}
	return a.generateImage(ctx, account, req.Prompt)
}
func (a *Adapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

func IsTokenValid(tokenURL string, minSeconds int64) bool {
	_, err := ValidateTokenURL(tokenURL, minSeconds)
	return err == nil
}

func BuildInvocationURL(tokenURL string, session *Session) (string, string, string, error) {
	u, err := url.Parse(tokenURL)
	if err != nil {
		return "", "", "", err
	}
	conversationID, chatSessionID := randomUUID(), randomHex(16)
	if session != nil {
		if session.ConversationID != "" {
			conversationID = session.ConversationID
		}
		if session.ChatSessionID != "" {
			chatSessionID = session.ChatSessionID
		}
	}
	xSession := chatSessionID
	if len(chatSessionID) >= 32 {
		xSession = chatSessionID[:8] + "-" + chatSessionID[8:12] + "-" + chatSessionID[12:16] + "-" + chatSessionID[16:20] + "-" + chatSessionID[20:32]
	}
	q := u.Query()
	q.Set("chatsessionid", chatSessionID)
	q.Set("XRoutingParameterSessionKey", chatSessionID)
	q.Set("clientrequestid", chatSessionID)
	q.Set("X-SessionId", xSession)
	q.Set("ConversationId", conversationID)
	u.RawQuery = q.Encode()
	return u.String(), conversationID, chatSessionID, nil
}

func socketDeadline(ctx context.Context, defaultTimeout time.Duration) time.Time {
	deadline := time.Now().Add(defaultTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		return d
	}
	return deadline
}

func (a *Adapter) Chat(ctx context.Context, req provider.ChatRequest, credentialSecret []byte) (provider.ChatResponse, error) {
	var account Account
	if err := json.Unmarshal(credentialSecret, &account); err != nil || account.TokenURL == "" {
		return provider.ChatResponse{}, errors.New("copilot credential requires token_url")
	}
	if !IsTokenValid(account.TokenURL, 30) {
		return provider.ChatResponse{}, errors.New("copilot token is expired or invalid")
	}
	sess := sessionFromUpstream(req.Upstream)
	prompt := provider.FormatPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	imageDataURI := extractImageDataURI(ctx, req.Messages)
	full, err := a.generate(ctx, account, &sess, prompt, req.Model, imageDataURI, nil)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	cleanContent := full
	var toolCalls []provider.ToolCall
	finishReason := "stop"
	if len(req.Tools) > 0 && req.ToolChoice != "none" {
		cleanContent, toolCalls = provider.ParseToolCalls(full)
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
	}
	return provider.ChatResponse{
		ID:           "copilot-" + randomHex(8),
		Model:        req.Model,
		Content:      cleanContent,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Upstream:     upstreamFromSession(sess),
	}, nil
}

func (a *Adapter) StreamChat(ctx context.Context, req provider.ChatRequest, credentialSecret []byte) (<-chan provider.StreamChunk, error) {
	var account Account
	if err := json.Unmarshal(credentialSecret, &account); err != nil || account.TokenURL == "" {
		return nil, errors.New("copilot credential requires token_url")
	}
	if !IsTokenValid(account.TokenURL, 30) {
		return nil, errors.New("copilot token is expired or invalid")
	}
	sess := sessionFromUpstream(req.Upstream)
	prompt := provider.FormatPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	imageDataURI := extractImageDataURI(ctx, req.Messages)
	out := make(chan provider.StreamChunk, 16)
	hasTools := len(req.Tools) > 0 && req.ToolChoice != "none"

	go func() {
		defer close(out)
		if !hasTools {
			_, err := a.generate(ctx, account, &sess, prompt, req.Model, imageDataURI, func(delta string) {
				select {
				case out <- provider.StreamChunk{Content: delta}:
				case <-ctx.Done():
				}
			})
			if err != nil {
				out <- provider.StreamChunk{Err: err}
				return
			}
			out <- provider.StreamChunk{Done: true, FinishReason: "stop", Upstream: upstreamFromSession(sess)}
			return
		}

		// Tool calling mode: buffer stream to parse tool call blocks cleanly
		var buf strings.Builder
		_, err := a.generate(ctx, account, &sess, prompt, req.Model, imageDataURI, func(delta string) {
			buf.WriteString(delta)
		})
		if err != nil {
			out <- provider.StreamChunk{Err: err}
			return
		}
		cleanText, toolCalls := provider.ParseToolCalls(buf.String())
		finishReason := "stop"
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
		if cleanText != "" {
			select {
			case out <- provider.StreamChunk{Content: cleanText}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- provider.StreamChunk{
			Done:         true,
			ToolCalls:    toolCalls,
			FinishReason: finishReason,
			Upstream:     upstreamFromSession(sess),
		}:
		case <-ctx.Done():
			return
		}
	}()
	return out, nil
}

func extractImageDataURI(ctx context.Context, messages []provider.Message) string {
	for _, m := range messages {
		for _, p := range m.Parts {
			if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
				raw := strings.TrimSpace(p.ImageURL.URL)
				if strings.HasPrefix(raw, "data:image/") {
					return raw
				}
				if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
					dataURI, err := fetchImageAsDataURI(ctx, raw)
					if err == nil && dataURI != "" {
						return dataURI
					}
				}
			}
		}
	}
	return ""
}

func fetchImageAsDataURI(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch image HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil || len(data) == 0 {
		return "", fmt.Errorf("read image bytes failed: %w", err)
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = http.DetectContentType(data)
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf("data:%s;base64,%s", mime, b64), nil
}

func (a *Adapter) generate(ctx context.Context, account Account, sess *Session, text, model, imageDataURI string, onDelta func(string)) (string, error) {
	wsURL, convID, chatID, err := BuildInvocationURL(account.TokenURL, sess)
	if err != nil {
		return "", err
	}
	if sess != nil {
		sess.ConversationID, sess.ChatSessionID = convID, chatID
	}
	payload := cloneMap(a.Template)
	if payload == nil {
		return "", errors.New("copilot invocation template is required")
	}
	payload["invocationId"] = fmt.Sprintf("%d", sess.InvocationID)
	args, ok := payload["arguments"].([]any)
	if !ok || len(args) == 0 {
		return "", errors.New("copilot template arguments missing")
	}
	arg0, ok := args[0].(map[string]any)
	if !ok {
		return "", errors.New("copilot template argument shape invalid")
	}

	reqID := randomUUID()
	traceID := randomHex(16)

	arg0["clientCorrelationId"] = reqID
	arg0["requestId"] = reqID
	arg0["traceId"] = traceID
	arg0["tone"] = mapTone(model)
	arg0["isStartOfSession"] = sess.InvocationID == 0
	arg0["conversationId"] = convID
	arg0["sessionId"] = chatID
	if sess.ConversationTransferToken != "" {
		arg0["conversationTransferToken"] = sess.ConversationTransferToken
	}
	if sess.TurnState != "" {
		arg0["turnState"] = sess.TurnState
	}
	if clientInfo, ok := arg0["clientInfo"].(map[string]any); ok {
		clientInfo["clientSessionId"] = chatID
	}
	if message, ok := arg0["message"].(map[string]any); ok {
		message["text"] = text
		message["requestId"] = reqID
		if imageDataURI != "" {
			message["imageUrl"] = imageDataURI
			message["originalImageUrl"] = imageDataURI
		}
	}

	data, _ := json.Marshal(payload)
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	header := http.Header{
		"Origin":     []string{a.Origin},
		"User-Agent": []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"},
	}
	ws, _, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return "", errors.New("copilot websocket connection failed")
	}
	defer ws.Close()
	// DialContext only bounds the handshake; plain ReadMessage does not
	// observe ctx. AfterFunc closes the socket on cancel; stop it on return
	// so no goroutine outlives the request (no time.After leak).
	stopCtx := context.AfterFunc(ctx, func() {
		_ = ws.SetReadDeadline(time.Now())
		_ = ws.Close()
	})
	defer stopCtx()
	_ = ws.SetReadDeadline(socketDeadline(ctx, 60*time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"protocol":"json","version":1}`+recordSeparator)); err != nil {
		return "", errors.New("copilot protocol initialization failed")
	}
	_, _, err = ws.ReadMessage()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("copilot handshake read failed")
	}
	if err := ws.WriteMessage(websocket.TextMessage, append(data, recordSeparator...)); err != nil {
		return "", errors.New("copilot request dispatch failed")
	}
	var full strings.Builder
	for {
		_ = ws.SetReadDeadline(socketDeadline(ctx, 60*time.Second))
		_, frame, err := ws.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// Mid-stream socket cut is a truncated answer, never success.
			return "", errors.New("copilot websocket read failed")
		}
		for _, raw := range strings.Split(string(frame), recordSeparator) {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			var event struct {
				Type      int            `json:"type"`
				Arguments []any          `json:"arguments,omitempty"`
				Item      map[string]any `json:"item,omitempty"`
				Error     string         `json:"error"`
			}
			if json.Unmarshal([]byte(raw), &event) != nil {
				continue
			}
			if event.Type == 6 {
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+recordSeparator))
				continue
			}
			if event.Error != "" {
				return "", errors.New("copilot upstream error: " + event.Error)
			}
			if event.Type == 1 && len(event.Arguments) > 0 {
				if argMap, ok := event.Arguments[0].(map[string]any); ok {
					if msgs, ok := argMap["messages"].([]any); ok {
						for _, m := range msgs {
							if mObj, ok := m.(map[string]any); ok {
								if mObj["author"] == "bot" {
									if isLoaderMessage(mObj) {
										continue
									}
									if txt, ok := mObj["text"].(string); ok {
										txt = cleanLoaderPrefix(txt)
										if len(txt) > full.Len() {
											delta := txt[full.Len():]
											full.WriteString(delta)
											if onDelta != nil {
												onDelta(delta)
											}
										}
									}
								}
							}
						}
					}
				}
			}
			if event.Type == 2 {
				// Terminal frame: item.result decides success. Audit 2026-10-05:
				// InvalidRequest arrives with empty bot text; returning ""
				// with nil error hides it as HTTP 200 success.
				value, message := terminalResult(event.Item)
				if !isSuccessResult(value) {
					return "", terminalError(value, message)
				}
				var transferToken any
				var turnState any
				botMessages := 0
				botRawLen := 0
				var botTexts []string
				if item := event.Item; item != nil {
					transferToken, _ = item["conversationTransferToken"]
					turnState, _ = item["turnState"]
					if msgs, ok := item["messages"].([]any); ok {
						for _, m := range msgs {
							if mObj, ok := m.(map[string]any); ok {
								if mObj["author"] != "user" {
									if isLoaderMessage(mObj) {
										continue
									}
									if txt, ok := mObj["text"].(string); ok && txt != "" {
										botMessages++
										botRawLen += len(txt)
										cleaned := cleanLoaderPrefix(txt)
										if cleaned != "" {
											botTexts = append(botTexts, cleaned)
										}
									}
								}
							}
						}
						if len(botTexts) > 0 {
							finalText := strings.Join(botTexts, "\n\n")
							if len(finalText) > full.Len() {
								delta := finalText[full.Len():]
								full.WriteString(delta)
								if onDelta != nil {
									onDelta(delta)
								}
							}
						}
					}
				}
				// Block empty content before mutating the session: an empty
				// turn must not advance invocation state or transfer tokens.
				// Diagnose without tokens, URLs, or message text: counts only.
				if strings.TrimSpace(full.String()) == "" {
					switch {
					case botMessages == 0:
						return "", provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "copilot bot generated no messages in terminal frame")
					case botRawLen == 0:
						return "", provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "copilot bot sent messages with empty text (only loader or status)")
					default:
						return "", provider.NewEmptyResponseError(provider.EmptyReasonParserFailed, fmt.Sprintf("copilot bot sent %d messages (raw_len=%d) but cleaner extracted no text", botMessages, botRawLen))
					}
				}
				if token, ok := transferToken.(string); ok {
					captureTransferToken(sess, token)
				}
				if turnState != nil {
					captureTurnState(sess, turnState)
				}
				if sess != nil {
					sess.InvocationID++
				}
				return full.String(), nil
			}
		}
	}
}

func isLoaderMessage(mObj map[string]any) bool {
	if mType, ok := mObj["messageType"].(string); ok && mType != "" {
		return true
	}
	txt, _ := mObj["text"].(string)
	clean := strings.TrimSpace(txt)
	loaderPrefixes := []string{
		"Looking into it",
		"Working on it",
		"Just a sec",
		"Putting it together",
		"Thinking",
		"Searching the web",
		"Still thinking",
		"Almost there",
		"Finding info",
	}
	for _, p := range loaderPrefixes {
		if strings.HasPrefix(clean, p) && (len(clean) <= len(p)+4 || strings.HasSuffix(clean, "…") || strings.HasSuffix(clean, "...")) {
			return true
		}
	}
	return false
}

func cleanLoaderPrefix(text string) string {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		isLoader := false
		for _, p := range []string{"In progress", "Looking into it", "Working on it", "Just a sec", "Thinking"} {
			if strings.HasPrefix(first, p) && (strings.HasSuffix(first, "…") || strings.HasSuffix(first, "...")) {
				isLoader = true
				break
			}
		}
		if isLoader {
			lines = lines[1:]
		} else {
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func extractDelta(item map[string]any) string {
	for _, key := range []string{"text", "delta", "message"} {
		if value, ok := item[key].(string); ok {
			return value
		}
		if nested, ok := item[key].(map[string]any); ok {
			if text, ok := nested["text"].(string); ok {
				return text
			}
		}
	}
	return ""
}

func formatMessages(messages []provider.Message) string {
	if len(messages) == 0 {
		return ""
	}
	var b strings.Builder
	for i, message := range messages {
		text := message.TextContent()
		if text == "" {
			continue
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.Title(strings.ToLower(message.Role)))
		b.WriteString(": ")
		b.WriteString(text)
	}
	return b.String()
}

func sessionFromUpstream(upstream provider.UpstreamState) Session {
	return Session{ConversationID: upstream.ConversationID, ChatSessionID: upstream.ChatSessionID, InvocationID: upstream.InvocationID, ConversationTransferToken: upstream.ConversationTransferToken, TurnState: upstream.TurnState}
}

func upstreamFromSession(sess Session) provider.UpstreamState {
	return provider.UpstreamState{ConversationID: sess.ConversationID, ChatSessionID: sess.ChatSessionID, InvocationID: sess.InvocationID, ConversationTransferToken: sess.ConversationTransferToken, TurnState: sess.TurnState}
}

// terminalResult reads the type-2 completion status. Missing result on an
// otherwise-closed turn is treated as empty (caller rejects empty Success).
func terminalResult(item map[string]any) (value, message string) {
	if item == nil {
		return "", ""
	}
	raw, _ := item["result"].(map[string]any)
	if raw == nil {
		return "", ""
	}
	value, _ = raw["value"].(string)
	message, _ = raw["message"].(string)
	return strings.TrimSpace(value), strings.TrimSpace(message)
}

func isSuccessResult(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "Success") || value == ""
}

func terminalError(value, message string) error {
	if value == "" {
		return provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "copilot terminal frame had no result value")
	}
	kind := "copilot upstream rejected request"
	lower := strings.ToLower(value)
	switch {
	case strings.Contains(lower, "throttl") || strings.Contains(lower, "ratelimit") || strings.Contains(lower, "429"):
		kind = "copilot upstream throttled request"
	case strings.Contains(lower, "invalidrequest") || strings.Contains(lower, "invalid_request"):
		kind = "copilot upstream rejected continuation"
	}
	if message != "" {
		capped := message
		if len(capped) > 300 {
			capped = capped[:300]
		}
		return fmt.Errorf("%s (%s): %s", kind, value, capped)
	}
	return fmt.Errorf("%s (%s)", kind, value)
}

func isInvalidRequestError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rejected continuation") || strings.Contains(msg, "invalidrequest") || strings.Contains(msg, "invalid_request")
}

func captureTransferToken(sess *Session, token string) {
	if sess == nil || strings.TrimSpace(token) == "" || len(token) > 8192 {
		return
	}
	sess.ConversationTransferToken = token
}

func captureTurnState(sess *Session, state any) {
	if sess == nil {
		return
	}
	switch v := state.(type) {
	case string:
		if v != "" && len(v) <= 32768 {
			sess.TurnState = v
		}
	case map[string]any:
		if data, err := json.Marshal(v); err == nil && len(data) <= 32768 {
			sess.TurnState = string(data)
		}
	}
}
func mapTone(model string) string {
	m := strings.ToLower(model)
	if strings.Contains(m, "think") || strings.Contains(m, "reason") {
		return "Gpt_5_6_Reasoning"
	}
	if strings.Contains(m, "creative") {
		return "Creative"
	}
	if strings.Contains(m, "precise") {
		return "Precise"
	}
	if strings.Contains(m, "balanced") {
		return "Balanced"
	}
	return "Gpt_5_6_Chat"
}
func randomHex(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return fmt.Sprintf("%x", b) }
func randomUUID() string {
	return randomHex(4) + "-" + randomHex(2) + "-" + randomHex(2) + "-" + randomHex(2) + "-" + randomHex(6)
}
func cloneMap(in map[string]any) map[string]any {
	data, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}
