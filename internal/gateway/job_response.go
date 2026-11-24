package gateway

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"sever_provider/internal/jobstore"
	"sever_provider/internal/tenant"
)

// jobResponse is the public view of a job. Provider payloads, input JSON,
// worker lock metadata, and raw error text are deliberately excluded: job IDs
// alone are not an authorization boundary, so responses must not expose
// upstream bodies or credentials.
type jobResponse struct {
	ID          string
	Type        string
	Kind        string
	Model       string
	Engine      string
	ResultJSON  string
	OutputPath  string
	OutputMIME  string
	Status      jobstore.Status
	Attempts    int
	Error       string
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// maskJobError converts a stored raw provider/runtime error into the same
// user-facing message used by synchronous endpoints.
func maskJobError(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	_, message, _ := maskProviderError(errors.New(raw))
	return message
}

func jobView(job jobstore.Job) jobResponse {
	return jobResponse{
		ID:          job.ID,
		Type:        job.Type,
		Kind:        job.Kind,
		Model:       job.Model,
		Engine:      job.Engine,
		ResultJSON:  job.ResultJSON,
		OutputPath:  job.OutputPath,
		OutputMIME:  job.OutputMIME,
		Status:      job.Status,
		Attempts:    job.Attempts,
		Error:       maskJobError(job.Error),
		CompletedAt: job.CompletedAt,
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   job.UpdatedAt,
	}
}

func writeJobResponse(w http.ResponseWriter, job jobstore.Job) {
	writeJSON(w, http.StatusOK, jobView(job))
}

// jobOwnedByRequest treats legacy ownerless rows as visible to any authenticated
// caller, but enforces ownership for rows created after owner tracking landed.
func jobOwnedByRequest(r *http.Request, job jobstore.Job) bool {
	owner := strings.TrimSpace(job.OwnerKeyID)
	if owner == "" {
		return true
	}
	return owner == tenant.ID(r.Context())
}
