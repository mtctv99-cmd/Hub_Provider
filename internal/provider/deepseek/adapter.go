package deepseek

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sever_provider/internal/provider"
)

// CircuitBreakerState biểu thị trạng thái của Circuit Breaker.
type CircuitBreakerState string

const (
	CircuitClosed   CircuitBreakerState = "CLOSED"
	CircuitHalfOpen CircuitBreakerState = "HALF_OPEN"
	CircuitOpen     CircuitBreakerState = "OPEN"
)

// CircuitBreaker bảo vệ hệ thống khỏi lặp lỗi liên tục khi upstream gặp sự cố.
type CircuitBreaker struct {
	mu           sync.RWMutex
	state        CircuitBreakerState
	consecutive  int
	threshold    int
	cooldown     time.Duration
	openedAt     time.Time
	halfOpenPass int
}

// NewCircuitBreaker khởi tạo breaker mới.
func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 1 * time.Minute
	}
	return &CircuitBreaker{
		state:     CircuitClosed,
		threshold: threshold,
		cooldown:  cooldown,
	}
}

// Allow kiểm tra request có được phép thực hiện hay không.
func (cb *CircuitBreaker) Allow(now time.Time) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if now.Sub(cb.openedAt) >= cb.cooldown {
			cb.state = CircuitHalfOpen
			cb.halfOpenPass = 0
			return true
		}
		return false
	case CircuitHalfOpen:
		// Cho phép một số ít request thử nghiệm
		if cb.halfOpenPass < 2 {
			cb.halfOpenPass++
			return true
		}
		return false
	default:
		return true
	}
}

// RecordSuccess ghi nhận request thành công, reset breaker về CLOSED.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = CircuitClosed
	cb.consecutive = 0
	cb.halfOpenPass = 0
}

// RecordFailure ghi nhận request thất bại, mở breaker nếu vượt ngưỡng.
func (cb *CircuitBreaker) RecordFailure(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutive++
	if cb.consecutive >= cb.threshold || cb.state == CircuitHalfOpen {
		cb.state = CircuitOpen
		cb.openedAt = time.Now()
	}
}

// State trả về trạng thái hiện tại.
func (cb *CircuitBreaker) State() CircuitBreakerState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Adapter triển khai interface provider.Adapter cho DeepSeek Web.
type Adapter struct {
	Client       *Client
	activeLeases atomic.Int32
	Breaker      *CircuitBreaker
}

// New khởi tạo DeepSeek Web Adapter.
func New(client *Client) *Adapter {
	if client == nil {
		client = NewClient("", nil)
	}
	return &Adapter{
		Client:  client,
		Breaker: NewCircuitBreaker(3, 1*time.Minute),
	}
}

// Name trả về tên định danh provider.
func (a *Adapter) Name() string {
	return "deepseek_web"
}

// Capabilities trả về danh sách năng lực hỗ trợ.
func (a *Adapter) Capabilities() []provider.Capability {
	return []provider.Capability{
		provider.CapabilityChat,
		provider.CapabilityStreaming,
		provider.CapabilityReasoning,
	}
}

// ActiveLeases trả về số lease đang hoạt động.
func (a *Adapter) ActiveLeases() int32 {
	return a.activeLeases.Load()
}

// AcquireLease tăng số lease đang thực thi.
func (a *Adapter) AcquireLease() {
	a.activeLeases.Add(1)
}

// ReleaseLease giảm số lease khi hoàn tất.
func (a *Adapter) ReleaseLease() {
	a.activeLeases.Add(-1)
}

// CredentialSecret chứa thông tin bí mật tài khoản DeepSeek.
type CredentialSecret struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id,omitempty"`
}

func parseCredentialSecret(secretBytes []byte) (token string, sessionID string, err error) {
	if len(secretBytes) == 0 {
		return "", "", errors.New("empty credential secret")
	}

	var cred CredentialSecret
	if err := json.Unmarshal(secretBytes, &cred); err == nil && strings.TrimSpace(cred.Token) != "" {
		return strings.TrimSpace(cred.Token), strings.TrimSpace(cred.SessionID), nil
	}

	// Thử dùng raw string token nếu không phải JSON
	rawStr := strings.TrimSpace(string(secretBytes))
	if rawStr != "" && !strings.HasPrefix(rawStr, "{") {
		return rawStr, "", nil
	}

	return "", "", errors.New("invalid credential secret: missing token")
}

// IsTokenExpired checks JWT exp claim. Returns true if token expires within minSeconds.
func IsTokenExpired(token string, minSeconds int64) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return false
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(decoded, &claims) != nil || claims.Exp == 0 {
		return false
	}
	return claims.Exp <= time.Now().Unix()+minSeconds
}

func isReasonerModel(model string) bool {
	m := strings.ToLower(model)
	if strings.Contains(m, "nothinking") {
		return false
	}
	return strings.Contains(m, "reasoner") || strings.Contains(m, "r1") || strings.Contains(m, "pro")
}

func isSearchModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "search")
}

