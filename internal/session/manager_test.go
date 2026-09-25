package session

import (
	"testing"
	"time"
)

func TestSessionBindingLifecycle(t *testing.T) {
	now := time.Now()
	m := New(3, time.Minute)
	m.now = func() time.Time { return now }

	b, reused := m.Bind("sess-1", "cred-1", "gemini_web")
	if reused || b.Turns != 1 {
		t.Fatalf("first bind: reused=%v, turns=%d", reused, b.Turns)
	}

	b2, reused := m.Bind("sess-1", "cred-1", "gemini_web")
	if !reused || b2.Turns != 2 {
		t.Fatalf("second bind: reused=%v, turns=%d", reused, b2.Turns)
	}

	b3, reused := m.Bind("sess-1", "cred-1", "gemini_web")
	if !reused || b3.Turns != 3 {
		t.Fatalf("third bind: reused=%v, turns=%d", reused, b3.Turns)
	}

	// 4th turn exceeds maxTurns (3) -> auto reset to new conversation binding (turn 1)
	b4, reused := m.Bind("sess-1", "cred-2", "gemini_web")
	if reused || b4.Turns != 1 || b4.CredentialID != "cred-2" {
		t.Fatalf("reset bind: reused=%v, turns=%d, cred=%s", reused, b4.Turns, b4.CredentialID)
	}

	// Test idle TTL sweep
	now = now.Add(2 * time.Minute)
	if swept := m.Sweep(); swept != 1 {
		t.Fatalf("swept=%d, want 1", swept)
	}
	if _, ok := m.Get("sess-1"); ok {
		t.Fatal("expected sess-1 to be swept")
	}
}

func TestSessionSaveUpstream(t *testing.T) {
	m := New(25, time.Minute)
	if _, ok := m.Get("sess-up"); ok {
		t.Fatal("unexpected existing session")
	}
	// No-op on unknown session.
	m.SaveUpstream("sess-up", "c_1", "r_1", "rc_1")
	if _, ok := m.Get("sess-up"); ok {
		t.Fatal("SaveUpstream must not create a session")
	}
	b, _ := m.Bind("sess-up", "cred-1", "gemini_web")
	if b.Turns != 1 {
		t.Fatalf("turns=%d, want 1", b.Turns)
	}
	m.SaveUpstream("sess-up", "c_1", "r_1", "rc_1")
	got, ok := m.Get("sess-up")
	if !ok {
		t.Fatal("expected sess-up to exist")
	}
	if got.Turns != 1 {
		t.Fatalf("SaveUpstream must not bump turns, got %d", got.Turns)
	}
	if got.UpstreamConversationID != "c_1" || got.UpstreamResponseID != "r_1" || got.UpstreamChoiceID != "rc_1" {
		t.Fatalf("upstream not persisted: %+v", got)
	}
}
