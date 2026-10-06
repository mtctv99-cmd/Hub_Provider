package scheduler

import (
	"context"
	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/deepseek"
	"sever_provider/internal/session"
	"strings"
	"testing"
	"time"
)

func TestBoundChatNeverChangesAccount(t *testing.T) {
	for _, stream := range []bool{false, true} {
		sessions := session.New(0, 0)
		sessions.Commit("s", "owner", "test", provider.UpstreamState{ConversationID: "old"})
		source := &trackingSource{creds: []identity.Credential{{ID: "owner", Domain: "test", Status: identity.StatusAuthRequired, MaxConcurrency: 1}, {ID: "other", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 1}}}
		adapter := &countAdapter{}
		models := NewModelRegistry()
		models.Register(ModelSpec{ID: "m", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}})
		d := &Dispatcher{Models: models, Sessions: sessions, Credentials: source, Adapters: singleRegistry{adapter}, Leases: NewLeaseManager()}
		var err error
		if stream {
			_, err = d.StreamChat(context.Background(), provider.ChatRequest{Model: "m", SessionID: "s"})
		} else {
			_, err = d.Chat(context.Background(), provider.ChatRequest{Model: "m", SessionID: "s"})
		}
		if err == nil || adapter.calls != 0 {
			t.Fatal("old chat switched account")
		}
		b, _ := sessions.Get("s")
		if b.CredentialID != "owner" || b.UpstreamConversationID != "old" {
			t.Fatal("mapping changed")
		}
	}
}
func TestStreamCommitsBeforeDone(t *testing.T) {
	m := session.New(0, 0)
	in := make(chan provider.StreamChunk, 1)
	in <- provider.StreamChunk{Content: "hello", Done: true, Upstream: provider.UpstreamState{ConversationID: "c"}}
	close(in)
	out := wrapRelease(in, nil, "", context.Background(), func(err error, u provider.UpstreamState) error { return m.Commit("s", "owner", "test", u) })
	chunk := <-out
	if !chunk.Done {
		t.Fatal(chunk.Err)
	}
	if _, ok := m.Get("s"); !ok {
		t.Fatal("Done delivered before commit")
	}
}

func TestStreamEmptyResponseBlocksCommit(t *testing.T) {
	m := session.New(0, 0)
	in := make(chan provider.StreamChunk, 1)
	in <- provider.StreamChunk{Done: true, Upstream: provider.UpstreamState{ConversationID: "c"}}
	close(in)
	out := wrapRelease(in, nil, "", context.Background(), func(err error, u provider.UpstreamState) error {
		if err == nil {
			return m.Commit("s", "owner", "test", u)
		}
		return nil
	})
	chunk := <-out
	if chunk.Done {
		t.Fatal("expected empty stream to fail Done")
	}
	if chunk.Err == nil || !strings.Contains(chunk.Err.Error(), "empty_response") {
		t.Fatalf("expected empty_response error, got: %v", chunk.Err)
	}
	if _, ok := m.Get("s"); ok {
		t.Fatal("empty response committed session")
	}
}
func TestStreamAbruptCloseIsError(t *testing.T) {
	in := make(chan provider.StreamChunk)
	close(in)
	out := wrapRelease(in, nil, "", context.Background(), nil)
	chunk := <-out
	if chunk.Err == nil || !strings.Contains(chunk.Err.Error(), "without completion") {
		t.Fatal("false success")
	}
}
func TestMuteCooldownIsPreservedAndExpires(t *testing.T) {
	now := time.Now()
	until := now.Add(4 * time.Hour)
	status, cooldown := classifyProviderError(&deepseek.RateLimitError{Until: until}, now)
	if status != identity.StatusRateLimited || cooldown == nil || !cooldown.Equal(until) {
		t.Fatal("mute cooldown lost")
	}
	c := credentialAfterCooldown(identity.Credential{Status: status, CooldownUntil: cooldown}, until.Add(time.Second))
	if c.Status != identity.StatusHealthy {
		t.Fatalf("expected StatusHealthy after cooldown expires, got %v", c.Status)
	}
}

