package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/provider"
)

const maxGeneratedImageBytes = 20 * 1024 * 1024

var imageURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

func (a *Adapter) generateImage(ctx context.Context, account Account, prompt string) (provider.ImageResponse, error) {
	wsURL, conversationID, chatSessionID, err := BuildInvocationURL(account.TokenURL, &Session{})
	if err != nil {
		return provider.ImageResponse{}, err
	}
	payload := cloneMap(a.Template)
	if payload == nil {
		return provider.ImageResponse{}, errors.New("copilot invocation template is required")
	}
	payload["invocationId"] = "0"
	args, ok := payload["arguments"].([]any)
	if !ok || len(args) == 0 {
		return provider.ImageResponse{}, errors.New("copilot template arguments missing")
	}
	arg0, ok := args[0].(map[string]any)
	if !ok {
		return provider.ImageResponse{}, errors.New("copilot template argument shape invalid")
	}
	arg0["conversationId"], arg0["sessionId"], arg0["tone"] = conversationID, chatSessionID, "Magic"
	arg0["isStartOfSession"] = true
	if message, ok := arg0["message"].(map[string]any); ok {
		message["text"] = prompt
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return provider.ImageResponse{}, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	header := http.Header{"Origin": []string{a.Origin}, "User-Agent": []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/153 Safari/537.36"}}
	ws, _, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return provider.ImageResponse{}, errors.New("copilot image websocket connection failed")
	}
	defer ws.Close()
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"protocol":"json","version":1}`+recordSeparator)); err != nil {
		return provider.ImageResponse{}, errors.New("copilot image protocol initialization failed")
	}
	if _, _, err := ws.ReadMessage(); err != nil {
		return provider.ImageResponse{}, errors.New("copilot image handshake failed")
	}
	if err := ws.WriteMessage(websocket.TextMessage, append(data, recordSeparator...)); err != nil {
		return provider.ImageResponse{}, errors.New("copilot image request dispatch failed")
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_ = ws.SetReadDeadline(time.Now().Add(20 * time.Second))
		kind, frame, readErr := ws.ReadMessage()
		if readErr != nil {
			return provider.ImageResponse{}, errors.New("copilot image websocket read failed")
		}
		if kind != websocket.TextMessage {
			continue
		}
		for _, raw := range strings.Split(string(frame), recordSeparator) {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			var event struct {
				Type  int            `json:"type"`
				Item  map[string]any `json:"item"`
				Error string         `json:"error"`
			}
			if json.Unmarshal([]byte(raw), &event) != nil {
				continue
			}
			if event.Type == 6 {
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+recordSeparator))
				continue
			}
			if event.Error != "" {
				return provider.ImageResponse{}, errors.New("copilot image upstream error")
			}
			if event.Type != 2 && event.Type != 3 {
				continue
			}
			urls := extractImageURLs(event.Item)
			if len(urls) == 0 {
				return provider.ImageResponse{}, errors.New("copilot image response contained no image URL")
			}
			return downloadImage(ctx, urls[0])
		}
	}
	return provider.ImageResponse{}, context.DeadlineExceeded
}

func extractImageURLs(value any) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch item := v.(type) {
		case string:
			for _, match := range imageURLPattern.FindAllString(item, -1) {
				clean := strings.TrimRight(match, ".,);]")
				u, err := url.Parse(clean)
				if err == nil && (u.Scheme == "http" || u.Scheme == "https") && !seen[clean] {
					seen[clean] = true
					out = append(out, clean)
				}
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		case map[string]any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(value)
	return out
}

func validateImageURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Hostname() == "" || (u.Port() != "" && u.Port() != "443") || strings.HasSuffix(u.Hostname(), ".") {
		return errors.New("copilot image URL is not a public HTTPS address")
	}
	if _, err := netip.ParseAddr(u.Hostname()); err == nil {
		return errors.New("copilot image IP literals are not allowed")
	} else if !strings.Contains(u.Hostname(), ".") || strings.ContainsAny(u.Hostname(), "\\%") || strings.HasSuffix(strings.ToLower(u.Hostname()), ".localhost") {
		return errors.New("copilot image host is invalid")
	}
	return nil
}

func publicImageIP(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Is4In6() {
		return false
	}
	for _, cidr := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "fc00::/7"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return false
		}
	}
	return true
}

func imageTransport() *http.Transport {
	return &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid image address")
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("unable to resolve image host")
		}
		for _, ip := range ips {
			if !publicImageIP(ip) {
				return nil, errors.New("image host resolves to a nonpublic IP")
			}
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}}
}

func downloadImage(ctx context.Context, rawURL string) (provider.ImageResponse, error) {
	if err := validateImageURL(rawURL); err != nil {
		return provider.ImageResponse{}, err
	}
	transport := imageTransport()
	defer transport.CloseIdleConnections()
	clientCopy := http.Client{Timeout: 30 * time.Second, Transport: transport}
	clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("image redirect limit exceeded")
		}
		return validateImageURL(req.URL.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return provider.ImageResponse{}, err
	}
	resp, err := clientCopy.Do(req)
	if err != nil {
		return provider.ImageResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return provider.ImageResponse{}, fmt.Errorf("copilot image download HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxGeneratedImageBytes {
		return provider.ImageResponse{}, errors.New("copilot image exceeds maximum size")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedImageBytes+1))
	if err != nil {
		return provider.ImageResponse{}, err
	}
	if len(data) > maxGeneratedImageBytes {
		return provider.ImageResponse{}, errors.New("copilot image exceeds maximum size")
	}
	mimeType := strings.Split(resp.Header.Get("Content-Type"), ";")[0]
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return provider.ImageResponse{}, errors.New("copilot response is not an image")
	}
	return provider.ImageResponse{Data: data, MIME: mimeType}, nil
}
