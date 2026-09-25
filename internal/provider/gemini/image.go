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
	raw, err := a.generateRaw(ctx, "Generate an image of: "+req.Prompt, req.Model, cookie, ConversationMetadata{}, nil, maxImageRPCBytes)
	if err != nil {
		return provider.ImageResponse{}, err
	}
	imageURL, err := extractGeneratedImageURL(raw)
	if err != nil {
		return provider.ImageResponse{}, err
	}
	return a.downloadGeneratedImage(ctx, imageURL)
}

func generatedImageURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "lh3.googleusercontent.com" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.RawQuery != "" {
		return "", false
	}
	if !strings.HasPrefix(u.Path, "/gg/") && !strings.HasPrefix(u.Path, "/gg-dl/") {
		return "", false
	}
	if len(u.Path) > 2048 || len(u.Path) < 10 || strings.ContainsAny(u.Path, "\\%") || u.EscapedPath() != u.Path {
		return "", false
	}
	return raw, true
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
			return "", errors.New("invalid gemini image RPC frame")
		}
		var rows []json.RawMessage
		if json.Unmarshal([]byte(line), &rows) != nil || len(rows) != 1 {
			return "", errors.New("invalid gemini image RPC frame")
		}
		var row []json.RawMessage
		if json.Unmarshal(rows[0], &row) != nil || len(row) < 3 {
			return "", errors.New("invalid gemini image RPC row")
		}
		var kind string
		if json.Unmarshal(row[0], &kind) != nil || kind != "wrb.fr" {
			continue
		}
		var payload string
		if json.Unmarshal(row[2], &payload) != nil {
			return "", errors.New("invalid gemini image RPC payload")
		}
		var inner []json.RawMessage
		if json.Unmarshal([]byte(payload), &inner) != nil || len(inner) <= 4 {
			return "", errors.New("invalid gemini image response")
		}
		var candidates []json.RawMessage
		if json.Unmarshal(inner[4], &candidates) != nil {
			return "", errors.New("invalid gemini image candidates")
		}
		for _, item := range candidates {
			var candidate []json.RawMessage
			if json.Unmarshal(item, &candidate) != nil || len(candidate) <= 12 || string(candidate[12]) == "null" {
				continue
			}
			// candidate[12] is the generated-image attachment branch.
			var attachment any
			if json.Unmarshal(candidate[12], &attachment) != nil {
				return "", errors.New("invalid gemini image attachment")
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
	if found == "" {
		return "", errors.New("gemini image response contains no supported generated image")
	}
	return found, nil
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
	if _, ok := generatedImageURL(rawURL); !ok {
		return provider.ImageResponse{}, errors.New("gemini generated image URL is not allowed")
	}
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.Timeout = 30 * time.Second
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("gemini image redirect rejected")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return provider.ImageResponse{}, err
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
