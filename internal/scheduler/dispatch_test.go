package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"sever_provider/internal/identity"
	"sever_provider/internal/pool"
	"sever_provider/internal/provider"
	"sever_provider/internal/session"
)

type trackingSource struct {
	creds      []identity.Credential
	successes  []string
	statusSets []statusSet
}

type statusSet struct {
	ID             string
	Status         identity.CredentialStatus
	CooldownUntil  *time.Time
	IncrementError bool
}

func (s *trackingSource) List(_ context.Context, _ string) ([]identity.Credential, error) {
	return s.creds, nil
}
func (s *trackingSource) ReadSecret(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}
func (s *trackingSource) MarkSuccess(_ context.Context, id string) error {
	s.successes = append(s.successes, id)
	return nil
}
func (s *trackingSource) SetStatus(_ context.Context, id string, status identity.CredentialStatus, cooldown *time.Time, inc bool) error {
	s.statusSets = append(s.statusSets, statusSet{ID: id, Status: status, CooldownUntil: cooldown, IncrementError: inc})
	return nil
}

type countAdapter struct {
	callErr     error
	calls       int
	lastRequest provider.ChatRequest
	upstream    provider.UpstreamState
}

func (a *countAdapter) Name() string { return "test" }
func (a *countAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}
}
func (a *countAdapter) Chat(_ context.Context, req provider.ChatRequest, _ []byte) (provider.ChatResponse, error) {
	a.calls++
	a.lastRequest = req
	if a.callErr != nil {
		return provider.ChatResponse{}, a.callErr
	}
	return provider.ChatResponse{ID: "resp-1", Content: "hello", Upstream: a.upstream}, nil
}
func (a *countAdapter) StreamChat(_ context.Context, req provider.ChatRequest, _ []byte) (<-chan provider.StreamChunk, error) {
	a.calls++
	a.lastRequest = req
	ch := make(chan provider.StreamChunk, 2)
	ch <- provider.StreamChunk{Content: "hi"}
	ch <- provider.StreamChunk{Done: true, Upstream: a.upstream}
	close(ch)
	return ch, a.callErr
}
func (a *countAdapter) Speech(_ context.Context, _ provider.SpeechRequest, _ []byte) ([]byte, string, error) {
	return nil, "", provider.ErrCapabilityUnsupported
}
func (a *countAdapter) Image(_ context.Context, _ provider.ImageRequest, _ []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}
func (a *countAdapter) Video(_ context.Context, _ provider.VideoRequest, _ []byte) (provider.VideoResponse, error) {
	return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
}

type singleRegistry struct{ a provider.Adapter }

func (r singleRegistry) Get(string) (provider.Adapter, bool) { return r.a, true }

func TestDispatcherRecordsCredentialSuccess(t *testing.T) {
	source := &trackingSource{
		creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}},
	}
	adapter := &countAdapter{}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})

	d := &Dispatcher{
		Models:      models,
		Credentials: source,
		Adapters:    singleRegistry{adapter},
		Leases:      NewLeaseManager(),
		Breakers:    NewBreakerRegistry(),
	}

	_, err := d.Chat(context.Background(), provider.ChatRequest{Model: "test-model", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if len(source.successes) != 1 || source.successes[0] != "c1" {
		t.Errorf("expected MarkSuccess(c1), got %v", source.successes)
	}
	if d.Breakers.Get("test").State() != CircuitClosed {
		t.Errorf("expected provider breaker CLOSED, got %s", d.Breakers.Get("test").State())
	}
	if d.Breakers.GetAccount("c1").State() != CircuitClosed {
		t.Errorf("expected account breaker CLOSED, got %s", d.Breakers.GetAccount("c1").State())
	}
}

func TestDispatcherRecordsCredentialFailure(t *testing.T) {
	source := &trackingSource{
		creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}},
	}
	adapter := &countAdapter{callErr: errors.New("HTTP 429 rate limit exceeded")}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})

	d := &Dispatcher{
		Models:      models,
		Credentials: source,
		Adapters:    singleRegistry{adapter},
		Leases:      NewLeaseManager(),
		Breakers:    NewBreakerRegistry(),
	}

	_, err := d.Chat(context.Background(), provider.ChatRequest{Model: "test-model", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error from Chat, got nil")
	}
	if len(source.statusSets) != 1 {
		t.Fatalf("expected 1 SetStatus call, got %d", len(source.statusSets))
	}
	ss := source.statusSets[0]
	if ss.ID != "c1" {
		t.Errorf("expected credential c1, got %s", ss.ID)
	}
	if ss.Status != identity.StatusRateLimited {
		t.Errorf("expected RATE_LIMITED status, got %s", ss.Status)
	}
	if ss.CooldownUntil == nil {
		t.Error("expected cooldown to be set for rate-limited error")
	}
	if !ss.IncrementError {
		t.Error("expected IncrementError to be true")
	}
}

