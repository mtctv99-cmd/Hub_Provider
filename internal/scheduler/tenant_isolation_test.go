package scheduler

import (
	"context"
	"strings"
	"testing"

	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
	"sever_provider/internal/session"
	"sever_provider/internal/tenant"
)

func newTenantDispatcher(sessions *session.Manager, adapter provider.Adapter) *Dispatcher {
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})
	return &Dispatcher{
		Models: models,
		Credentials: &trackingSource{creds: []identity.Credential{
			{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2},
			{ID: "c2", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2},
		}},
		Adapters: singleRegistry{adapter},
		Leases:   NewLeaseManager(),
		Sessions: sessions,
		Pools:    nil,
		Breakers: NewBreakerRegistry(),
	}
}

// H3 regression: two tenants sending the same X-Session-ID must get separate
// bindings and must not read each other's provider affinity/upstream state.
func TestTenantSessionNamespaceIsolation(t *testing.T) {
	sessions := session.New(0, 0)
	adapter := &countAdapter{upstream: provider.UpstreamState{ConversationID: "c_tenant_a"}}
	d := newTenantDispatcher(sessions, adapter)

	ctxA := tenant.WithID(context.Background(), "tenant-a")
	ctxB := tenant.WithID(context.Background(), "tenant-b")

	if _, err := d.Chat(ctxA, provider.ChatRequest{Model: "test-model", SessionID: "shared"}); err != nil {
		t.Fatalf("tenant A chat failed: %v", err)
	}
	if adapter.lastRequest.SessionID == "shared" {
		t.Fatal("session ID reached the adapter without a tenant namespace")
	}

	adapter.upstream = provider.UpstreamState{ConversationID: "c_tenant_b"}
	if _, err := d.Chat(ctxB, provider.ChatRequest{Model: "test-model", SessionID: "shared"}); err != nil {
		t.Fatalf("tenant B chat failed: %v", err)
	}
	if !adapter.lastRequest.Upstream.Empty() {
		t.Fatalf("tenant B read tenant A upstream affinity: %+v", adapter.lastRequest.Upstream)
	}

	keyA := namespaceSession(ctxA, "shared")
	keyB := namespaceSession(ctxB, "shared")
	if keyA == keyB {
		t.Fatalf("tenants share session key %q", keyA)
	}
	bindA, okA := sessions.Get(keyA)
	bindB, okB := sessions.Get(keyB)
	if !okA || !okB {
		t.Fatalf("missing tenant bindings: A=%v B=%v", okA, okB)
	}
	if bindA.UpstreamConversationID != "c_tenant_a" {
		t.Fatalf("tenant A binding overwritten: %+v", bindA)
	}
	if bindB.UpstreamConversationID != "c_tenant_b" {
		t.Fatalf("tenant B binding wrong: %+v", bindB)
	}

	// Tenant A keeps its own conversation on the next turn.
	adapter.upstream = provider.UpstreamState{ConversationID: "c_tenant_a_2"}
	if _, err := d.Chat(ctxA, provider.ChatRequest{Model: "test-model", SessionID: "shared"}); err != nil {
		t.Fatalf("tenant A second chat failed: %v", err)
	}
	if adapter.lastRequest.Upstream.ConversationID != "c_tenant_a" {
		t.Fatalf("tenant A lost its upstream affinity: %+v", adapter.lastRequest.Upstream)
	}
}

// Client-supplied namespace-looking IDs must not let another tenant join an
// existing namespace, and the legacy SessionNSKey path must keep working.
func TestNamespaceSessionRejectsForeignPrefixAndKeepsLegacyKey(t *testing.T) {
	ctxA := tenant.WithID(context.Background(), "tenant-a")
	keyA := namespaceSession(ctxA, "shared")
	forged := "abcdef0123456789:shared"
	got := namespaceSession(ctxA, forged)
	if got == forged || !strings.HasPrefix(got, keyA[:16]+":") {
		t.Fatalf("foreign namespace prefix was trusted: %q", got)
	}
	if again := namespaceSession(ctxA, keyA); again != keyA {
		t.Fatalf("own namespace must stay idempotent: %q -> %q", keyA, again)
	}

	legacyCtx := context.WithValue(context.Background(), SessionNSKey, "legacy-key")
	legacy := namespaceSession(legacyCtx, "shared")
	if legacy == "shared" || namespaceSession(legacyCtx, "shared") != legacy {
		t.Fatalf("legacy SessionNSKey namespace unstable: %q", legacy)
	}
	otherLegacy := namespaceSession(context.WithValue(context.Background(), SessionNSKey, "legacy-key-2"), "shared")
	if otherLegacy == legacy {
		t.Fatal("different legacy keys share a namespace")
	}
	// A tenant ID wins over the legacy key.
	if namespaceSession(tenant.WithID(legacyCtx, "tenant-a"), "shared") != keyA {
		t.Fatal("tenant identity must take precedence over SessionNSKey")
	}
	// No identity keeps the legacy raw ID unchanged.
	if got := namespaceSession(context.Background(), "shared"); got != "shared" {
		t.Fatalf("unauthenticated context namespaced session: %q", got)
	}
}

