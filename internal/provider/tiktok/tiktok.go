package tiktok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"sever_provider/internal/provider"
)

const (
	maxTextBytes            = 300
	failureLimit            = 3
	cooldown                = 15 * time.Minute
	endpointPenaltyDuration = 180 * time.Second
)

var defaultEndpoints = []string{
	"https://api16-normal-v6.tiktokv.com/media/api/text/speech/invoke/",
	"https://api16-normal-c-useast1a.tiktokv.com/media/api/text/speech/invoke/",
}

var userAgents = []string{
	"com.zhiliaoapp.musically/2022600030 (Linux; U; Android 10; en_US; SM-G981B; Build/QP1A.190711.020;tt-ok/3.12.13.1)",
	"com.zhiliaoapp.musically/2022500040 (Linux; U; Android 11; en_US; Pixel 5; Build/RQ3A.210905.001;tt-ok/3.12.13.1)",
}

type Session struct {
	ID            string
	Failures      int
	CooldownUntil time.Time
	LastUsed      time.Time
}

type Pool struct {
	mu       sync.Mutex
	sessions []Session
	next     int
}

func (p *Pool) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, item := range p.sessions {
		if !now.Before(item.CooldownUntil) {
			return true
		}
	}
	return false
}

func (p *Pool) Size() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.sessions) }

func NewPool(sessionIDs []string) *Pool {
	p := &Pool{}
	for _, id := range sessionIDs {
		if strings.TrimSpace(id) != "" {
			p.sessions = append(p.sessions, Session{ID: strings.TrimSpace(id)})
		}
	}
	return p
}

func (p *Pool) AddSession(id string)    { p.Add(id) }
func (p *Pool) RemoveSession(id string) { p.Remove(id) }

func (p *Pool) Remove(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, item := range p.sessions {
		if item.ID == id {
			p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
			if len(p.sessions) == 0 {
				p.next = 0
			} else {
				p.next %= len(p.sessions)
			}
			return
		}
	}
}

func (p *Pool) Add(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, item := range p.sessions {
		if item.ID == id {
			return
		}
	}
	p.sessions = append(p.sessions, Session{ID: id})
}

func (p *Pool) Acquire(now time.Time) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) == 0 {
		return "", errors.New("tiktok session pool is empty")
	}
	for i := 0; i < len(p.sessions); i++ {
		idx := (p.next + i) % len(p.sessions)
		item := &p.sessions[idx]
		if now.Before(item.CooldownUntil) || (!item.LastUsed.IsZero() && now.Sub(item.LastUsed) < 200*time.Millisecond) {
			continue
		}
		item.LastUsed = now
		p.next = (idx + 1) % len(p.sessions)
		return item.ID, nil
	}
	for i := 0; i < len(p.sessions); i++ {
		idx := (p.next + i) % len(p.sessions)
		item := &p.sessions[idx]
		if now.Before(item.CooldownUntil) {
			continue
		}
		item.LastUsed = now
		p.next = (idx + 1) % len(p.sessions)
		return item.ID, nil
	}
	return "", errors.New("all tiktok sessions are cooling down")
}

func (p *Pool) Success(id string) { p.update(id, false) }
func (p *Pool) Failure(id string) { p.update(id, true) }
func (p *Pool) RateLimited(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.sessions {
		if p.sessions[i].ID == id {
			p.sessions[i].Failures = 0
			p.sessions[i].CooldownUntil = time.Now().Add(cooldown)
		}
	}
}

func (p *Pool) update(id string, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.sessions {
		if p.sessions[i].ID != id {
			continue
		}
		if failed {
			p.sessions[i].Failures++
			if p.sessions[i].Failures >= failureLimit {
				p.sessions[i].CooldownUntil = time.Now().Add(cooldown)
				p.sessions[i].Failures = 0
			}
		} else {
			p.sessions[i].Failures = 0
			p.sessions[i].CooldownUntil = time.Time{}
		}
	}
}

type Adapter struct {
	Client            *http.Client
	Pool              *Pool
	Endpoints         []string
	UserAgents        []string
	mu                sync.Mutex
	endpointDeadUntil map[string]time.Time
}

func New(client *http.Client, pool *Pool) *Adapter {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if pool == nil {
		pool = NewPool(nil)
	}
	return &Adapter{Client: client, Pool: pool, Endpoints: append([]string(nil), defaultEndpoints...), UserAgents: append([]string(nil), userAgents...), endpointDeadUntil: make(map[string]time.Time)}
}

func (a *Adapter) AddSession(sessionID string) {
	if a.Pool != nil {
		a.Pool.Add(sessionID)
	}
}