func TestDispatcherSessionAffinityUsesBoundCredential(t *testing.T) {
	source := &trackingSource{creds: []identity.Credential{
		{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 1, PoolRole: identity.PoolWorker},
		{ID: "c2", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 1, PoolRole: identity.PoolSession},
	}}
	adapter := &countAdapter{}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})
	sessions := session.New(session.DefaultMaxTurns, session.DefaultIdleTTL)
	_, _ = sessions.Bind("sess-1", "c1", "test")
	d := &Dispatcher{Models: models, Credentials: source, Adapters: singleRegistry{adapter}, Leases: NewLeaseManager(), Sessions: sessions, Pools: pool.New(), Breakers: NewBreakerRegistry()}
	if _, err := d.Chat(context.Background(), provider.ChatRequest{Model: "test-model", SessionID: "sess-1", Messages: []provider.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if len(source.successes) != 1 || source.successes[0] != "c1" {
		t.Fatalf("expected bound credential c1, got %v", source.successes)
	}
}

func TestDispatcherModelFailover(t *testing.T) {
	source := &trackingSource{creds: []identity.Credential{{ID: "fallback", Domain: "fallback", Status: identity.StatusHealthy, MaxConcurrency: 1}}}
	adapter := &countAdapter{}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "primary", Provider: "primary", Capabilities: []provider.Capability{provider.CapabilityChat}, FailoverTo: []string{"fallback"}, TimeoutSec: 10})
	_ = models.Register(ModelSpec{ID: "fallback", Provider: "fallback", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})
	registry := provider.NewRegistry()
	registry.Register(adapterWithName{name: "fallback", Adapter: adapter})
	d := &Dispatcher{Models: models, Credentials: source, Adapters: registry, Leases: NewLeaseManager(), Breakers: NewBreakerRegistry()}
	resp, err := d.Chat(context.Background(), provider.ChatRequest{Model: "primary", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("failover Chat failed: %v", err)
	}
	if resp.Content != "hello" {
		t.Fatalf("unexpected fallback response: %q", resp.Content)
	}
}

type adapterWithName struct {
	name    string
	Adapter provider.Adapter
}

func (a adapterWithName) Name() string                        { return a.name }
func (a adapterWithName) Capabilities() []provider.Capability { return a.Adapter.Capabilities() }
func (a adapterWithName) Chat(ctx context.Context, req provider.ChatRequest, secret []byte) (provider.ChatResponse, error) {
	return a.Adapter.Chat(ctx, req, secret)
}
func (a adapterWithName) StreamChat(ctx context.Context, req provider.ChatRequest, secret []byte) (<-chan provider.StreamChunk, error) {
	return a.Adapter.StreamChat(ctx, req, secret)
}
func (a adapterWithName) Speech(ctx context.Context, req provider.SpeechRequest, secret []byte) ([]byte, string, error) {
	return a.Adapter.Speech(ctx, req, secret)
}
func (a adapterWithName) Image(ctx context.Context, req provider.ImageRequest, secret []byte) (provider.ImageResponse, error) {
	return a.Adapter.Image(ctx, req, secret)
}
func (a adapterWithName) Video(ctx context.Context, req provider.VideoRequest, secret []byte) (provider.VideoResponse, error) {
	return a.Adapter.Video(ctx, req, secret)
}

