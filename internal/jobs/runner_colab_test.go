package jobs

import (
	"context"
	"database/sql"
	"encoding/base64"
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
	"sever_provider/internal/filestore"
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

func TestRunnerRemotePiperSpeech(t *testing.T) {
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := jobstore.New(database)
	mediaStore, err := filestore.New(filepath.Join(t.TempDir(), "media"))
	if err != nil {
		t.Fatal(err)
	}
	hub := workerhub.New(workerhub.Config{AuthToken: "tts-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=tts-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(workerhub.Registration{WorkerID: "worker-tts", Model: "piper:vi_female", Token: "tts-secret"}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Jobs: store, Files: mediaStore, Workers: hub}
	payload, _ := json.Marshal(TTSRequest{Text: "xin chao", VoiceID: "vi_female"})
	job := jobstore.Job{Type: "tts", Model: "piper:vi_female", Payload: payload}
	if err := store.Create(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNext(context.Background(), "test-runner")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, _, err := runner.remotePiperSpeech(context.Background(), claimed.Model, TTSRequest{Text: "xin chao", VoiceID: "vi_female"})
		result <- err
	}()
	var req map[string]any
	if err := conn.ReadJSON(&req); err != nil {
		t.Fatal(err)
	}
	if req["type"] != "tts_request" || req["voice_id"] != "vi_female" {
		t.Fatalf("unexpected req: %+v", req)
	}
	wav := []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x44\xac\x00\x00\x88\x58\x01\x00\x02\x00\x10\x00data\x00\x00\x00\x00")
	if err := conn.WriteJSON(map[string]any{
		"type":         "audio_result",
		"request_id":   req["request_id"],
		"mime":         "audio/wav",
		"audio_base64": base64.StdEncoding.EncodeToString(wav),
		"done":         true,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("remote piper speech failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote piper speech hung")
	}
}
