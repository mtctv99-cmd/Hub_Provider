package jobstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := New(db)
	ctx := context.Background()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return store, ctx
}

func TestCreateGetAndCount(t *testing.T) {
	store, ctx := openTestStore(t)
	job := Job{Type: "email", Payload: []byte(`{"to":"user@example.com"}`)}
	if err := store.Create(ctx, &job); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.Status != PENDING {
		t.Fatalf("Create did not populate pending job: %#v", job)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != job.ID || got.Type != job.Type || string(got.Payload) != string(job.Payload) || got.Status != PENDING {
		t.Fatalf("unexpected job: %#v", got)
	}
	count, err := store.CountByStatus(ctx, PENDING)
	if err != nil || count != 1 {
		t.Fatalf("pending count = %d, err = %v", count, err)
	}
}

func TestClaimCompleteAndFail(t *testing.T) {
	store, ctx := openTestStore(t)
	first := Job{ID: "first", Type: "work", Payload: []byte("one")}
	second := Job{ID: "second", Type: "work", Payload: []byte("two")}
	if err := store.Create(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &second); err != nil {
		t.Fatal(err)
	}

	claimed, err := store.ClaimNext(ctx, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != first.ID || claimed.Status != PROCESSING || claimed.LockedBy != "worker-a" || claimed.Attempts != 1 {
		t.Fatalf("unexpected claim: %#v", claimed)
	}
	if err := store.Complete(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Get(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != COMPLETED || completed.CompletedAt == nil || completed.LockedAt != nil {
		t.Fatalf("unexpected completed job: %#v", completed)
	}

	claimed, err = store.ClaimNext(ctx, "worker-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Fail(ctx, claimed.ID, errors.New("provider unavailable")); err != nil {
		t.Fatal(err)
	}
	failed, err := store.Get(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != FAILED || failed.Error != "provider unavailable" || failed.LockedAt != nil {
		t.Fatalf("unexpected failed job: %#v", failed)
	}
	if _, err := store.ClaimNext(ctx, "worker-c"); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("ClaimNext error = %v, want ErrNoJobs", err)
	}
}

func TestResetStaleAndCleanupOld(t *testing.T) {
	store, ctx := openTestStore(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store.now = func() time.Time { return now }
	job := Job{ID: "stale", Type: "work"}
	if err := store.Create(ctx, &job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNext(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now.Add(10 * time.Minute) }
	reset, err := store.ResetStale(ctx, 5*time.Minute)
	if err != nil || reset != 1 {
		t.Fatalf("reset = %d, err = %v", reset, err)
	}
	resetJob, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resetJob.Status != PENDING || resetJob.Attempts != 1 {
		t.Fatalf("unexpected reset job: %#v", resetJob)
	}

	if _, err := store.ClaimNext(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now.Add(20 * time.Minute) }
	if err := store.Complete(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now.Add(30 * time.Minute) }
	removed, err := store.CleanupOld(ctx, 5*time.Minute)
	if err != nil || removed != 1 {
		t.Fatalf("removed = %d, err = %v", removed, err)
	}
	if _, err := store.Get(ctx, job.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Get after cleanup error = %v", err)
	}
}

func TestValidation(t *testing.T) {
	store, ctx := openTestStore(t)
	if err := store.Create(ctx, nil); err == nil {
		t.Fatal("Create(nil) should fail")
	}
	if _, err := store.CountByStatus(ctx, Status("UNKNOWN")); err == nil {
		t.Fatal("invalid status should fail")
	}
	if _, err := store.ResetStale(ctx, -time.Second); err == nil {
		t.Fatal("negative stale age should fail")
	}
}