// Image sessions must be tenant-scoped too: the same X-Session-ID on another
// key must not continue the first key's Gemini conversation or credential.
func TestTenantSessionNamespaceIsolationImage(t *testing.T) {
	sessions := session.New(0, 0)
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "img", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5})
	adapter := &imageChatAdapter{data: []byte("png"), returnUpstrm: provider.UpstreamState{ConversationID: "c_img_a"}}
	d := &Dispatcher{
		Models:      models,
		Sessions:    sessions,
		Credentials: &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}},
		Adapters:    mapRegistry{"test": adapter},
		Leases:      NewLeaseManager(),
		Breakers:    NewBreakerRegistry(),
	}
	ctxA := tenant.WithID(context.Background(), "tenant-a")
	ctxB := tenant.WithID(context.Background(), "tenant-b")

	if _, err := d.Image(ctxA, provider.ImageRequest{Model: "img", Prompt: "cat", SessionID: "shared"}); err != nil {
		t.Fatalf("tenant A image failed: %v", err)
	}
	adapter.returnUpstrm = provider.UpstreamState{ConversationID: "c_img_b"}
	if _, err := d.Image(ctxB, provider.ImageRequest{Model: "img", Prompt: "cat", SessionID: "shared"}); err != nil {
		t.Fatalf("tenant B image failed: %v", err)
	}
	if adapter.lastImage == nil || !adapter.lastImage.Upstream.Empty() {
		t.Fatalf("tenant B read tenant A image conversation: %+v", adapter.lastImage)
	}

	bindA, okA := sessions.Get(namespaceSession(ctxA, "shared"))
	bindB, okB := sessions.Get(namespaceSession(ctxB, "shared"))
	if !okA || !okB {
		t.Fatalf("missing image tenant bindings: A=%v B=%v", okA, okB)
	}
	if bindA.UpstreamConversationID != "c_img_a" || bindB.UpstreamConversationID != "c_img_b" {
		t.Fatalf("image bindings not isolated: A=%+v B=%+v", bindA, bindB)
	}
}

type recordingVideoAdapter struct {
	countAdapter
	lastVideo provider.VideoRequest
}

func (a *recordingVideoAdapter) Video(_ context.Context, req provider.VideoRequest, _ []byte) (provider.VideoResponse, error) {
	a.lastVideo = req
	return provider.VideoResponse{Data: []byte("mp4")}, nil
}

func TestTenantSessionNamespaceVideo(t *testing.T) {
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "vid", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityVideo}, TimeoutSec: 5})
	adapter := &recordingVideoAdapter{}
	d := &Dispatcher{
		Models:      models,
		Credentials: &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}},
		Adapters:    singleRegistry{adapter},
		Leases:      NewLeaseManager(),
		Breakers:    NewBreakerRegistry(),
	}
	ctxA := tenant.WithID(context.Background(), "tenant-a")
	ctxB := tenant.WithID(context.Background(), "tenant-b")
	if _, err := d.Video(ctxA, provider.VideoRequest{Model: "vid", Prompt: "clip", SessionID: "shared"}); err != nil {
		t.Fatalf("tenant A video failed: %v", err)
	}
	if adapter.lastVideo.SessionID != namespaceSession(ctxA, "shared") || adapter.lastVideo.SessionID == "shared" {
		t.Fatalf("video session not namespaced for tenant A: %q", adapter.lastVideo.SessionID)
	}
	if _, err := d.Video(ctxB, provider.VideoRequest{Model: "vid", Prompt: "clip", SessionID: "shared"}); err != nil {
		t.Fatalf("tenant B video failed: %v", err)
	}
	if adapter.lastVideo.SessionID != namespaceSession(ctxB, "shared") {
		t.Fatalf("video session not namespaced for tenant B: %q", adapter.lastVideo.SessionID)
	}
}
