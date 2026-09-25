package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"sever_provider/internal/provider"
)

const maxImageBytes = 16 * 1024 * 1024 // 16 MB

func ParseImageData(rawURL string) ([]byte, string, string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, "", "", errors.New("empty image URL")
	}

	if strings.HasPrefix(rawURL, "data:") {
		idx := strings.Index(rawURL, ",")
		if idx == -1 {
			return nil, "", "", errors.New("invalid data URL format")
		}
		header := rawURL[:idx]
		payload := rawURL[idx+1:]

		mimeType := "image/png"
		if strings.HasPrefix(header, "data:") {
			parts := strings.Split(strings.TrimPrefix(header, "data:"), ";")
			if len(parts) > 0 && parts[0] != "" {
				mimeType = strings.TrimSpace(parts[0])
			}
		}

		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(payload)
			if err != nil {
				return nil, "", "", fmt.Errorf("decode base64 image: %w", err)
			}
		}

		if len(data) > maxImageBytes {
			return nil, "", "", fmt.Errorf("image exceeds maximum size of 16MB (%d bytes)", len(data))
		}

		ext := extensionForMime(mimeType)
		filename := fmt.Sprintf("image_%d%s", time.Now().UnixNano(), ext)
		return data, filename, mimeType, nil
	}

	// Remote HTTP/HTTPS URL
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", "", fmt.Errorf("unsupported image URL scheme: %s", rawURL)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, "", "", fmt.Errorf("fetch remote image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, "", "", fmt.Errorf("fetch remote image HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("read remote image: %w", err)
	}
	if len(data) > maxImageBytes {
		return nil, "", "", errors.New("remote image exceeds maximum size of 16MB")
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	mimeType = strings.Split(mimeType, ";")[0]

	ext := extensionForMime(mimeType)
	base := filepath.Base(u.Path)
	if base == "" || base == "." || base == "/" {
		base = fmt.Sprintf("image_%d%s", time.Now().UnixNano(), ext)
	}
	return data, base, mimeType, nil
}

