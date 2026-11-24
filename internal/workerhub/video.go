package workerhub

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"sever_provider/internal/provider"
)

const (
	// maxGeneratedVideoBytes matches the media store ceiling in the job runner.
	maxGeneratedVideoBytes = 100 * 1024 * 1024
	videoRequestTimeout    = 45 * time.Minute
)

// Video sends a text-to-video request to a worker advertising the model and
// returns the generated MP4 bytes. Worker output is untrusted: exactly one
// bounded video result is accepted.
func (h *Hub) Video(ctx context.Context, req provider.VideoRequest) (provider.VideoResponse, error) {
	if h == nil || strings.TrimSpace(req.Model) == "" {
		return provider.VideoResponse{}, ErrNoWorker
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return provider.VideoResponse{}, errors.New("video prompt is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, videoRequestTimeout)
	defer cancel()

	payload := map[string]any{"prompt": req.Prompt}
	if req.Size != "" {
		payload["size"] = req.Size
	}
	if req.Duration > 0 {
		payload["duration"] = req.Duration
	}
	stream, err := h.Dispatch(ctx, OutboundRequest{Model: req.Model, Type: "video_request", Payload: payload})
	if err != nil {
		return provider.VideoResponse{}, err
	}

	var video []byte
	var mime string
	for event := range stream.Events {
		switch event.Type {
		case "error":
			// Reported through stream.Err once the channel closes.
		case "video_progress", "progress":
			// Progress is advisory; the final result carries the bytes.
		case "video_result":
			if video != nil {
				return provider.VideoResponse{}, errors.New("worker returned more than one video result")
			}
			mime = strings.ToLower(strings.TrimSpace(event.MIME))
			if !strings.HasPrefix(mime, "video/") {
				return provider.VideoResponse{}, fmt.Errorf("unsupported worker video MIME %q", event.MIME)
			}
			if event.VideoBase64 == "" || base64.StdEncoding.DecodedLen(len(event.VideoBase64)) > maxGeneratedVideoBytes+2 {
				return provider.VideoResponse{}, errors.New("worker video is empty or exceeds limit")
			}
			decoded, decodeErr := base64.StdEncoding.Strict().DecodeString(event.VideoBase64)
			if decodeErr != nil || len(decoded) == 0 || len(decoded) > maxGeneratedVideoBytes {
				return provider.VideoResponse{}, errors.New("invalid or oversized worker video")
			}
			if strings.Contains(mime, "mp4") && (len(decoded) < 12 || !bytes.Equal(decoded[4:8], []byte("ftyp"))) {
				return provider.VideoResponse{}, errors.New("worker video is not an MP4 file")
			}
			video = decoded
		default:
			return provider.VideoResponse{}, fmt.Errorf("unexpected worker video event %q", event.Type)
		}
	}
	if streamErr, ok := <-stream.Err; ok && streamErr != nil {
		if ctx.Err() != nil {
			return provider.VideoResponse{}, ctx.Err()
		}
		return provider.VideoResponse{}, streamErr
	}
	if ctx.Err() != nil {
		return provider.VideoResponse{}, ctx.Err()
	}
	if video == nil {
		return provider.VideoResponse{}, errors.New("worker video response ended without video_result")
	}
	return provider.VideoResponse{Data: video, MIME: mime}, nil
}
