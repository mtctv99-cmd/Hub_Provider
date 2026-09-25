package scheduler

import (
	"context"
	"testing"
	"time"

	"sever_provider/internal/identity"
)

func TestLeaseReclaimExpired(t *testing.T) {
	m := NewLeaseManager()
	m.Register(identity.RuntimeCapacity{CredentialID: "c1", Domain: "gemini_web", Status: identity.StatusHealthy, MaxConcurrency: 1})
	lease, err := m.Acquire(context.Background(), "gemini_web", "r1", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if got := m.ReclaimExpired(); got != 1 {
		t.Fatalf("reclaimed=%d, want 1", got)
	}
	if got := m.ActiveForCredential("c1"); got != 0 {
		t.Fatalf("active=%d, want 0", got)
	}
	if err := m.Release(lease.ID); err == nil {
		t.Fatal("expected expired lease to be removed")
	}
}

func TestLeaseUnregisterRemovesCapacity(t *testing.T) {
	m := NewLeaseManager()
	m.Register(identity.RuntimeCapacity{CredentialID: "c1", Domain: "gemini_web", Status: identity.StatusHealthy, MaxConcurrency: 1})
	lease, err := m.Acquire(context.Background(), "gemini_web", "r1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	m.Unregister("c1")
	if got := m.ActiveForCredential("c1"); got != 0 {
		t.Fatalf("active=%d, want 0 after unregister", got)
	}
	if err := m.Release(lease.ID); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseCapacityAndRelease(t *testing.T) {
	m := NewLeaseManager()
	m.Register(identity.RuntimeCapacity{CredentialID: "c1", Domain: "gemini_web", Status: identity.StatusHealthy, MaxConcurrency: 2})
	ctx := context.Background()
	one, err := m.Acquire(ctx, "gemini_web", "r1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	two, err := m.Acquire(ctx, "gemini_web", "r2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.ActiveForCredential("c1"); got != 2 {
		t.Fatalf("active=%d", got)
	}
	if err := m.Release(one.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(two.ID); err != nil {
		t.Fatal(err)
	}
	if got := m.ActiveForCredential("c1"); got != 0 {
		t.Fatalf("active=%d", got)
	}
}
