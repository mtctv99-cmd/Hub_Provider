package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"sever_provider/internal/admin"
	"sever_provider/internal/db"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

func TestVideoGenerationsAndTaskEndpoints(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	jobs := jobstore.New(database.DB)
	if err := jobs.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}

	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{
		ID:           "muse-video",
		Provider:     "meta_web",
		Capabilities: []provider.Capability{provider.CapabilityVideo},
		TimeoutSec:   300,
	})

	auth := admin.NewFromEnv()
	h := NewHandler(nil, models)
	h.SetJobs(jobs)
	h.SetAdminAuth(auth)

	// 1. Missing model or prompt -> 400
	badBody, _ := json.Marshal(map[string]any{"model": ""})
	badReq := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", bytes.NewReader(badBody))
	badRec := httptest.NewRecorder()
	h.VideoGenerations(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty model/prompt, got %d", badRec.Code)
	}

	// 2. Unconfigured model -> 422
	unconfBody, _ := json.Marshal(map[string]any{"model": "unconfigured-video", "prompt": "a red car"})
	unconfReq := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", bytes.NewReader(unconfBody))
	unconfRec := httptest.NewRecorder()
	h.VideoGenerations(unconfRec, unconfReq)
	if unconfRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for unconfigured model, got %d", unconfRec.Code)
	}

	// 3. Valid job create -> 202 Accepted
	okBody, _ := json.Marshal(map[string]any{"model": "muse-video", "prompt": "a red paper boat on water", "duration": 5})
	okReq := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", bytes.NewReader(okBody))
	okRec := httptest.NewRecorder()
	h.VideoGenerations(okRec, okReq)
	if okRec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", okRec.Code, okRec.Body.String())
	}

	var res struct {
		JobID     string `json:"job_id"`
		Status    string `json:"status"`
		StatusURL string `json:"status_url"`
	}
	if err := json.NewDecoder(okRec.Body).Decode(&res); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if res.JobID == "" || res.Status != "PENDING" {
		t.Fatalf("unexpected create response: %+v", res)
	}

	// 4. Query VideoTask -> 200 OK
	taskReq := httptest.NewRequest(http.MethodGet, "/v1/videos/tasks/"+res.JobID, nil)
	taskRec := httptest.NewRecorder()
	h.VideoTask(taskRec, taskReq)
	if taskRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", taskRec.Code)
	}
	var jobRes jobstore.Job
	if err := json.NewDecoder(taskRec.Body).Decode(&jobRes); err != nil {
		t.Fatalf("decode job failed: %v", err)
	}
	if jobRes.ID != res.JobID || jobRes.Kind != "video" {
		t.Fatalf("unexpected job result: %+v", jobRes)
	}
}