func TestRecoverCooldownExpiredRestoresHealthyAndPersists(t *testing.T) {
	now := time.Now()
	until := now.Add(-10 * time.Second) // expired
	source := &trackingSource{
		creds: []identity.Credential{{
			ID:            "c-muted",
			Domain:        "deepseek_web",
			Status:        identity.StatusRateLimited,
			CooldownUntil: &until,
		}},
	}
	d := &Dispatcher{
		Credentials: source,
		Breakers:    NewBreakerRegistry(),
	}
	// Record an account failure in breaker to ensure it resets on recovery
	d.Breakers.GetAccount("c-muted").Failure(now)

	recovered := d.recoverCooldownIfExpired(context.Background(), source.creds[0], now)
	if recovered.Status != identity.StatusHealthy {
		t.Fatalf("expected StatusHealthy, got %v", recovered.Status)
	}
	if recovered.CooldownUntil != nil {
		t.Fatalf("expected CooldownUntil to be nil, got %v", recovered.CooldownUntil)
	}
	// Verify that the status updater was called to persist Healthy state
	if len(source.statusSets) == 0 {
		t.Fatal("expected status update to be persisted to DB")
	}
	lastSet := source.statusSets[len(source.statusSets)-1]
	if lastSet.ID != "c-muted" || lastSet.Status != identity.StatusHealthy || lastSet.CooldownUntil != nil {
		t.Fatalf("unexpected persisted status: %+v", lastSet)
	}
	// Verify that the breaker account was reset
	if !d.Breakers.GetAccount("c-muted").Allow(now) {
		t.Fatal("expected account breaker to be allowed after recovery")
	}
}

func TestBoundChatQueuesAndWaitsForBusyAccount(t *testing.T) {
	sessions := session.New(0, 0)
	sessions.Commit("s1", "acc1", "deepseek_web", provider.UpstreamState{ConversationID: "c1"})

	source := &trackingSource{
		creds: []identity.Credential{
			{ID: "acc1", Domain: "deepseek_web", Status: identity.StatusHealthy, MaxConcurrency: 1},
		},
	}
	adapter := &countAdapter{}
	models := NewModelRegistry()
	models.Register(ModelSpec{ID: "deepseek-chat", Provider: "deepseek_web", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 5})
	lm := NewLeaseManager()
	lm.Register(identity.RuntimeCapacity{CredentialID: "acc1", Domain: "deepseek_web", Status: identity.StatusHealthy, MaxConcurrency: 1})
	d := &Dispatcher{Models: models, Sessions: sessions, Credentials: source, Adapters: singleRegistry{adapter}, Leases: lm}

	// 1. Manually acquire a lease on acc1 to simulate an active request running
	lease, err := lm.AcquireCredential(context.Background(), "acc1", "req-first", 2*time.Second)
	if err != nil {
		t.Fatalf("failed to acquire first lease: %v", err)
	}

	// 2. Start a bound chat request in a background goroutine
	doneCh := make(chan error, 1)
	go func() {
		_, chatErr := d.Chat(context.Background(), provider.ChatRequest{Model: "deepseek-chat", SessionID: "s1"})
		doneCh <- chatErr
	}()

	// Verify that the second request does NOT finish immediately (it must wait!)
	select {
	case err := <-doneCh:
		t.Fatalf("expected bound request to wait for busy account, but finished early with: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Good: it is waiting in queue!
	}

	// 3. Release the first lease; the queued bound request should now acquire the slot and succeed!
	if err := lm.Release(lease.ID); err != nil {
		t.Fatalf("failed to release first lease: %v", err)
	}

	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("bound request failed after slot was freed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bound request to complete")
	}

	if adapter.calls != 1 {
		t.Fatalf("expected 1 call to adapter, got %d", adapter.calls)
	}
}

func TestDeepSeekMaxConcurrencyHardcodedToOne(t *testing.T) {
	sessions := session.New(0, 0)
	source := &trackingSource{
		creds: []identity.Credential{
			{ID: "acc-ds", Domain: "deepseek_web", Status: identity.StatusHealthy, MaxConcurrency: 5}, // configured as 5
		},
	}
	adapter := &countAdapter{}
	models := NewModelRegistry()
	models.Register(ModelSpec{ID: "deepseek-chat", Provider: "deepseek_web", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 5})
	lm := NewLeaseManager()
	d := &Dispatcher{Models: models, Sessions: sessions, Credentials: source, Adapters: singleRegistry{adapter}, Leases: lm}

	// Run once to register capacity
	_, err := d.Chat(context.Background(), provider.ChatRequest{Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}

	// Check runtime capacity registered in LeaseManager
	lm.mu.Lock()
	cap, ok := lm.capacities["acc-ds"]
	lm.mu.Unlock()
	if !ok {
		t.Fatal("capacity not registered")
	}
	if cap.MaxConcurrency != 1 {
		t.Fatalf("expected MaxConcurrency to be forced to 1 for deepseek_web, got %d", cap.MaxConcurrency)
	}
}
