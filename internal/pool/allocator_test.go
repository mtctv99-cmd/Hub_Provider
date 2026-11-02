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

func TestAllocatorFortyPercentSessionSplit(t *testing.T) {
	a := New()
	// 5 AUTO accounts: 40% = 2 for sessions, 60% = 3 for workers
	creds := []identity.Credential{
		{ID: "acc-1", PoolRole: identity.PoolAuto, MaxConcurrency: 1},
		{ID: "acc-2", PoolRole: identity.PoolAuto, MaxConcurrency: 1},
		{ID: "acc-3", PoolRole: identity.PoolAuto, MaxConcurrency: 1},
		{ID: "acc-4", PoolRole: identity.PoolAuto, MaxConcurrency: 1},
		{ID: "acc-5", PoolRole: identity.PoolAuto, MaxConcurrency: 1},
	}

	// 1. Session request: should get the 2 session accounts (40%)
	sessionGot := a.Allocate(creds, true)
	if len(sessionGot) != 2 {
		t.Fatalf("expected 2 session accounts, got %d: %+v", len(sessionGot), sessionGot)
	}
	for _, c := range sessionGot {
		if c.Effective != RoleSession {
			t.Errorf("expected RoleSession, got %s for %s", c.Effective, c.Credential.ID)
		}
	}

	// 2. Regular worker request: should get the 3 worker accounts (60%) when idle
	workerGot := a.Allocate(creds, false)
	if len(workerGot) != 3 {
		t.Fatalf("expected 3 worker accounts, got %d: %+v", len(workerGot), workerGot)
	}
	for _, c := range workerGot {
		if c.Effective != RoleWorker {
			t.Errorf("expected RoleWorker, got %s for %s", c.Effective, c.Credential.ID)
		}
	}
}

func TestAllocatorIdleSessionBorrowsForWorkerTraffic(t *testing.T) {
	a := New()
	// 5 AUTO accounts:
	// acc-1, acc-2 become Session accounts (40%)
	// acc-3, acc-4, acc-5 become Worker accounts (60%)
	creds := []identity.Credential{
		{ID: "acc-1", PoolRole: identity.PoolAuto, MaxConcurrency: 1, ActiveLeases: 0}, // Session account: IDLE!
		{ID: "acc-2", PoolRole: identity.PoolAuto, MaxConcurrency: 1, ActiveLeases: 1}, // Session account: BUSY with session!
		{ID: "acc-3", PoolRole: identity.PoolAuto, MaxConcurrency: 1, ActiveLeases: 1}, // Worker account: BUSY!
		{ID: "acc-4", PoolRole: identity.PoolAuto, MaxConcurrency: 1, ActiveLeases: 1}, // Worker account: BUSY!
		{ID: "acc-5", PoolRole: identity.PoolAuto, MaxConcurrency: 1, ActiveLeases: 1}, // Worker account: BUSY!
	}

	// Worker pool is exhausted (all 3 workers have ActiveLeases >= MaxConcurrency).
	// Idle session account (acc-1) should step in to help ("qua chạy thường phụ").
	// Busy session account (acc-2) should NOT be borrowed.
	got := a.Allocate(creds, false)

	// Expect 3 workers + 1 borrowed idle session account = 4
	if len(got) != 4 {
		t.Fatalf("expected 4 candidates (3 workers + 1 borrowed idle session), got %d: %+v", len(got), got)
	}

	foundIdleBorrow := false
	foundBusyBorrow := false
	for _, c := range got {
		if c.Credential.ID == "acc-1" {
			foundIdleBorrow = true
			if c.Effective != RoleWorker {
				t.Errorf("expected acc-1 effective role to be borrowed as RoleWorker, got %s", c.Effective)
			}
		}
		if c.Credential.ID == "acc-2" {
			foundBusyBorrow = true
		}
	}

	if !foundIdleBorrow {
		t.Error("idle session account acc-1 was not borrowed to help worker traffic")
	}
	if foundBusyBorrow {
		t.Error("busy session account acc-2 was erroneously borrowed")
	}
}

func TestAllocatePrefersHealthyOverDegraded(t *testing.T) {
	a := New()
	a.SetPriority("degraded", 100) // higher priority must not win over health
	creds := []identity.Credential{
		{ID: "degraded", PoolRole: identity.PoolAuto, Status: identity.StatusDegraded, MaxConcurrency: 1},
		{ID: "healthy-1", PoolRole: identity.PoolAuto, Status: identity.StatusHealthy, MaxConcurrency: 1},
		{ID: "healthy-2", PoolRole: identity.PoolAuto, Status: identity.StatusHealthy, MaxConcurrency: 1},
		{ID: "healthy-3", PoolRole: identity.PoolAuto, Status: identity.StatusHealthy, MaxConcurrency: 1},
	}
	for _, session := range []bool{true, false} {
		got := a.Allocate(creds, session)
		if len(got) == 0 || got[0].Credential.ID == "degraded" {
			t.Fatalf("session=%v: degraded allocated first: %+v", session, got)
		}
	}
}
