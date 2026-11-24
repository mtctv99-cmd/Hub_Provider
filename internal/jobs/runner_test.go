package jobs

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"sever_provider/internal/jobstore"
)

func newRunnerTestStore(t *testing.T) (*sql.DB, *jobstore.Store) {
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

func TestJobConcurrencyFromEnv(t *testing.T) {
	t.Setenv("JOB_MAX_CONCURRENCY", "12")
	if got := jobConcurrencyFromEnv(); got != 12 {
		t.Fatalf("concurrency = %d, want 12", got)
	}
	t.Setenv("JOB_MAX_CONCURRENCY", "9999")
	if got := jobConcurrencyFromEnv(); got != MaxJobConcurrency {
		t.Fatalf("concurrency = %d, want clamp to %d", got, MaxJobConcurrency)
	}
	for _, invalid := range []string{"", "0", "-3", "not-a-number"} {
		t.Setenv("JOB_MAX_CONCURRENCY", invalid)
		if got := jobConcurrencyFromEnv(); got != DefaultJobConcurrency {
			t.Fatalf("JOB_MAX_CONCURRENCY=%q concurrency = %d, want %d", invalid, got, DefaultJobConcurrency)
		}
	}
}

func TestRunnerSlotCap(t *testing.T) {
	runner := &Runner{MaxConcurrency: 2}
	runner.initConcurrency()
	if !runner.tryAcquireSlot() || !runner.tryAcquireSlot() {
		t.Fatal("expected two available slots")
	}
	if runner.tryAcquireSlot() {
		t.Fatal("must not acquire a slot above MaxConcurrency")
	}
	runner.releaseSlot()
	if !runner.tryAcquireSlot() {
		t.Fatal("released slot was not reusable")
	}
}

// The claim loop must stop claiming while every slot is busy; otherwise a
// burst of video jobs all allocate before any can finish.
func TestRunnerDoesNotClaimBeyondConcurrency(t *testing.T) {
	db, store := newRunnerTestStore(t)
	for _, id := range []string{"job-one", "job-two"} {
		job := jobstore.Job{ID: id, Type: "llm"}
		if err := store.Create(context.Background(), &job); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	firstStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	runner := &Runner{
		Jobs:           store,
		Poll:           5 * time.Millisecond,
		MaxConcurrency: 1,
		processFn: func(ctx context.Context, _ jobstore.Job) error {
			if calls.Add(1) == 1 {
				firstStarted <- struct{}{}
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer waitCancel()
		_ = runner.Wait(waitCtx)
	}()
	runner.Start(ctx)

	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("runner never claimed the first job")
	}
	time.Sleep(150 * time.Millisecond)
	var processing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='PROCESSING'`).Scan(&processing); err != nil {
		t.Fatal(err)
	}
	if processing != 1 {
		t.Fatalf("processing jobs = %d while the only slot is busy, want 1", processing)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("processed %d jobs after releasing the slot, want 2", got)
	}
}

// Cancellation during shutdown must return an in-flight job to PENDING rather
// than marking it FAILED.
func TestRunnerRequeuesOnShutdown(t *testing.T) {
	_, store := newRunnerTestStore(t)
	job := jobstore.Job{ID: "shutdown-job", Type: "video"}
	if err := store.Create(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	runner := &Runner{
		Jobs:           store,
		Poll:           5 * time.Millisecond,
		MaxConcurrency: 1,
		processFn: func(ctx context.Context, _ jobstore.Job) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner.Start(ctx)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner never started the job")
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if err := runner.Wait(waitCtx); err != nil {
		t.Fatalf("runner wait: %v", err)
	}
	saved, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != jobstore.PENDING || saved.LockedAt != nil || saved.Attempts != 1 {
		t.Fatalf("shutdown job = %+v, want PENDING with attempts=1 and no lock", saved)
	}
	failed, err := store.CountByStatus(context.Background(), jobstore.FAILED)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("failed jobs = %d after shutdown, want 0", failed)
	}
}

func TestDownloadUpstreamVideoRejectsUnsafeURLs(t *testing.T) {
	cases := []string{
		"",
		"file:///etc/passwd",
		"http://example.com/video.mp4",
		"https://127.0.0.1/video.mp4",
		"https://10.0.0.1/video.mp4",
		"https://192.168.1.1/video.mp4",
		"https://172.16.0.1/video.mp4",
		"https://169.254.169.254/latest/meta-data",
		"https://[::1]/video.mp4",
		"https://localhost/video.mp4",
		"https://user:password@example.com/video.mp4",
		"https://example.com:8443/video.mp4",
	}
	for _, rawURL := range cases {
		t.Run(rawURL, func(t *testing.T) {
			if _, _, err := downloadUpstreamVideo(context.Background(), rawURL); err == nil {
				t.Fatalf("download of %q must be rejected", rawURL)
			}
		})
	}
}
