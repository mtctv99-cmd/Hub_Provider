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
	maxSpeechAudioBytes = 32 * 1024 * 1024
	speechTimeout       = 2 * time.Minute
)

// Speech sends a typed TTS request to a worker advertising the requested model.
// Worker audio is untrusted: only a single bounded WAV result is accepted.
func (h *Hub) Speech(ctx context.Context, req provider.SpeechRequest) ([]byte, string, error) {
	if h == nil || strings.TrimSpace(req.Model) == "" {
		return nil, "", ErrNoWorker
	}
	if strings.TrimSpace(req.Input) == "" {
		return nil, "", errors.New("speech text is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, speechTimeout)
	defer cancel()
	voice := strings.TrimSpace(req.Voice)
	if voice == "" && strings.HasPrefix(req.Model, "piper:") {
		voice = strings.TrimPrefix(req.Model, "piper:")
	}
	stream, err := h.Dispatch(ctx, OutboundRequest{
		Model: req.Model, Type: "tts_request",
		Payload: map[string]string{"text": req.Input, "voice_id": voice},
	})
	if err != nil {
		return nil, "", err
	}
	var audio []byte
	for event := range stream.Events {
		if event.Type == "error" {
			continue // Read the error from stream.Err after the event channel closes.
		}
		if event.Type != "audio_result" || audio != nil {
			return nil, "", fmt.Errorf("unexpected worker speech event %q", event.Type)
		}
		if event.MIME != "audio/wav" {
			return nil, "", fmt.Errorf("unsupported worker audio MIME %q", event.MIME)
		}
		if event.AudioBase64 == "" || base64.StdEncoding.DecodedLen(len(event.AudioBase64)) > maxSpeechAudioBytes+2 {
			return nil, "", errors.New("worker audio is empty or exceeds limit")
		}
		audio, err = base64.StdEncoding.Strict().DecodeString(event.AudioBase64)
		if err != nil || len(audio) == 0 || len(audio) > maxSpeechAudioBytes {
			return nil, "", errors.New("invalid or oversized worker audio")
		}
		if len(audio) < 12 || !bytes.Equal(audio[:4], []byte("RIFF")) || !bytes.Equal(audio[8:12], []byte("WAVE")) {
			return nil, "", errors.New("worker audio is not a WAV file")
		}
	}
	if streamErr, ok := <-stream.Err; ok && streamErr != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", streamErr
	}
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if audio == nil {
		return nil, "", errors.New("worker speech response ended without audio_result")
	}
	return audio, "audio/wav", nil
}
