package domain

import "time"

type JobStatus string

const (
	JobPending    JobStatus = "PENDING"
	JobProcessing JobStatus = "PROCESSING"
	JobCompleted  JobStatus = "COMPLETED"
	JobFailed     JobStatus = "FAILED"
)

type JobKind string

const (
	JobChat  JobKind = "chat"
	JobTTS   JobKind = "tts"
	JobImage JobKind = "image"
	JobVideo JobKind = "video"
)

type Job struct {
	ID          string     `json:"job_id"`
	Kind        JobKind    `json:"kind"`
	Status      JobStatus  `json:"status"`
	Model       string     `json:"model,omitempty"`
	Engine      string     `json:"engine,omitempty"`
	InputJSON   string     `json:"-"`
	ResultJSON  string     `json:"result,omitempty"`
	OutputPath  string     `json:"-"`
	OutputMIME  string     `json:"output_mime,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type MediaFile struct {
	ID        string     `json:"id"`
	JobID     string     `json:"job_id,omitempty"`
	Path      string     `json:"-"`
	MIME      string     `json:"mime"`
	SizeBytes int64      `json:"size_bytes"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type WorkerSnapshot struct {
	WorkerID       string    `json:"worker_id"`
	Model          string    `json:"model"`
	Endpoint       string    `json:"endpoint,omitempty"`
	MaxConcurrency int       `json:"max_concurrency"`
	ActiveRequests int       `json:"active_requests"`
	ConnectedAt    time.Time `json:"connected_at"`
	LastSeenAt     time.Time `json:"last_seen_at"`
	Status         string    `json:"status"`
}

type Session struct {
	ID           string    `json:"session_id"`
	CredentialID string    `json:"credential_id,omitempty"`
	Turns        int       `json:"turns"`
	LastActiveAt time.Time `json:"last_active_at"`
}
