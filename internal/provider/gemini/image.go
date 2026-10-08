package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"sever_provider/internal/provider"
)

const (
	maxImageRPCBytes       = 4 * 1024 * 1024
	maxGeneratedImageBytes = 16 * 1024 * 1024
	maxImageLines          = 128
)

// Image uses the same authenticated StreamGenerate request as chat. The image
// extractor deliberately recognizes only candidate[12] image attachments, not
// arbitrary URLs in the model's prose or other RPC fields.
func (a *Adapter) Image(ctx context.Context, req provider.ImageRequest, secret []byte) (provider.ImageResponse, error) {
	if req.Model != "gemini-3.8-flash" || strings.TrimSpace(req.Prompt) == "" || req.Size != "" {
		return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
	}
	var cred struct {
		Cookie string `json:"cookie"`
	}
	if err := json.Unmarshal(secret, &cred); err != nil {
		return provider.ImageResponse{}, errors.New("gemini image credential is invalid")
	}
	cookie, err := SanitizeCookie(cred.Cookie)
	if err != nil {
		return provider.ImageResponse{}, fmt.Errorf("gemini image requires an authenticated cookie: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	raw, err := a.generateImageRaw(ctx, "Generate an image of: "+req.Prompt, req.Model, cookie)
	if err != nil {
		return provider.ImageResponse{}, err
	}
	imageURL, err := extractGeneratedImageURL(raw)
	if err != nil {
		return provider.ImageResponse{}, err
	}
	if resp, dlErr := a.downloadGeneratedImageWithCookie(ctx, imageURL, "", false); dlErr == nil {
		return resp, nil
	}
	// ponytail: CDN lh3.googleusercontent.com thường 403 khi thiếu Cookie/Referer.
	// Retry 1 lần kèm cookie phiên (không gửi Authorization) vì URL gg-dl là
	// signed URL gắn phiên, không phải asset public.
	return a.downloadGeneratedImageWithCookie(ctx, imageURL, cookie, true)
}

func (a *Adapter) generateImageRaw(ctx context.Context, prompt, model, cookie string) (string, error) {
	a.syncTokenBaseURL()
	tokens, err := a.Tokens.Get(ctx, cookie)
	if err != nil {
		return "", err
	}
	if tokens.XSRF == "" {
		return "", fmt.Errorf("gemini page did not expose XSRF token")
	}
	body := BuildPayload(prompt, model, tokens.XSRF, "", ConversationMetadata{}, nil)
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
	req.Header.Set("x-goog-ext-525001261-jspb", imageRPCHeader())
	resp, err := a.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxImageRPCBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(raw)) > maxImageRPCBytes {
		return "", fmt.Errorf("gemini image RPC response exceeded bounded maximum of %d bytes", maxImageRPCBytes)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini RPC HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return string(raw), nil
}

func (a *Adapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

func imageRPCHeader() string {
	// Mode 10 bật Imagen trong StreamGenerate RPC, tách khỏi header chat text 71c2d248d3b102ff.
	return `[1,null,null,null,"56fdd199312815e2",null,null,0,[4,5,6,8,4,5,6,8],null,null,2,null,null,1,1,"37148B01-D513-431C-B2FD-18F42A6EFE22"]`
}

func generatedImageURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.RawQuery != "" {
		return "", false
	}
	switch u.Host {
	case "lh3.googleusercontent.com":
		if !strings.HasPrefix(u.Path, "/gg/") && !strings.HasPrefix(u.Path, "/gg-dl/") {
			return "", false
		}
	case "work.fife.usercontent.google.com":
		if !strings.HasPrefix(u.Path, "/rd-gg-dl/") {
			return "", false
		}
	default:
		return "", false
	}
	if len(u.Path) > 2048 || len(u.Path) < 10 || strings.ContainsAny(u.Path, "\\%") || u.EscapedPath() != u.Path {
		return "", false
	}
	return raw, true
}

func generatedImageRedirectURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	switch u.Host {
	case "lh3.googleusercontent.com", "work.fife.usercontent.google.com":
		return raw, true
	}
	return "", false
}

func validateImageRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return errors.New("gemini image redirect rejected")
	}
	// Giữ cookie phiên và referer khi theo redirect CDN nội bộ Google.
	if len(via) > 0 {
		if c := via[0].Header.Get("Cookie"); c != "" {
			req.Header.Set("Cookie", c)
		}
		if r := via[0].Header.Get("Referer"); r != "" {
			req.Header.Set("Referer", r)
		}
	}
	if _, ok := generatedImageRedirectURL(req.URL.String()); !ok {
		return errors.New("gemini image redirect rejected")
	}
	return nil
}