func (a *Adapter) Name() string { return "tiktok_api" }
func (a *Adapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilitySpeech}
}
func (a *Adapter) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, provider.ErrCapabilityUnsupported
}
func (a *Adapter) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	return nil, provider.ErrCapabilityUnsupported
}
func (a *Adapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}
func (a *Adapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

func (a *Adapter) Speech(ctx context.Context, req provider.SpeechRequest, credentialSecret []byte) ([]byte, string, error) {
	parts := splitText(req.Input, maxTextBytes)
	if len(parts) == 0 {
		return nil, "", errors.New("tts input is empty")
	}
		voice := req.Voice
		if strings.HasPrefix(voice, "tiktok:") {
			voice = strings.TrimPrefix(voice, "tiktok:")
		}
		switch voice {
		case "vi_male":
			voice = "BV074_streaming"
		case "vi_female":
			voice = "BV075_streaming"
		case "":
			if strings.HasPrefix(req.Model, "tiktok:") {
				voice = strings.TrimPrefix(req.Model, "tiktok:")
				if voice == "vi_male" {
					voice = "BV074_streaming"
				} else if voice == "vi_female" {
					voice = "BV075_streaming"
				}
			} else {
				voice = "BV075_streaming"
			}
		}

	var secret struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(credentialSecret, &secret)
	var session string
	var err error
	if secret.SessionID != "" {
		session = secret.SessionID
	} else {
		session, err = a.Pool.Acquire(time.Now())
	}
	if err != nil {
		return nil, "", err
	}

	var out bytes.Buffer
	for _, part := range parts {
		data, err := a.generatePart(ctx, session, part, voice)
		if err != nil {
			if errors.Is(err, errRateLimited) {
				a.Pool.RateLimited(session)
			} else {
				a.Pool.Failure(session)
			}
			return nil, "", err
		}
		a.Pool.Success(session)
		_, _ = out.Write(data)
	}
	return out.Bytes(), "audio/mpeg", nil
}

var errRateLimited = errors.New("tiktok rate limited")

func (a *Adapter) generatePart(ctx context.Context, session, text, voice string) ([]byte, error) {
	if len(a.Endpoints) == 0 {
		return nil, errors.New("tiktok endpoint list is empty")
	}
	for attempt := 0; attempt < 3; attempt++ {
		for _, endpoint := range a.healthyEndpoints() {
			form := "text_speaker=" + url.QueryEscape(voice) + "&req_text=" + url.QueryEscape(text) + "&speaker_map_type=0&aid=1233"
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Accept-Encoding", "identity")
			req.Header.Set("Cookie", "sessionid="+session)
			if len(a.UserAgents) > 0 {
				req.Header.Set("User-Agent", a.UserAgents[rand.Intn(len(a.UserAgents))])
			}
			resp, err := a.Client.Do(req)
			if err != nil {
				a.penalizeEndpoint(endpoint)
				continue
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				a.penalizeEndpoint(endpoint)
				continue
			}
			if resp.StatusCode != http.StatusOK {
				if resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500 {
					a.penalizeEndpoint(endpoint)
				}
				continue
			}
			var result struct {
				Message string `json:"message"`
				Data    struct {
					VStr string `json:"v_str"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &result) != nil {
				continue
			}
			if strings.EqualFold(result.Message, "success") && result.Data.VStr != "" {
				decoded, decodeErr := base64.StdEncoding.DecodeString(result.Data.VStr)
				if decodeErr == nil && len(decoded) > 0 {
					return decoded, nil
				}
			}
			message := strings.ToLower(result.Message)
			if strings.Contains(message, "rate") || strings.Contains(message, "couldn’t") || strings.Contains(message, "couldn’t") {
				return nil, errRateLimited
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("tiktok speech request failed")
}

func (a *Adapter) healthyEndpoints() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	out := make([]string, 0, len(a.Endpoints))
	for _, endpoint := range a.Endpoints {
		if until, ok := a.endpointDeadUntil[endpoint]; !ok || !now.Before(until) {
			out = append(out, endpoint)
		}
	}
	if len(out) == 0 {
		out = append(out, a.Endpoints...)
		a.endpointDeadUntil = make(map[string]time.Time)
	}
	return out
}

func (a *Adapter) penalizeEndpoint(endpoint string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.endpointDeadUntil == nil {
		a.endpointDeadUntil = make(map[string]time.Time)
	}
	a.endpointDeadUntil[endpoint] = time.Now().Add(endpointPenaltyDuration)
}

func splitText(input string, maxBytes int) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}
	var result []string
	for len(input) > maxBytes {
		cut := maxBytes
		for cut > 0 && (input[cut]&0xc0) == 0x80 {
			cut--
		}
		if cut == 0 {
			cut = maxBytes
		}
		if idx := strings.LastIndexAny(input[:cut], " \n,.;!?。！？，；："); idx > maxBytes/2 {
			cut = idx + 1
		}
		result = append(result, strings.TrimSpace(input[:cut]))
		input = strings.TrimSpace(input[cut:])
	}
	if input != "" {
		result = append(result, input)
	}
	return result
}

func urlEscape(value string) string {
	var b strings.Builder
	for _, c := range []byte(value) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
