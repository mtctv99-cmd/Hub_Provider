package workerhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"sever_provider/internal/provider"
)

// Available reports whether a worker advertises this exact model.
func (h *Hub) Available(model string) bool {
	if h == nil || strings.TrimSpace(model) == "" {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, w := range h.workers {
		if !w.closed && w.canServe(model) {
			return true
		}
	}
	return false
}

// Chat sends the complete conversation to the inbound worker and waits for a terminal result.
func (h *Hub) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := h.chatStream(ctx, req)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	var text strings.Builder
	var usage provider.Usage
	terminal := false
	rawBytes := 0
	textFrames := 0
	for event := range stream.Events {
		if event.Type == "error" {
			continue
		}
		if len(event.Raw) > 0 {
			rawBytes += len(event.Raw)
		}
		if event.Delta != "" || event.Text != "" {
			textFrames++
		}
		text.WriteString(event.Delta)
		text.WriteString(event.Text)
		if event.Type == "token_end" || event.Type == "job_result" || event.Done {
			terminal = true
		}
		if len(event.Raw) > 0 {
			var payload struct {
				Usage provider.Usage `json:"usage"`
			}
			if json.Unmarshal(event.Raw, &payload) == nil && payload.Usage != (provider.Usage{}) {
				usage = payload.Usage
			}
		}
	}
	if streamErr, ok := <-stream.Err; ok && streamErr != nil {
		return provider.ChatResponse{}, streamErr
	}
	if !terminal {
		return provider.ChatResponse{}, errors.New("worker response ended without a terminal event")
	}
	// Diagnose before returning: distinguish a worker that emitted no text
	// from one whose payload the parser could not extract. Counts only, no
	// payload or credential material leaves this function.
	if strings.TrimSpace(text.String()) == "" {
		if textFrames > 0 || rawBytes > 0 {
			return provider.ChatResponse{}, provider.NewEmptyResponseError(provider.EmptyReasonParserFailed, fmt.Sprintf("worker sent %d text frames (%d raw bytes) but extracted no text", textFrames, rawBytes))
		}
		return provider.ChatResponse{}, provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "worker completed without emitting text deltas")
	}
	return provider.ChatResponse{ID: "chatcmpl-" + newRequestID(), Model: req.Model, Content: text.String(), Usage: usage}, nil
}

func (h *Hub) chatStream(ctx context.Context, req provider.ChatRequest) (*Stream, error) {
	if h == nil {
		return nil, ErrNoWorker
	}
	if req.Model == "" {
		return nil, ErrNoWorker
	}
	return h.Submit(ctx, req.Model, map[string]any{"messages": req.Messages, "stream": req.Stream, "temperature": req.Temperature, "max_tokens": req.MaxTokens})
}

// StreamChat adapts worker token events into provider chunks. The request context
// must remain alive until the returned channel closes; cancellation frees its slot.
func (h *Hub) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream, err := h.chatStream(ctx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	out := make(chan provider.StreamChunk)
	go func() {
		defer close(out)
		defer cancel()
		terminal := false
		var emittedText strings.Builder
		emittedToolCalls := false
		rawBytes := 0
		for event := range stream.Events {
			if event.Type == "error" {
				continue
			}
			chunk := provider.StreamChunk{Content: event.Delta + event.Text}
			if len(event.Raw) > 0 {
				rawBytes += len(event.Raw)
			}
			emittedText.WriteString(chunk.Content)
			if event.Type == "token_end" || event.Type == "job_result" || event.Done {
				chunk.Done = true
				terminal = true
			}
			if len(event.Raw) > 0 {
				var payload struct {
					Usage provider.Usage `json:"usage"`
				}
				if json.Unmarshal(event.Raw, &payload) == nil && payload.Usage != (provider.Usage{}) {
					chunk.Usage = &payload.Usage
				}
			}
			if chunk.Done && strings.TrimSpace(emittedText.String()) == "" && !emittedToolCalls {
				// Block empty Done turns before the dispatcher can commit the
				// session: counts only, no payload or credential material.
				reason := provider.EmptyReasonUpstreamEmpty
				if rawBytes > 0 {
					reason = provider.EmptyReasonParserFailed
				}
				chunk.Err = provider.NewEmptyResponseError(reason, fmt.Sprintf("worker stream completed without text content (raw_bytes=%d)", rawBytes))
				chunk.Done = false
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
			if chunk.Done || chunk.Err != nil {
				return
			}
		}
		if streamErr, ok := <-stream.Err; ok && streamErr != nil {
			select {
			case out <- provider.StreamChunk{Err: streamErr}:
			case <-ctx.Done():
			}
		} else if !terminal {
			select {
			case out <- provider.StreamChunk{Err: fmt.Errorf("worker response ended without terminal event")}:
			case <-ctx.Done():
			}
		} else if strings.TrimSpace(emittedText.String()) == "" && !emittedToolCalls {
			reason := provider.EmptyReasonUpstreamEmpty
			if rawBytes > 0 {
				reason = provider.EmptyReasonParserFailed
			}
			select {
			case out <- provider.StreamChunk{Err: provider.NewEmptyResponseError(reason, fmt.Sprintf("worker stream closed without text content (raw_bytes=%d)", rawBytes))}:
			case <-ctx.Done():
			}
		}
	}()
	return out, nil
}