func (a *Adapter) UploadImage(ctx context.Context, imageBytes []byte, filename, mimeType, cookie string, tokens PageTokens) (UploadedFile, error) {
	if len(imageBytes) == 0 {
		return UploadedFile{}, errors.New("cannot upload empty image")
	}
	if len(imageBytes) > maxImageBytes {
		return UploadedFile{}, fmt.Errorf("image exceeds 16MB (%d bytes)", len(imageBytes))
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	if filename == "" {
		filename = fmt.Sprintf("image_%d%s", time.Now().UnixNano(), extensionForMime(mimeType))
	}

	pushID := tokens.PushID
	if pushID == "" {
		pushID = "feeds/mcudyrk2a4khkz"
	}
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}

	// Step 1: Initiate Scotty resumable upload
	uploadBase := strings.TrimRight(a.UploadBaseURL, "/")
	if uploadBase == "" {
		uploadBase = "https://push.clients6.google.com"
	}
	startURL := uploadBase + "/upload/"
	startReq, err := http.NewRequestWithContext(ctx, http.MethodPost, startURL, strings.NewReader(fmt.Sprintf("File name: %s", filename)))
	if err != nil {
		return UploadedFile{}, fmt.Errorf("create scotty start request: %w", err)
	}
	startReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	startReq.Header.Set("X-Goog-Upload-Command", "start")
	startReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startReq.Header.Set("X-Goog-Upload-Header-Content-Length", fmt.Sprintf("%d", len(imageBytes)))
	startReq.Header.Set("Push-ID", pushID)
	startReq.Header.Set("X-Tenant-Id", "bard-storage")
	startReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	startReq.Header.Set("Origin", "https://gemini.google.com")
	startReq.Header.Set("Referer", "https://gemini.google.com/")
	if cookie != "" {
		startReq.Header.Set("Cookie", cookie)
	}
	if sapisid := SapisidFromCookie(cookie); sapisid != "" {
		startReq.Header.Set("Authorization", SapisidHash(sapisid, time.Now()))
	}

	startResp, err := client.Do(startReq)
	if err != nil {
		return UploadedFile{}, fmt.Errorf("scotty start upload: %w", err)
	}
	defer startResp.Body.Close()

	if startResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(startResp.Body, 1024))
		return UploadedFile{}, fmt.Errorf("scotty start upload HTTP %d: %s", startResp.StatusCode, string(respBody))
	}

	uploadURL := startResp.Header.Get("X-Goog-Upload-URL")
	if uploadURL == "" {
		if uploadID := startResp.Header.Get("X-GUploader-UploadID"); uploadID != "" {
			uploadURL = fmt.Sprintf("https://push.clients6.google.com/upload/?upload_id=%s&upload_protocol=resumable", uploadID)
		}
	}
	if uploadURL == "" {
		return UploadedFile{}, errors.New("scotty did not return an upload URL")
	}

	// Step 2: Upload raw image bytes
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(imageBytes))
	if err != nil {
		return UploadedFile{}, fmt.Errorf("create scotty upload request: %w", err)
	}
	upReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	upReq.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	upReq.Header.Set("X-Goog-Upload-Offset", "0")
	upReq.Header.Set("Push-ID", pushID)
	upReq.Header.Set("X-Tenant-Id", "bard-storage")
	upReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	upReq.Header.Set("Origin", "https://gemini.google.com")
	upReq.Header.Set("Referer", "https://gemini.google.com/")
	if cookie != "" {
		upReq.Header.Set("Cookie", cookie)
	}
	if sapisid := SapisidFromCookie(cookie); sapisid != "" {
		upReq.Header.Set("Authorization", SapisidHash(sapisid, time.Now()))
	}

	upResp, err := client.Do(upReq)
	if err != nil {
		return UploadedFile{}, fmt.Errorf("scotty upload bytes: %w", err)
	}
	defer upResp.Body.Close()

	upBody, err := io.ReadAll(io.LimitReader(upResp.Body, 1024))
	if err != nil {
		return UploadedFile{}, fmt.Errorf("read scotty upload response: %w", err)
	}
	if upResp.StatusCode != http.StatusOK {
		return UploadedFile{}, fmt.Errorf("scotty upload HTTP %d: %s", upResp.StatusCode, string(upBody))
	}

	fileID := strings.TrimSpace(string(upBody))
	if fileID == "" || !strings.HasPrefix(fileID, "/contrib_service/") {
		return UploadedFile{}, fmt.Errorf("invalid scotty file ID: %s", fileID)
	}

	// Step 3: Register file via ProcessFile RPC
	bl := tokens.BL
	if bl == "" {
		bl = "boq_assistant-bard-web-server"
	}
	fsid := tokens.FSID

	innerReq := []any{
		[]any{
			[]any{fileID, nil, 1, mimeType},
			filename,
			nil, nil, nil, nil, nil, nil,
			[]any{0},
		},
		nil,
		1,
		[]any{"en"},
	}
	innerJSON, _ := json.Marshal(innerReq)
	outerReq := []any{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outerReq)

	vals := url.Values{}
	if tokens.XSRF != "" {
		vals.Set("at", tokens.XSRF)
	}
	vals.Set("f.req", string(outerJSON))

	procURL := fmt.Sprintf("%s/_/BardChatUi/data/assistant.lamda.BardFrontendService/ProcessFile?bl=%s&f.sid=%s&hl=en&_reqid=%d&rt=c",
		a.BaseURL, url.QueryEscape(bl), url.QueryEscape(fsid), time.Now().UnixNano()%1000000)

	procReq, err := http.NewRequestWithContext(ctx, http.MethodPost, procURL, strings.NewReader(vals.Encode()))
	if err == nil {
		procReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
		procReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		procReq.Header.Set("Origin", "https://gemini.google.com")
		procReq.Header.Set("Referer", "https://gemini.google.com/")
		if cookie != "" {
			procReq.Header.Set("Cookie", cookie)
		}
		if sapisid := SapisidFromCookie(cookie); sapisid != "" {
			procReq.Header.Set("Authorization", SapisidHash(sapisid, time.Now()))
		}
		if procResp, procErr := client.Do(procReq); procErr == nil {
			_ = procResp.Body.Close()
		}
	}

	return UploadedFile{ID: fileID, Name: filename}, nil
}

func (a *Adapter) ExtractAndUploadFiles(ctx context.Context, messages []provider.Message, cookie string, tokens PageTokens) ([]UploadedFile, error) {
	var files []UploadedFile
	for _, m := range messages {
		for _, p := range m.Parts {
			if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
				data, filename, mimeType, err := ParseImageData(p.ImageURL.URL)
				if err != nil {
					return nil, fmt.Errorf("parse image payload: %w", err)
				}
				uploaded, err := a.UploadImage(ctx, data, filename, mimeType, cookie, tokens)
				if err != nil {
					return nil, fmt.Errorf("gemini scotty upload: %w", err)
				}
				files = append(files, uploaded)
			}
		}
	}
	return files, nil
}

func extensionForMime(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}