// Chat gọi chat completion dạng non-streaming.
func (a *Adapter) Chat(ctx context.Context, req provider.ChatRequest, credentialSecret []byte) (provider.ChatResponse, error) {
	if !a.Breaker.Allow(time.Now()) {
		return provider.ChatResponse{}, errors.New("deepseek: circuit breaker open")
	}

	token, credSessionID, err := parseCredentialSecret(credentialSecret)
	if err != nil {
		return provider.ChatResponse{}, fmt.Errorf("deepseek parse credential: %w", err)
	}
	if IsTokenExpired(token, 60) {
		return provider.ChatResponse{}, errors.New("deepseek: token expired or about to expire (auth required)")
	}

	sessionID := req.Upstream.ChatSessionID
	if sessionID == "" {
		sessionID = credSessionID
	}

	prompt := provider.FormatPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	thinkingEnabled := isReasonerModel(req.Model)
	searchEnabled := isSearchModel(req.Model)

	a.AcquireLease()
	defer a.ReleaseLease()

	resp, chatSessionID, err := a.Client.SendPrompt(ctx, token, sessionID, prompt, thinkingEnabled, searchEnabled)
	if err != nil {
		a.Breaker.RecordFailure(err)
		return provider.ChatResponse{}, err
	}
	defer resp.Body.Close()

	parser := NewStreamParser(thinkingEnabled, chatSessionID)
	stream := parser.ParseStream(ctx, resp.Body)

	var contentBuilder strings.Builder
	var reasoningBuilder strings.Builder

	for chunk := range stream {
		if chunk.Err != nil {
			a.Breaker.RecordFailure(chunk.Err)
			return provider.ChatResponse{}, chunk.Err
		}
		if chunk.Content != "" {
			contentBuilder.WriteString(chunk.Content)
		}
		if chunk.ReasoningContent != "" {
			reasoningBuilder.WriteString(chunk.ReasoningContent)
		}
	}

	a.Breaker.RecordSuccess()

	fullContent := contentBuilder.String()
	cleanContent := fullContent
	var toolCalls []provider.ToolCall
	finishReason := "stop"

	if len(req.Tools) > 0 && req.ToolChoice != "none" {
		cleanContent, toolCalls = provider.ParseToolCalls(fullContent)
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
	}

	return provider.ChatResponse{
		ID:               "deepseek-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Model:            req.Model,
		Content:          cleanContent,
		ReasoningContent: reasoningBuilder.String(),
		ToolCalls:        toolCalls,
		FinishReason:     finishReason,
		Upstream:         provider.UpstreamState{ChatSessionID: chatSessionID},
	}, nil
}

// StreamChat gọi chat completion dạng SSE streaming.
func (a *Adapter) StreamChat(ctx context.Context, req provider.ChatRequest, credentialSecret []byte) (<-chan provider.StreamChunk, error) {
	if !a.Breaker.Allow(time.Now()) {
		return nil, errors.New("deepseek: circuit breaker open")
	}

	token, credSessionID, err := parseCredentialSecret(credentialSecret)
	if err != nil {
		return nil, fmt.Errorf("deepseek parse credential: %w", err)
	}
	if IsTokenExpired(token, 60) {
		return nil, errors.New("deepseek: token expired or about to expire (auth required)")
	}

	sessionID := req.Upstream.ChatSessionID
	if sessionID == "" {
		sessionID = credSessionID
	}

	prompt := provider.FormatPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	thinkingEnabled := isReasonerModel(req.Model)
	searchEnabled := isSearchModel(req.Model)

	a.AcquireLease()

	resp, chatSessionID, err := a.Client.SendPrompt(ctx, token, sessionID, prompt, thinkingEnabled, searchEnabled)
	if err != nil {
		a.ReleaseLease()
		a.Breaker.RecordFailure(err)
		return nil, err
	}

	out := make(chan provider.StreamChunk, 32)
	parser := NewStreamParser(thinkingEnabled, chatSessionID)
	stream := parser.ParseStream(ctx, resp.Body)

	go func() {
		defer a.ReleaseLease()
		defer resp.Body.Close()
		defer close(out)

		hasError := false
		hasTools := len(req.Tools) > 0 && req.ToolChoice != "none"

		if !hasTools {
			for chunk := range stream {
				if chunk.Err != nil {
					hasError = true
					a.Breaker.RecordFailure(chunk.Err)
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
			}
			if !hasError {
				a.Breaker.RecordSuccess()
			}
			return
		}

		// Chế độ Tool Calling:
		// Stream reasoning chunks trực tiếp. Buffer text content để parse tool calls sạch sẽ.
		var contentBuf strings.Builder
		for chunk := range stream {
			if chunk.Err != nil {
				hasError = true
				a.Breaker.RecordFailure(chunk.Err)
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
				return
			}

			if chunk.ReasoningContent != "" {
				select {
				case out <- provider.StreamChunk{
					ReasoningContent: chunk.ReasoningContent,
					Upstream:         chunk.Upstream,
				}:
				case <-ctx.Done():
					return
				}
			}

			if chunk.Content != "" {
				contentBuf.WriteString(chunk.Content)
			}
		}

		if !hasError {
			a.Breaker.RecordSuccess()
		}

		fullText := contentBuf.String()
		cleanText, toolCalls := provider.ParseToolCalls(fullText)

		finishReason := "stop"
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}

		if cleanText != "" {
			select {
			case out <- provider.StreamChunk{
				Content:  cleanText,
				Upstream: provider.UpstreamState{ChatSessionID: chatSessionID},
			}:
			case <-ctx.Done():
				return
			}
		}

		select {
		case out <- provider.StreamChunk{
			Done:         true,
			ToolCalls:    toolCalls,
			FinishReason: finishReason,
			Upstream:     provider.UpstreamState{ChatSessionID: chatSessionID},
		}:
		case <-ctx.Done():
			return
		}
	}()

	return out, nil
}

// Speech không hỗ trợ.
func (a *Adapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}

// Image không hỗ trợ.
func (a *Adapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}
