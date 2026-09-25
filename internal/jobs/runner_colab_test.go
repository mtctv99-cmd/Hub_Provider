package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/workerhub"
)

func TestColabJobRoutesInboundAndFailsClosed(t *testing.T) {
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := jobstore.New(database)
	models := scheduler.NewModelRegistry()
	models.Register(scheduler.ModelSpec{ID: "gemma-4-colab", Provider: "google_colab", Capabilities: []provider.Capability{provider.CapabilityChat}})
	hub := workerhub.New(workerhub.Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	runner := &Runner{Jobs: store, Dispatcher: &scheduler.Dispatcher{Models: models}, Workers: hub}
	payload, _ := json.Marshal(LLMRequest{Prompt: "hello", Model: "gemma-4-colab"})
	job := jobstore.Job{Type: "llm", Model: "gemma-4-colab", Payload: payload}
	if err := store.Create(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	job, err = store.ClaimNext(context.Background(), "test-runner")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.processLLM(context.Background(), job); !errors.Is(err, workerhub.ErrNoWorker) {
		t.Fatalf("missing worker: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=test-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteJSON(workerhub.Registration{WorkerID: "colab", Model: "gemma-4-colab"})
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- runner.processLLM(context.Background(), job) }()
	var request map[string]any
	if err := conn.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	conn.WriteJSON(map[string]any{"type": "job_result", "request_id": request["request_id"], "text": "answer"})
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job hung")
	}
	saved, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != jobstore.COMPLETED || !strings.Contains(saved.ResultJSON, "answer") {
		t.Fatalf("job=%+v", saved)
	}
}
