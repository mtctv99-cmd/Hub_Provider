// Package jobstore provides a small SQLite-backed persistent job queue.
package jobstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"
)

//go:embed schema.sql
var schemaFS embed.FS

// Status is the lifecycle state of a job.
type Status string

const (
	PENDING    Status = "PENDING"
	PROCESSING Status = "PROCESSING"
	COMPLETED  Status = "COMPLETED"
	FAILED     Status = "FAILED"

	// Status-prefixed aliases are provided for callers that prefer that naming style.
	StatusPending    = PENDING
	StatusProcessing = PROCESSING
	StatusCompleted  = COMPLETED
	StatusFailed     = FAILED
)

var (
	// ErrNoJobs is returned by ClaimNext when no pending job is available.
	ErrNoJobs = sql.ErrNoRows
	// ErrNotFound is returned when a state transition does not match a job.
	ErrNotFound = sql.ErrNoRows
)

// Job is the persisted representation of a unit of work. Payload is opaque to
// this package and is stored as a SQLite BLOB.
type Job struct {
	ID          string
	Type        string
	Kind        string
	Model       string
	Engine      string
	Payload     []byte
	InputJSON   string
	ResultJSON  string
	OutputPath  string
	OutputMIME  string
	Status      Status
	Attempts    int
	AvailableAt time.Time
	LockedAt    *time.Time
	LockedBy    string
	CompletedAt *time.Time
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store persists jobs in the generic jobs table using the supplied SQLite
// database handle. The handle may be a *db.DB's embedded *sql.DB.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// New creates a job store. The jobs table is created lazily by the first
// operation, or explicitly with EnsureSchema.
func New(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// EnsureSchema creates the generic jobs table and its indexes if needed.
func (s *Store) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("jobstore: nil database")
	}
	if _, err := s.db.ExecContext(ctx, mustSchema()); err != nil {
		return fmt.Errorf("jobstore: ensure schema: %w", err)
	}
	return nil
}

func mustSchema() string {
	data, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		panic(err)
	}
	return string(data)
}

func (s *Store) prepare(ctx context.Context) error {
	return s.EnsureSchema(ctx)
}

// Create inserts a pending job. An empty ID is replaced with a generated ID;
// generated values are written back to job.ID. Create rejects non-pending
// statuses so that jobs enter the queue through one well-defined state.
func (s *Store) Create(ctx context.Context, job *Job) error {
	if job == nil {
		return errors.New("jobstore: nil job")
	}
	if err := s.prepare(ctx); err != nil {
		return err
	}
	if job.Type == "" {
		return errors.New("jobstore: job type is required")
	}
	if job.Status != "" && job.Status != PENDING {
		return fmt.Errorf("jobstore: new job status must be %s", PENDING)
	}
	if job.ID == "" {
		job.ID = newID()
	}
	now := s.now().UTC()
	if job.AvailableAt.IsZero() {
		job.AvailableAt = now
	}
	job.Status = PENDING
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.Payload == nil {
		job.Payload = []byte{}
	}

	_, err := s.db.ExecContext(ctx, `
			INSERT INTO jobs
				(id, type, kind, model, engine, payload, input_json, result_json, output_path, output_mime, status, attempts, available_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Type, job.Kind, job.Model, job.Engine, job.Payload, job.InputJSON, job.ResultJSON,
		job.OutputPath, job.OutputMIME, job.Status, job.Attempts, job.AvailableAt,
		job.CreatedAt, job.UpdatedAt)
	if err != nil {
		return fmt.Errorf("jobstore: create job: %w", err)
	}
	return nil
}

// Get returns a job by ID. It returns sql.ErrNoRows when the job does not exist.
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	if err := s.prepare(ctx); err != nil {
		return Job{}, err
	}
	return s.scanJob(s.db.QueryRowContext(ctx, selectJobSQL+" WHERE id=?", id))
}

// ClaimNext atomically claims the oldest pending job that is available. The
// optional worker is recorded for diagnostics. Claiming increments Attempts.
func (s *Store) ClaimNext(ctx context.Context, workers ...string) (Job, error) {
	worker := ""
	if len(workers) > 0 {
		worker = workers[0]
	}
	if err := s.prepare(ctx); err != nil {
		return Job{}, err
	}
	now := s.now().UTC()
	row := s.db.QueryRowContext(ctx, `
		UPDATE jobs
		SET status=?, attempts=attempts+1, locked_at=?, locked_by=?, updated_at=?
		WHERE id = (
			SELECT id FROM jobs
			WHERE status=? AND available_at<=?
			ORDER BY available_at ASC, created_at ASC, id ASC
			LIMIT 1
		)
		RETURNING `+jobColumns,
		PROCESSING, now, worker, now, PENDING, now)
	job, err := s.scanJob(row)
	if err == sql.ErrNoRows {
		return Job{}, ErrNoJobs
	}
	if err != nil {
		return Job{}, fmt.Errorf("jobstore: claim job: %w", err)
	}
	return job, nil
}

// Complete marks a processing job complete and releases its lock.
func (s *Store) Complete(ctx context.Context, id string) error {
	if err := s.prepare(ctx); err != nil {
		return err
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status=?, completed_at=?, locked_at=NULL, locked_by=NULL,
			error=NULL, updated_at=?
		WHERE id=? AND status=?`, COMPLETED, now, now, id, PROCESSING)
	if err != nil {
		return fmt.Errorf("jobstore: complete job: %w", err)
	}
	return transitionResult(result, id, "complete")
}

