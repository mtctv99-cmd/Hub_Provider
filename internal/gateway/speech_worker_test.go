package gateway

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/db"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/workerhub"
)

func TestTTSGenerateAcceptsConnectedRemoteVoice(t *testing.T) {
	hub := workerhub.New(workerhub.Config{AuthToken: "secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(workerhub.Registration{WorkerID: "remote", Model: "piper:remote", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := NewHandler(nil, scheduler.NewModelRegistry())
	h.SetWorkers(hub)
	h.SetJobs(jobstore.New(store.DB))
	rec := httptest.NewRecorder()
	h.TTSGenerate(rec, httptest.NewRequest(http.MethodPost, "/api/tts/generate", strings.NewReader(`{"text":"hello","voice_id":"remote"}`)))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"job_id"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioSpeechRoutesToConnectedWorker(t *testing.T) {
	hub := workerhub.New(workerhub.Config{AuthToken: "secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(workerhub.Registration{WorkerID: "remote", Model: "piper:voice-a", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	models := scheduler.NewModelRegistry()
	if err := models.Register(scheduler.ModelSpec{ID: "piper:voice-a", Provider: "piper_local", Capabilities: []provider.Capability{provider.CapabilitySpeech}, TimeoutSec: 30}); err != nil {
		t.Fatal(err)
	}
	dispatcher := &scheduler.Dispatcher{Models: models}
	h := NewHandler(dispatcher, dispatcher.Models)

	h.SetWorkers(hub)
	if dispatcher.Workers != hub {
		t.Fatal("handler did not wire worker hub to dispatcher")
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.AudioSpeech(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"piper:voice-a","input":"hello","voice":"voice-a"}`)))
		result <- rec
	}()
	var request map[string]any
	if err := conn.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	if request["type"] != "tts_request" {
		t.Fatalf("request=%v", request)
	}
	wav := []byte("RIFF\x04\x00\x00\x00WAVE")
	if err := conn.WriteJSON(map[string]any{"type": "audio_result", "request_id": request["request_id"], "mime": "audio/wav", "audio_base64": base64.StdEncoding.EncodeToString(wav), "done": true}); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-result:
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "audio/wav" || rec.Body.String() != string(wav) {
			t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("speech request hung")
	}
}

func TestDynamicWorkerModelDiscoveryAndChat(t *testing.T) {
	hub := workerhub.New(workerhub.Config{AuthToken: "secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Connect worker advertising an unconfigured custom LLM and custom voice
	if err := conn.WriteJSON(workerhub.Registration{
		WorkerID:       "dynamic-worker-1",
		Model:          "custom-llm",
		Models:         []string{"custom-llm", "piper:dynamic-voice"},
		Capabilities:   []string{"chat", "streaming", "audio_speech"},
		MaxConcurrency: 4,
		Token:          "secret",
	}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}

	models := scheduler.NewModelRegistry()
	dispatcher := &scheduler.Dispatcher{Models: models, Workers: hub}
	h := NewHandler(dispatcher, models)
	h.SetWorkers(hub)

	// 1. Verify dynamic model is listed in /v1/models
	recModels := httptest.NewRecorder()
	h.ModelsList(recModels, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recModels.Code != 200 {
		t.Fatalf("status=%d, want 200", recModels.Code)
	}
	bodyStr := recModels.Body.String()
	if !strings.Contains(bodyStr, "custom-llm") || !strings.Contains(bodyStr, "piper:dynamic-voice") {
		t.Fatalf("dynamic models missing from list: %s", bodyStr)
	}

	// 2. Verify chat request for custom-llm routes to dynamic worker
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"custom-llm","messages":[{"role":"user","content":"hello"}]}`)))
		result <- rec
	}()

	var request map[string]any
	if err := conn.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	if request["type"] != "chat_request" || request["model"] != "custom-llm" {
		t.Fatalf("wrong request: %v", request)
	}
	if err := conn.WriteJSON(map[string]any{
		"type":        "job_result",
		"request_id":  request["request_id"],
		"text":        "response from dynamic worker",
		"done":        true,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case rec := <-result:
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response from dynamic worker") {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("chat request timed out")
	}
}