func TestDispatcherPersistsUpstreamConversation(t *testing.T) {
	source := &trackingSource{creds: []identity.Credential{
		{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2},
	}}
	adapter := &countAdapter{upstream: provider.UpstreamState{ConversationID: "c_1", ResponseID: "r_1", ChoiceID: "rc_1"}}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})
	sessions := session.New(session.DefaultMaxTurns, session.DefaultIdleTTL)
	d := &Dispatcher{Models: models, Credentials: source, Adapters: singleRegistry{adapter}, Leases: NewLeaseManager(), Sessions: sessions, Breakers: NewBreakerRegistry()}
	resp, err := d.Chat(context.Background(), provider.ChatRequest{Model: "test-model", SessionID: "sess-up", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Upstream.ConversationID != "c_1" {
		t.Fatalf("expected upstream in response, got %+v", resp.Upstream)
	}
	binding, ok := sessions.Get("sess-up")
	if !ok {
		t.Fatal("expected session binding")
	}
	if binding.UpstreamConversationID != "c_1" || binding.UpstreamResponseID != "r_1" || binding.UpstreamChoiceID != "rc_1" {
		t.Fatalf("upstream not saved: %+v", binding)
	}
	// Second turn must forward stored upstream to the adapter.
	adapter.upstream = provider.UpstreamState{}
	if _, err := d.Chat(context.Background(), provider.ChatRequest{Model: "test-model", SessionID: "sess-up", Messages: []provider.Message{{Role: "user", Content: "hi 2"}}}); err != nil {
		t.Fatalf("second Chat failed: %v", err)
	}
	if adapter.lastRequest.Upstream.ConversationID != "c_1" {
		t.Fatalf("expected adapter to receive stored upstream, got %+v", adapter.lastRequest.Upstream)
	}
}

func TestDispatcherGuestModelWithoutCredential(t *testing.T) {
	source := &trackingSource{creds: nil}
	adapter := &countAdapter{}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "gemini-3.5-flash-lite", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, GuestAllowed: true, TimeoutSec: 10})
	d := &Dispatcher{Models: models, Credentials: source, Adapters: singleRegistry{adapter}, Leases: NewLeaseManager(), Sessions: session.New(session.DefaultMaxTurns, session.DefaultIdleTTL), Breakers: NewBreakerRegistry()}
	resp, err := d.Chat(context.Background(), provider.ChatRequest{Model: "gemini-3.5-flash-lite", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("guest Chat failed: %v", err)
	}
	if resp.Content != "hello" {
		t.Fatalf("unexpected guest response: %q", resp.Content)
	}
	if adapter.calls != 1 {
		t.Fatalf("calls=%d, want 1", adapter.calls)
	}
	if len(source.successes) != 0 || len(source.statusSets) != 0 {
		t.Fatalf("guest must not touch credential status: %+v %+v", source.successes, source.statusSets)
	}
}

func TestDispatcherAccountBreakerSkipsCredential(t *testing.T) {
	source := &trackingSource{
		creds: []identity.Credential{
			{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2},
			{ID: "c2", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2},
		},
	}
	adapter := &countAdapter{}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})

	breakers := NewBreakerRegistry()
	// Trip c1 account breaker
	now := time.Now()
	ab := breakers.GetAccount("c1")
	for i := 0; i < 5; i++ {
		ab.Failure(now)
	}

	d := &Dispatcher{
		Models:      models,
		Credentials: source,
		Adapters:    singleRegistry{adapter},
		Leases:      NewLeaseManager(),
		Breakers:    breakers,
	}

	resp, err := d.Chat(context.Background(), provider.ChatRequest{Model: "test-model", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("unexpected content: %s", resp.Content)
	}
	// c1 should have been skipped (account breaker open), c2 used
	if len(source.successes) != 1 || source.successes[0] != "c2" {
		t.Errorf("expected MarkSuccess(c2) only, got %v", source.successes)
	}
}
