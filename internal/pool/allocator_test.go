package pool

import (
	"testing"

	"sever_provider/internal/identity"
)

func TestAllocatorPrefersPoolAndBorrows(t *testing.T) {
	a := New()
	creds := []identity.Credential{
		{ID: "session", PoolRole: identity.PoolSession, MaxConcurrency: 1},
		{ID: "worker-1", PoolRole: identity.PoolWorker, MaxConcurrency: 1},
		{ID: "worker-2", PoolRole: identity.PoolWorker, MaxConcurrency: 1},
	}
	got := a.Allocate(creds, true)
	if len(got) != 1 || got[0].Credential.ID != "session" {
		t.Fatalf("session allocation=%+v", got)
	}
	got = a.Allocate(creds, false)
	if len(got) != 2 {
		t.Fatalf("worker allocation=%+v", got)
	}
	got = a.Allocate([]identity.Credential{{ID: "session", PoolRole: identity.PoolSession}}, false)
	if len(got) != 1 || got[0].Effective != RoleWorker {
		t.Fatalf("borrow allocation=%+v", got)
	}
}