func extractGeneratedImageURL(raw string) (string, error) {
	if len(raw) > maxImageRPCBytes {
		return "", errors.New("gemini image RPC response is too large")
	}
	var found string
	lines := strings.Split(raw, "\n")
	if len(lines) > maxImageLines {
		return "", errors.New("gemini image RPC response has too many frames")
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == ")]}'" || isRPCFrameLength(line) {
			continue // XSSI prefix and RPC length frames
		}
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var rows []json.RawMessage
		if json.Unmarshal([]byte(line), &rows) != nil || len(rows) == 0 {
			continue
		}
		for _, rawRow := range rows {
			var row []json.RawMessage
			if json.Unmarshal(rawRow, &row) != nil || len(row) < 3 {
				continue
			}
			var kind string
			if json.Unmarshal(row[0], &kind) != nil || kind != "wrb.fr" {
				continue
			}
			var payload string
			if json.Unmarshal(row[2], &payload) != nil {
				continue
			}
			var inner []json.RawMessage
			if json.Unmarshal([]byte(payload), &inner) != nil {
				continue
			}
			if len(inner) <= 4 {
				continue
			}
			var candidates []json.RawMessage
			if json.Unmarshal(inner[4], &candidates) != nil {
				continue
			}
			for _, item := range candidates {
				var candidate []json.RawMessage
				if json.Unmarshal(item, &candidate) != nil || len(candidate) <= 12 || string(candidate[12]) == "null" {
					continue
				}
				// candidate[12] is the generated-image attachment branch.
				var attachment any
				if json.Unmarshal(candidate[12], &attachment) != nil {
					continue
				}
				count := 0
				var walk func(any, int) error
				walk = func(v any, depth int) error {
					count++
					if count > 2048 || depth > 12 {
						return errors.New("gemini image attachment exceeds parsing bounds")
					}
					switch node := v.(type) {
					case string:
						if strings.HasPrefix(node, "https://") {
							if u, ok := generatedImageURL(node); ok {
								if found != "" && found != u {
									return errors.New("gemini image response contains multiple images")
								}
								found = u
							}
						}
					case []any:
						for _, child := range node {
							if err := walk(child, depth+1); err != nil {
								return err
							}
						}
					}
					return nil
				}
				if err := walk(attachment, 0); err != nil {
					return "", err
				}
			}
		}
	}
	if found == "" {
		return "", errors.New("gemini image response contains no supported generated image")
	}
	return found, nil
}

func scanInlineImageURL(payload string) (string, bool) {
	var v any
	if json.Unmarshal([]byte(payload), &v) != nil {
		return "", false
	}
	found := ""
	var walk func(node any, depth int) bool
	walk = func(node any, depth int) bool {
		if depth > 12 || found != "" {
			return found != ""
		}
		if s, ok := node.(string); ok {
			if strings.Contains(s, "googleusercontent.com") {
				if u, ok := generatedImageURL(s); ok {
					found = u
					return true
				}
			}
			return false
		}
		if arr, ok := node.([]any); ok {
			for _, child := range arr {
				if walk(child, depth+1) {
					return true
				}
			}
		}
		return false
	}
	if walk(v, 0) && found != "" {
		return found, true
	}
	return "", false
}

func isRPCFrameLength(s string) bool {
	if len(s) == 0 || len(s) > 10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (a *Adapter) downloadGeneratedImage(ctx context.Context, rawURL string) (provider.ImageResponse, error) {
	return a.downloadGeneratedImageWithCookie(ctx, rawURL, "", false)
}

func (a *Adapter) downloadGeneratedImageWithCookie(ctx context.Context, rawURL, cookie string, withCookie bool) (provider.ImageResponse, error) {
	if _, ok := generatedImageURL(rawURL); !ok {
		return provider.ImageResponse{}, errors.New("gemini generated image URL is not allowed")
	}
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.Timeout = 30 * time.Second
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("gemini image redirect rejected")
		}
		if u, ok := generatedImageRedirectURL(req.URL.String()); ok {
			_ = u
			return validateImageRedirect(req, via)
		}
		return errors.New("gemini image redirect rejected")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return provider.ImageResponse{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://gemini.google.com/")
	if withCookie && cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := copyClient.Do(req)
	if err != nil {
		return provider.ImageResponse{}, fmt.Errorf("gemini image download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return provider.ImageResponse{}, fmt.Errorf("gemini image download HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxGeneratedImageBytes {
		return provider.ImageResponse{}, errors.New("gemini image exceeds maximum size")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedImageBytes+1))
	if err != nil {
		return provider.ImageResponse{}, fmt.Errorf("read gemini image: %w", err)
	}
	if len(data) == 0 || len(data) > maxGeneratedImageBytes {
		return provider.ImageResponse{}, errors.New("gemini image is empty or exceeds maximum size")
	}
	declared, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return provider.ImageResponse{}, errors.New("gemini image has invalid content type")
	}
	detected := http.DetectContentType(data)
	if declared != detected || (detected != "image/png" && detected != "image/jpeg" && detected != "image/webp" && detected != "image/gif") {
		return provider.ImageResponse{}, errors.New("gemini image MIME does not match supported image bytes")
	}
	return provider.ImageResponse{Data: data, MIME: detected}, nil
}
