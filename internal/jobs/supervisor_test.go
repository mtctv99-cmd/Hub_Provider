package jobs

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"sever_provider/internal/jobstore"
)

func newSupervisorTestStore(t *testing.T) (*sql.DB, *jobstore.Store) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := jobstore.New(db)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db, store
}

func TestSupervisorStaleWindows(t *testing.T) {
	sup := &Supervisor{}
	nonVideo, video := sup.staleWindows()
	if nonVideo != 10*time.Minute {
		t.Fatalf("non-video stale window = %v, want 10m", nonVideo)
	}
	if video != 60*time.Minute {
		t.Fatalf("video stale window = %v, want 60m", video)
	}

	sup.VideoTimeout = 90 * time.Minute
	if _, video := sup.staleWindows(); video != 105*time.Minute {
		t.Fatalf("video stale window = %v, want VideoTimeout+15m", video)
	}
}

// An 11-minute PROCESSING video job is healthy (VideoTimeout is 45m) and must
// not be reset, while a chat job at the same age is abandoned and reset.
func TestSupervisorKeepsLongRunningVideoJob(t *testing.T) {
	db, store := newSupervisorTestStore(t)
	ctx := context.Background()
	video := jobstore.Job{ID: "video", Type: "video"}
	chat := jobstore.Job{ID: "chat", Type: "llm"}
	for _, job := range []*jobstore.Job{&video, &chat} {
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimNext(ctx, "worker"); err != nil {
			t.Fatal(err)
		}
	}
	lockedAt := time.Now().UTC().Add(-11 * time.Minute)
	for _, id := range []string{video.ID, chat.ID} {
		if _, err := db.Exec(`UPDATE jobs SET locked_at=? WHERE id=?`, lockedAt, id); err != nil {
			t.Fatal(err)
		}
	}

	sup := &Supervisor{Jobs: store}
	sup.RunOnce(ctx)

	videoJob, err := store.Get(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if videoJob.Status != jobstore.PROCESSING {
		t.Fatalf("video job at 11m = %s, want PROCESSING", videoJob.Status)
	}
	chatJob, err := store.Get(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if chatJob.Status != jobstore.PENDING {
		t.Fatalf("chat job at 11m = %s, want PENDING", chatJob.Status)
	}

	// Past VideoTimeout+buffer the video job is genuinely abandoned.
	if _, err := db.Exec(`UPDATE jobs SET locked_at=? WHERE id=?`, time.Now().UTC().Add(-61*time.Minute), video.ID); err != nil {
		t.Fatal(err)
	}
	sup.RunOnce(ctx)
	videoJob, err = store.Get(ctx, video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if videoJob.Status != jobstore.PENDING {
		t.Fatalf("video job at 61m = %s, want PENDING", videoJob.Status)
	}
}
