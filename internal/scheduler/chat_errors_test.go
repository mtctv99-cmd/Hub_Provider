package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
)

func TestChatPreservesUpstreamErrors(t *testing.T) {
	for _, stream := range []bool{false, true} {
		models := NewModelRegistry()
		_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}})
		upstream := errors.New("HTTP 429 rate limit exceeded")
		source := &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 1}}}
		d := &Dispatcher{Models: models, Credentials: source, Adapters: singleRegistry{&countAdapter{callErr: upstream}}, Leases: NewLeaseManager()}
		var err error
		if stream {
			_, err = d.StreamChat(context.Background(), provider.ChatRequest{Model: "test-model"})
		} else {
			_, err = d.Chat(context.Background(), provider.ChatRequest{Model: "test-model"})
		}
		if !errors.Is(err, upstream) || errors.Is(err, ErrNoAvailableCredential) {
			t.Fatalf("stream=%t lost upstream error: %v", stream, err)
		}
		if !strings.Contains(err.Error(), "model=test-model provider=test") {
			t.Fatalf("missing diagnostic context: %v", err)
		}
	}
}

type emptyAdapter struct {
	upstream provider.UpstreamState
}

func (e *emptyAdapter) Name() string { return "test" }
func (e *emptyAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat}
}
func (e *emptyAdapter) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	return provider.ChatResponse{Content: "", Upstream: e.upstream}, nil
}
func (e *emptyAdapter) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Done: true, Upstream: e.upstream}
	close(ch)
	return ch, nil
}
func (e *emptyAdapter) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (e *emptyAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}
func (e *emptyAdapter) Video(context.Context, provider.VideoRequest, []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

func TestChatEmptyContentBlocksSessionAndRecordsFailure(t *testing.T) {
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}})
	source := &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 1}}}
	adapter := &emptyAdapter{
		upstream: provider.UpstreamState{ConversationID: "conv-123"},
	}
	d := &Dispatcher{
		Models:      models,
		Credentials: source,
		Adapters:    singleRegistry{adapter},
		Leases:      NewLeaseManager(),
	}

	_, err := d.Chat(context.Background(), provider.ChatRequest{Model: "test-model", SessionID: "sess-1"})
	if err == nil {
		t.Fatal("expected empty content chat to fail")
	}
	if !strings.Contains(err.Error(), "empty_response") {
		t.Fatalf("expected error to contain empty_response, got: %v", err)
	}
	if len(source.successes) > 0 {
		t.Fatalf("empty response was recorded as success: %v", source.successes)
	}
	if len(source.statusSets) == 0 {
		t.Fatal("expected credential failure to be recorded")
	}
}
