package session

import (
	"context"
	"errors"
	"path/filepath"
	"sever_provider/internal/db"
	"sever_provider/internal/provider"
	"testing"
	"time"
)

func TestPersistentRestartIdleAndManyTurns(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	m, err := NewPersistent(database.DB)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err = m.Commit("s", "owner", "gemini_web", provider.UpstreamState{ConversationID: "c", ResponseID: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	m.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if m.Sweep() != 0 {
		t.Fatal("persistent mapping expired")
	}
	restarted, err := NewPersistent(database.DB)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := restarted.Get("s")
	if !ok || b.Turns != 30 || b.UpstreamConversationID != "c" {
		t.Fatalf("continuity lost: %+v", b)
	}
	if err = restarted.Commit("s", "other", "gemini_web", provider.UpstreamState{}); err == nil {
		t.Fatal("owner changed")
	}
	if err = restarted.Delete("s"); err != nil {
		t.Fatal(err)
	}
	again, err := NewPersistent(database.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Get("s"); ok {
		t.Fatal("deleted mapping resurrected")
	}
}
func TestSessionLockCancellationAndCleanup(t *testing.T) {
	m := New(0, 0)
	unlock, err := m.Acquire(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err = m.Acquire(ctx, "s"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	unlock()
	next, err := m.Acquire(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	next()
	if len(m.gates) != 0 {
		t.Fatal("lock leaked")
	}
}
func TestCommitFailureDoesNotPublishMapping(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewPersistent(database.DB)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()
	if m.Commit("s", "c", "p", provider.UpstreamState{}) == nil {
		t.Fatal("closed DB accepted")
	}
	if _, ok := m.Get("s"); ok {
		t.Fatal("unpersisted mapping published")
	}
}
