package gemini

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	ErrEmptyCookie            = errors.New("cookie string is empty")
	ErrMissingSessionCookie   = errors.New("cookie missing __Secure-1PSID or SID session token")
	ErrMissingSapisidCookie   = errors.New("cookie missing SAPISID token (required for authentication hash)")
	ErrCookieInvalidOrExpired = errors.New("gemini cookie is invalid or expired (no XSRF token found)")
)

// SanitizeCookie cleans raw cookie input by stripping common headers, newlines,
// carriage returns, and quotes, and verifies key cookie components.
func SanitizeCookie(raw string) (string, error) {
	c := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(c), "cookie:") {
		c = strings.TrimSpace(c[7:])
	}
	c = strings.Trim(c, "\"'")
	c = strings.ReplaceAll(c, "\r", "")
	c = strings.ReplaceAll(c, "\n", " ")
	c = strings.TrimSpace(c)

	if c == "" {
		return "", ErrEmptyCookie
	}

	hasPSID := cookieValue(c, "__Secure-1PSID") != "" || cookieValue(c, "SID") != ""
	if !hasPSID {
		return "", ErrMissingSessionCookie
	}
	if SapisidFromCookie(c) == "" {
		return "", ErrMissingSapisidCookie
	}

	return c, nil
}

type PageTokens struct {
	XSRF          string
	BL            string
	FSID          string
	PushID        string
	Authenticated bool
	FetchedAt     time.Time
}

type TokenProvider struct {
	Client  *http.Client
	BaseURL string
	mu      sync.Mutex
	cache   map[string]PageTokens
	TTL     time.Duration
}

func NewTokenProvider(client *http.Client) *TokenProvider {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &TokenProvider{
		Client:  client,
		BaseURL: "https://gemini.google.com",
		cache:   make(map[string]PageTokens),
		TTL:     10 * time.Minute,
	}
}

func (p *TokenProvider) SetBaseURL(baseURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.BaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	p.cache = make(map[string]PageTokens)
}

var tokenPatterns = map[string]*regexp.Regexp{
	"xsrf":    regexp.MustCompile(`\"(?:SNlM0e|thykhd)\":\"([^\"]+)\"`),
	"bl":      regexp.MustCompile(`\"(?:cfb2h|cfb2h)\":\"([^\"]+)\"`),
	"fsid":    regexp.MustCompile(`\"FdrFJe\":\"([^\"]+)\"`),
	"push_id": regexp.MustCompile(`\"qKIAYe\":\"([^\"]+)\"`),
}

func (p *TokenProvider) Get(ctx context.Context, cookie string) (PageTokens, error) {
	cookie = strings.TrimSpace(cookie)
	p.mu.Lock()
	if cached, ok := p.cache[cookie]; ok && time.Since(cached.FetchedAt) < p.TTL && cached.XSRF != "" {
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	baseURL := strings.TrimRight(p.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://gemini.google.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/app", nil)
	if err != nil {
		return PageTokens{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return PageTokens{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PageTokens{}, err
	}
	if resp.StatusCode >= 400 {
		return PageTokens{}, fmt.Errorf("gemini token page returned HTTP %d", resp.StatusCode)
	}
	text := string(body)

	result := PageTokens{FetchedAt: time.Now(), PushID: "feeds/mcudyrk2a4khkz"}
	if m := tokenPatterns["push_id"].FindStringSubmatch(text); len(m) > 1 {
		result.PushID = m[1]
	}
	if m := tokenPatterns["xsrf"].FindStringSubmatch(text); len(m) > 1 {
		result.XSRF = m[1]
		result.Authenticated = strings.Contains(cookie, "__Secure-1PSID=") || strings.Contains(cookie, "SID=")
	}
	if m := tokenPatterns["bl"].FindStringSubmatch(text); len(m) > 1 {
		result.BL = m[1]
	}
	if m := tokenPatterns["fsid"].FindStringSubmatch(text); len(m) > 1 {
		result.FSID = m[1]
	}
	if result.BL == "" {
		result.BL = "boq_assistant-bard-web-server"
	}
	if result.XSRF == "" {
		return PageTokens{}, ErrCookieInvalidOrExpired
	}
	p.mu.Lock()
	p.cache[cookie] = result
	p.mu.Unlock()
	return result, nil
}

func cookieValue(cookie, key string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, key+"=") {
			return strings.TrimPrefix(part, key+"=")
		}
	}
	return ""
}

func SapisidFromCookie(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		for _, key := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
			if strings.HasPrefix(part, key+"=") {
				return strings.TrimPrefix(part, key+"=")
			}
		}
	}
	return ""
}

func SapisidHash(sapisid string, now time.Time) string {
	ts := now.Unix()
	digest := sha1.Sum([]byte(fmt.Sprintf("%d %s https://gemini.google.com", ts, sapisid)))
	return fmt.Sprintf("SAPISIDHASH %d_%x", ts, digest)
}
