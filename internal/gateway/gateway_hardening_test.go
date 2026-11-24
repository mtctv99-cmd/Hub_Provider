package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/db"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/scheduler"
)

func TestModelsListIsCached(t *testing.T) {
	models := scheduler.NewModelRegistry()
	if err := models.Register(scheduler.ModelSpec{ID: "first-model", Provider: "mock"}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, models)

	first := httptest.NewRecorder()
	h.ModelsList(first, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d", first.Code)
	}
	if first.Header().Get("X-Models-Cache") != "miss" {
		t.Fatalf("first cache header=%q", first.Header().Get("X-Models-Cache"))
	}

	if err := models.Register(scheduler.ModelSpec{ID: "second-model", Provider: "mock"}); err != nil {
		t.Fatal(err)
	}
	second := httptest.NewRecorder()
	h.ModelsList(second, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d", second.Code)
	}
	if second.Header().Get("X-Models-Cache") != "hit" {
		t.Fatalf("second cache header=%q", second.Header().Get("X-Models-Cache"))
	}
	if strings.Contains(second.Body.String(), "second-model") {
		t.Fatal("cached response must not include a model registered inside the TTL")
	}
	if !strings.Contains(second.Body.String(), "first-model") {
		t.Fatal("cached response lost the original model")
	}
}

func TestProbePerIPLimiter(t *testing.T) {
	h := NewHandler(nil, scheduler.NewModelRegistry())
	now := time.Now()
	if !h.allowProbeIP("203.0.113.1", now) {
		t.Fatal("first probe from an IP must be allowed")
	}
	if h.allowProbeIP("203.0.113.1", now.Add(5*time.Second)) {
		t.Fatal("second probe within the per-IP window must be rejected")
	}
	if !h.allowProbeIP("203.0.113.2", now.Add(5*time.Second)) {
		t.Fatal("a different IP must have its own budget")
	}
	if !h.allowProbeIP("203.0.113.1", now.Add(probePerIPInterval+time.Second)) {
		t.Fatal("probe must be allowed after the per-IP window")
	}
}

func TestJobResponseMasksErrorAndDropsPayload(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	jobs := jobstore.New(database.DB)
	if err := jobs.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	const secretPayload = `{"api_key":"sk-secret-value","prompt":"private"}`
	job := &jobstore.Job{Type: "llm", Kind: "chat", Model: "mock", Payload: []byte(secretPayload), InputJSON: secretPayload}
	if err := jobs.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.ClaimNext(ctx, "test-worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Fail(ctx, claimed.ID, `deepseek upstream body={"api_key":"sk-secret-value"}`); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(nil, scheduler.NewModelRegistry())
	h.SetJobs(jobs)
	rec := httptest.NewRecorder()
	h.LLMJob(rec, httptest.NewRequest(http.MethodGet, "/api/llm/jobs/"+job.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"sk-secret-value", "api_key", "private", `"Payload"`, `"InputJSON"`, `"LockedBy"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("job response leaked %q: %s", forbidden, body)
		}
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["ID"] != job.ID || view["Kind"] != "chat" {
		t.Fatalf("job view lost basic fields: %+v", view)
	}
	if view["Error"] != "upstream provider failed to complete request" {
		t.Fatalf("error was not masked: %v", view["Error"])
	}
}

func TestAccessLogNormalizationAndStatusRecorder(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions":                       "/v1/chat/completions",
		"/v1/videos/tasks/abc123":                    "/v1/videos/tasks/:id",
		"/api/tts/jobs/job1/audio":                   "/api/tts/jobs/:id/audio",
		"/api/llm/jobs/job1":                         "/api/llm/jobs/:id",
		"/v1/files/file1":                            "/v1/files/:id",
		"/admin/api/apikeys/revoke/abcdef0123456789": "/admin/api/apikeys/revoke/:id",
		"/admin/api/google/credentials/cred1":        "/admin/api/google/credentials/:id",
	}
	for input, want := range cases {
		if got := normalizeAccessPath(input); got != want {
			t.Fatalf("normalizeAccessPath(%q)=%q, want %q", input, got, want)
		}
	}

	inner := &statusWriterStub{header: http.Header{}}
	recorder := &accessResponseWriter{ResponseWriter: inner}
	recorder.WriteHeader(http.StatusTeapot)
	_, _ = recorder.Write([]byte("hello"))
	if recorder.status != http.StatusTeapot || recorder.bytes != 5 {
		t.Fatalf("status=%d bytes=%d", recorder.status, recorder.bytes)
	}
}

type statusWriterStub struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (s *statusWriterStub) Header() http.Header         { return s.header }
func (s *statusWriterStub) Write(p []byte) (int, error) { return s.body.Write(p) }
func (s *statusWriterStub) WriteHeader(status int)      { s.status = status }

// WebSocket routes require the middleware wrapper to keep the Hijacker contract.
var _ http.Hijacker = (*accessResponseWriter)(nil)

func TestWorkerChatTimeoutPolicy(t *testing.T) {
	if workerChatTimeout != 180*time.Second {
		t.Fatalf("workerChatTimeout=%v, want 180s", workerChatTimeout)
	}
	status, _, code := maskColabError(context.DeadlineExceeded)
	if status != http.StatusGatewayTimeout || code != "upstream_timeout" {
		t.Fatalf("deadline mapping status=%d code=%q, want 504 upstream_timeout", status, code)
	}
}
