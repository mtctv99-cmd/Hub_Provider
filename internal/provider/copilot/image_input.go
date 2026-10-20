package copilot

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
)

type imageAnnotation struct {
	ID                        string `json:"id"`
	MessageAnnotationType     string `json:"messageAnnotationType"`
	MessageAnnotationMetadata struct {
		Type       string `json:"@type"`
		Annotation string `json:"annotationType"`
		FileType   string `json:"fileType"`
		FileName   string `json:"fileName"`
	} `json:"messageAnnotationMetadata"`
}

func (a *Adapter) uploadImages(ctx context.Context, tokenURL, conversationID string, images []string) ([]imageAnnotation, error) {
	if len(images) == 0 {
		return nil, nil
	}
	u, err := url.Parse(tokenURL)
	if err != nil {
		return nil, errors.New("copilot vision: invalid token endpoint")
	}
	token := u.Query().Get("access_token")
	if token == "" {
		return nil, errors.New("copilot vision: token endpoint has no access token")
	}
	endpoint := strings.TrimSpace(a.UploadURL)
	if endpoint == "" {
		endpoint = "https://substrate.office.com/m365Copilot/UploadFile"
	}
	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	uploadClient := &http.Client{
		Transport: client.Transport,
		Jar:       client.Jar,
		Timeout:   client.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	annotations := make([]imageAnnotation, 0, len(images))
	for i, dataURI := range images {
		if _, err := decodeImageDataURI(dataURI); err != nil {
			return nil, fmt.Errorf("copilot vision: image %d: %w", i+1, err)
		}
		mediaType := strings.SplitN(strings.TrimPrefix(dataURI, "data:"), ";", 2)[0]
		fileType, _, err := mime.ParseMediaType(mediaType)
		if err != nil || !strings.HasPrefix(fileType, "image/") {
			return nil, fmt.Errorf("copilot vision: image %d has invalid media type", i+1)
		}
		ext := strings.TrimPrefix(fileType, "image/")
		if base, _, ok := strings.Cut(ext, "+"); ok {
			ext = base
		}
		if ext == "jpeg" {
			ext = "jpg"
		}
		form := url.Values{}
		form.Set("scenario", "UploadImage")
		form.Set("conversationId", conversationID)
		form.Set("FileBase64", dataURI)
		form.Add("optionsSets", "cwcgptvsan")
		form.Add("optionsSets", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, fmt.Errorf("copilot vision: build image %d upload: %w", i+1, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Scenario", "OfficeWebIncludedCopilot")
		req.Header.Set("X-Variants", "feature.EnableImageSupportInUploadFile")
		req.Header.Set("Origin", "https://m365.cloud.microsoft")
		req.Header.Set("Referer", "https://m365.cloud.microsoft/")
		resp, err := uploadClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("copilot vision: image %d upload failed: %w", i+1, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("copilot vision: image %d upload response failed: %w", i+1, readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			responseText := strings.TrimSpace(string(body))
			if len(responseText) > 240 {
				responseText = responseText[:240]
			}
			return nil, fmt.Errorf("copilot vision: image %d upload HTTP %d: %s", i+1, resp.StatusCode, responseText)
		}
		var result struct {
			DocID string `json:"docId"`
		}
		if err := json.Unmarshal(body, &result); err != nil || strings.TrimSpace(result.DocID) == "" {
			return nil, fmt.Errorf("copilot vision: image %d upload returned no document ID", i+1)
		}
		annotation := imageAnnotation{ID: result.DocID, MessageAnnotationType: "ImageFile"}
		annotation.MessageAnnotationMetadata.Type = "File"
		annotation.MessageAnnotationMetadata.Annotation = "File"
		annotation.MessageAnnotationMetadata.FileType = ext
		annotation.MessageAnnotationMetadata.FileName = fmt.Sprintf("image-%s.%s", randomHex(4), ext)
		annotations = append(annotations, annotation)
	}
	return annotations, nil
}
