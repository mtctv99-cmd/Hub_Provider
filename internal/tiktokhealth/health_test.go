package tiktokhealth

import (
	"context"
	"errors"
	"testing"
	"time"

	"sever_provider/internal/identity"
)

type fakeStore struct {
	creds   []identity.Credential
	secrets map[string]string
	domain  string
	marked  []string
	listErr error
}

func (f *fakeStore) List(_ context.Context, domain string) ([]identity.Credential, error) {
	f.domain = domain
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.creds, nil
}

func (f *fakeStore) ReadSecret(_ context.Context, id string) ([]byte, error) {
	secret, ok := f.secrets[id]
	if !ok {
		return nil, errors.New("secret missing")
	}
	return []byte(secret), nil
}

func (f *fakeStore) MarkSuccess(_ context.Context, id string) error {
	f.marked = append(f.marked, id)
	return nil
}

type fakeProber struct {
	fail   map[string]bool
	probed []string
	err    error
}

func (f *fakeProber) Probe(_ context.Context, sessionID string) error {
	f.probed = append(f.probed, sessionID)
	if f.err != nil {
		return f.err
	}
	if f.fail[sessionID] {
		return errors.New("upstream rejected the session")
	}
	return nil
}

func testConfig() Config {
	return Config{
		Timeout:     time.Second,
		MaxPerCycle: 10,
		Spacing:     time.Millisecond,
		Logf:        func(string, ...any) {},
	}
}

func session(id string) string { return `{"session_id":"` + id + `"}` }

func TestRevalidateResetsOnlySuccessfulDegradedCredentials(t *testing.T) {
	store := &fakeStore{
		creds: []identity.Credential{
			{ID: "degraded-ok", Status: identity.StatusDegraded},
			{ID: "degraded-bad", Status: identity.StatusDegraded},
			{ID: "healthy", Status: identity.StatusHealthy},
			{ID: "auth", Status: identity.StatusAuthRequired},
		},
		secrets: map[string]string{
			"degraded-ok":  session("s-ok"),
			"degraded-bad": session("s-bad"),
			"healthy":      session("s-healthy"),
			"auth":         session("s-auth"),
		},
	}
	prober := &fakeProber{fail: map[string]bool{"s-bad": true}}

	checked, reset := revalidate(context.Background(), store, prober, testConfig())

	if checked != 2 || reset != 1 {
		t.Fatalf("checked=%d reset=%d, want 2/1", checked, reset)
	}
	if store.domain != "tiktok_api" {
		t.Fatalf("listed domain %q, want tiktok_api", store.domain)
	}
	if len(store.marked) != 1 || store.marked[0] != "degraded-ok" {
		t.Fatalf("marked=%v, want [degraded-ok]", store.marked)
	}
	if len(prober.probed) != 2 {
		t.Fatalf("probed=%v, want only the two degraded sessions", prober.probed)
	}
}

func TestRevalidateLeavesStatusWhenProbeFails(t *testing.T) {
	store := &fakeStore{
		creds:   []identity.Credential{{ID: "degraded", Status: identity.StatusDegraded}},
		secrets: map[string]string{"degraded": session("s")},
	}
	prober := &fakeProber{err: errors.New("timeout")}

	checked, reset := revalidate(context.Background(), store, prober, testConfig())

	if checked != 1 || reset != 0 {
		t.Fatalf("checked=%d reset=%d, want 1/0", checked, reset)
	}
	if len(store.marked) != 0 {
		t.Fatalf("marked=%v, want none", store.marked)
	}
}

func TestRevalidateSkipsUnusableSecrets(t *testing.T) {
	store := &fakeStore{
		creds: []identity.Credential{
			{ID: "bad-json", Status: identity.StatusDegraded},
			{ID: "empty-session", Status: identity.StatusDegraded},
			{ID: "missing-secret", Status: identity.StatusDegraded},
		},
		secrets: map[string]string{
			"bad-json":      `{"session_id":`,
			"empty-session": `{"session_id":"   "}`,
		},
	}
	prober := &fakeProber{}

	checked, reset := revalidate(context.Background(), store, prober, testConfig())

	if checked != 0 || reset != 0 {
		t.Fatalf("checked=%d reset=%d, want 0/0", checked, reset)
	}
	if len(prober.probed) != 0 || len(store.marked) != 0 {
		t.Fatalf("probed=%v marked=%v, want none", prober.probed, store.marked)
	}
}

func TestRevalidateCapsProbesPerCycle(t *testing.T) {
	store := &fakeStore{secrets: map[string]string{}}
	for _, id := range []string{"a", "b", "c", "d"} {
		store.creds = append(store.creds, identity.Credential{ID: id, Status: identity.StatusDegraded})
		store.secrets[id] = session("s-" + id)
	}
	prober := &fakeProber{}
	cfg := testConfig()
	cfg.MaxPerCycle = 2

	checked, reset := revalidate(context.Background(), store, prober, cfg)

	if checked != 2 || reset != 2 {
		t.Fatalf("checked=%d reset=%d, want 2/2", checked, reset)
	}
	if len(prober.probed) != 2 {
		t.Fatalf("probed=%d, want 2", len(prober.probed))
	}
}

func TestRevalidateStopsWhenContextIsCanceled(t *testing.T) {
	store := &fakeStore{
		creds: []identity.Credential{
			{ID: "a", Status: identity.StatusDegraded},
			{ID: "b", Status: identity.StatusDegraded},
		},
		secrets: map[string]string{"a": session("s-a"), "b": session("s-b")},
	}
	prober := &fakeProber{}
	cfg := testConfig()
	cfg.Spacing = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	checked, reset := revalidate(ctx, store, prober, cfg)

	if checked != 0 || reset != 0 {
		t.Fatalf("checked=%d reset=%d, want 0/0", checked, reset)
	}
}

func TestRunStopsWhenContextIsCanceled(t *testing.T) {
	store := &fakeStore{}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	go func() {
		Run(ctx, store, &fakeProber{}, Config{
			Interval:     time.Hour,
			StartupDelay: time.Millisecond,
			Logf:         func(string, ...any) {},
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