// CompleteWithOutput marks a processing job complete and stores result/media metadata.
func (s *Store) CompleteWithOutput(ctx context.Context, id, resultJSON, outputPath, outputMIME string) error {
	if err := s.prepare(ctx); err != nil {
		return err
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status=?, completed_at=?, locked_at=NULL, locked_by=NULL,
			error=NULL, result_json=?, output_path=?, output_mime=?, updated_at=?
		WHERE id=? AND status=?`, COMPLETED, now, resultJSON, outputPath, outputMIME, now, id, PROCESSING)
	if err != nil {
		return fmt.Errorf("jobstore: complete job with output: %w", err)
	}
	return transitionResult(result, id, "complete")
}

// Fail marks a processing job failed and releases its lock. failure may be an
// error or a string; other values are formatted with fmt.Sprint.
func (s *Store) Fail(ctx context.Context, id string, failure any) error {
	if err := s.prepare(ctx); err != nil {
		return err
	}
	message := failureMessage(failure)
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status=?, error=?, locked_at=NULL, locked_by=NULL, updated_at=?
		WHERE id=? AND status=?`, FAILED, message, now, id, PROCESSING)
	if err != nil {
		return fmt.Errorf("jobstore: fail job: %w", err)
	}
	return transitionResult(result, id, "fail")
}

// ResetStale returns processing jobs whose lock is at least maxAge old to the
// pending queue. Attempts are retained. It returns the number of jobs reset.
func (s *Store) ResetStale(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge < 0 {
		return 0, errors.New("jobstore: stale age cannot be negative")
	}
	if err := s.prepare(ctx); err != nil {
		return 0, err
	}
	cutoff := s.now().UTC().Add(-maxAge)
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status=?, available_at=?, locked_at=NULL, locked_by=NULL, updated_at=?
		WHERE status=? AND locked_at IS NOT NULL AND locked_at<=?`,
		PENDING, now, now, PROCESSING, cutoff)
	if err != nil {
		return 0, fmt.Errorf("jobstore: reset stale jobs: %w", err)
	}
	count, err := result.RowsAffected()
	return int(count), err
}

// CleanupOld removes completed and failed jobs older than maxAge, based on
// completed_at for completed jobs and updated_at for failed jobs.
func (s *Store) CleanupOld(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge < 0 {
		return 0, errors.New("jobstore: cleanup age cannot be negative")
	}
	if err := s.prepare(ctx); err != nil {
		return 0, err
	}
	cutoff := s.now().UTC().Add(-maxAge)
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM jobs
		WHERE (status=? AND completed_at IS NOT NULL AND completed_at<=?)
		   OR (status=? AND updated_at<=?)`, COMPLETED, cutoff, FAILED, cutoff)
	if err != nil {
		return 0, fmt.Errorf("jobstore: cleanup old jobs: %w", err)
	}
	count, err := result.RowsAffected()
	return int(count), err
}

// CountByStatus returns the number of jobs in status.
func (s *Store) CountByStatus(ctx context.Context, status Status) (int, error) {
	if !validStatus(status) {
		return 0, fmt.Errorf("jobstore: invalid status %q", status)
	}
	if err := s.prepare(ctx); err != nil {
		return 0, err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status=?`, status).Scan(&count); err != nil {
		return 0, fmt.Errorf("jobstore: count %s jobs: %w", status, err)
	}
	return count, nil
}

const jobColumns = `id, type, kind, model, engine, payload, input_json, result_json, output_path, output_mime, status, attempts, available_at, locked_at, locked_by, completed_at, error, created_at, updated_at`
const selectJobSQL = `SELECT ` + jobColumns + ` FROM jobs`

func (s *Store) scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var job Job
	var lockedAt, completedAt sql.NullTime
	var lockedBy, failure sql.NullString
	if err := row.Scan(&job.ID, &job.Type, &job.Kind, &job.Model, &job.Engine, &job.Payload,
		&job.InputJSON, &job.ResultJSON, &job.OutputPath, &job.OutputMIME, &job.Status, &job.Attempts,
		&job.AvailableAt, &lockedAt, &lockedBy, &completedAt, &failure,
		&job.CreatedAt, &job.UpdatedAt); err != nil {
		return Job{}, err
	}
	job.LockedBy = lockedBy.String
	job.Error = failure.String
	if lockedAt.Valid {
		job.LockedAt = &lockedAt.Time
	}
	if completedAt.Valid {
		job.CompletedAt = &completedAt.Time
	}
	return job, nil
}

func transitionResult(result sql.Result, id, action string) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("jobstore: %s job %q: inspect result: %w", action, id, err)
	}
	if count == 0 {
		return fmt.Errorf("jobstore: %s job %q: %w", action, id, ErrNotFound)
	}
	return nil
}

func validStatus(status Status) bool {
	switch status {
	case PENDING, PROCESSING, COMPLETED, FAILED:
		return true
	default:
		return false
	}
}

func failureMessage(failure any) string {
	if failure == nil {
		return ""
	}
	if err, ok := failure.(error); ok {
		return err.Error()
	}
	return fmt.Sprint(failure)
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("job-%x", b[:])
}
