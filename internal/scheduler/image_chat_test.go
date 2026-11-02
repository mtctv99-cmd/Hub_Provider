package scheduler

import (
	"context"
	"testing"

	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
	"sever_provider/internal/session"
)

type imageChatAdapter struct {
	countAdapter
	data         []byte
	lastImage    *provider.ImageRequest
	returnUpstrm provider.UpstreamState
}

func (a *imageChatAdapter) Chat(ctx context.Context, req provider.ChatRequest, secret []byte) (provider.ChatResponse, error) {
	a.calls++
	a.lastRequest = req
	if a.callErr != nil {
		return provider.ChatResponse{}, a.callErr
	}
	return provider.ChatResponse{ID: "img-1", ImageData: a.data, ImageMIME: "image/png"}, nil
}

func (a *imageChatAdapter) Image(_ context.Context, req provider.ImageRequest, _ []byte) (provider.ImageResponse, error) {
	a.calls++
	a.lastImage = &req
	if a.callErr != nil {
		return provider.ImageResponse{}, a.callErr
	}
	return provider.ImageResponse{Data: a.data, MIME: "image/png", Upstream: a.returnUpstrm}, nil
}

type mapRegistry map[string]provider.Adapter

func (m mapRegistry) Get(name string) (provider.Adapter, bool) {
	adapter, ok := m[name]
	return adapter, ok
}

func TestDispatcherChatImageOnlyNotRejectedEmpty(t *testing.T) {
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "img", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5})
	adapter := &imageChatAdapter{data: []byte("png")}
	d := &Dispatcher{
		Models:      models,
		Credentials: &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}},
		Adapters:    mapRegistry{"test": adapter},
		Leases:      NewLeaseManager(),
		Breakers:    NewBreakerRegistry(),
	}
	resp, err := d.Chat(context.Background(), provider.ChatRequest{Model: "img", ImageGeneration: true, Messages: []provider.Message{{Role: "user", Content: "a cat"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if len(resp.ImageData) == 0 || !adapter.lastRequest.ImageGeneration {
		t.Fatalf("image response missing: data=%d request=%+v", len(resp.ImageData), adapter.lastRequest)
	}
}

func TestImageFailoverSkipsNonImageModel(t *testing.T) {
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "img", Provider: "img", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, FailoverTo: []string{"text"}, TimeoutSec: 5})
	_ = models.Register(ModelSpec{ID: "text", Provider: "text", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 5})
	imgAdapter := &imageChatAdapter{countAdapter: countAdapter{callErr: provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "no image")}}
	textAdapter := &countAdapter{}
	d := &Dispatcher{
		Models:      models,
		Credentials: &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}},
		Adapters:    mapRegistry{"img": imgAdapter, "text": textAdapter},
		Leases:      NewLeaseManager(),
		Breakers:    NewBreakerRegistry(),
	}
	if _, err := d.Chat(context.Background(), provider.ChatRequest{Model: "img", ImageGeneration: true, Messages: []provider.Message{{Role: "user", Content: "a cat"}}}); err == nil {
		t.Fatal("expected image generation failure")
	}
	if textAdapter.calls != 0 {
		t.Fatalf("text-only fallback was called %d times", textAdapter.calls)
	}
}

// An image request must stick to the credential that served it, the way chat
// binds its session; otherwise every image can land on a different Google
// account even when the client sends the same X-Session-ID.
func TestImageBindsSessionToCredential(t *testing.T) {
	sessions := session.New(0, 0)
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "img", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5})
	adapter := &imageChatAdapter{data: []byte("png")}
	d := &Dispatcher{
		Models:      models,
		Sessions:    sessions,
		Credentials: &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}},
		Adapters:    mapRegistry{"test": adapter},
		Leases:      NewLeaseManager(),
		Breakers:    NewBreakerRegistry(),
	}
	if _, err := d.Image(context.Background(), provider.ImageRequest{Model: "img", Prompt: "a cat", SessionID: "s1"}); err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	binding, ok := sessions.Get("s1")
	if !ok || binding.CredentialID != "c1" {
		t.Fatalf("image session not bound to serving credential: %+v ok=%v", binding, ok)
	}
}

// A repeated image session must continue the Gemini conversation, not just the
// credential, so style/context carry across turns.
func TestImageContinuesSessionConversation(t *testing.T) {
	sessions := session.New(0, 0)
	if err := sessions.Commit("s2", "c1", "test", provider.UpstreamState{ConversationID: "c_old", ResponseID: "r_old", ChoiceID: "rc_old"}); err != nil {
		t.Fatal(err)
	}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "img", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5})
	adapter := &imageChatAdapter{data: []byte("png"), returnUpstrm: provider.UpstreamState{ConversationID: "c_new", ResponseID: "r_new"}}
	d := &Dispatcher{
		Models:      models,
		Sessions:    sessions,
		Credentials: &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}},
		Adapters:    mapRegistry{"test": adapter},
		Leases:      NewLeaseManager(),
		Breakers:    NewBreakerRegistry(),
	}
	if _, err := d.Image(context.Background(), provider.ImageRequest{Model: "img", Prompt: "same style", SessionID: "s2"}); err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	if adapter.lastImage == nil || adapter.lastImage.Upstream.ConversationID != "c_old" {
		t.Fatalf("adapter did not receive the bound conversation: %+v", adapter.lastImage)
	}
	binding, ok := sessions.Get("s2")
	if !ok || binding.UpstreamConversationID != "c_new" || binding.CredentialID != "c1" {
		t.Fatalf("session did not commit the new turn: %+v ok=%v", binding, ok)
	}
}

// An image-mode model fronts the image-capable model, so the adapter sees a
// model it knows while the client keeps using the public ID.
func TestProviderModelRequestUsesBaseModel(t *testing.T) {
	req := provider.ChatRequest{Model: "gemini-image", ImageGeneration: true}
	got := providerModelRequest(req, ModelSpec{ID: "gemini-image", BaseModel: "gemini-3.8-flash"})
	if got.Model != "gemini-3.8-flash" || !got.ImageGeneration {
		t.Fatalf("base model rewrite = %q image=%v", got.Model, got.ImageGeneration)
	}
}
