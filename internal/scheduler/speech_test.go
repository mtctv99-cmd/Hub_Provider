package scheduler

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/settings"
	"sever_provider/internal/workerhub"
)

func TestDispatcherSpeechUsesRemoteWorkerWithoutLocalModel(t *testing.T) {
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
	models := NewModelRegistry()
	if err := models.Register(ModelSpec{ID: "piper:remote", Provider: "piper_local", Capabilities: []provider.Capability{provider.CapabilitySpeech}}); err != nil {
		t.Fatal(err)
	}
	dispatcher := &Dispatcher{Models: models, Workers: hub}
	result := make(chan struct {
		audio []byte
		mime  string
		err   error
	}, 1)
	go func() {
		audio, mime, err := dispatcher.Speech(context.Background(), provider.SpeechRequest{Model: "piper:remote", Input: "hello", Voice: "piper:remote"})
		result <- struct {
			audio []byte
			mime  string
			err   error
		}{audio, mime, err}
	}()
	var request map[string]any
	if err := conn.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	if request["type"] != "tts_request" || request["voice_id"] != "remote" {
		t.Fatalf("request=%v", request)
	}
	wav := []byte("RIFF\x04\x00\x00\x00WAVE")
	if err := conn.WriteJSON(map[string]any{"type": "audio_result", "request_id": request["request_id"], "mime": "audio/wav", "audio_base64": base64.StdEncoding.EncodeToString(wav), "done": true}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || string(got.audio) != string(wav) || got.mime != "audio/wav" {
			t.Fatalf("result=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher hung")
	}
}

func TestDispatcherSpeechDoesNotUseAbsentLocalPiper(t *testing.T) {
	models := NewModelRegistry()
	if err := models.Register(ModelSpec{ID: "piper:missing", Provider: "piper_local", Capabilities: []provider.Capability{provider.CapabilitySpeech}}); err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry()
	reg.Register(piper.New("piper", t.TempDir()))
	d := &Dispatcher{Models: models, Adapters: reg}
	_, _, err := d.Speech(context.Background(), provider.SpeechRequest{Model: "piper:missing", Input: "hi", Voice: "missing"})
	if err == nil || !strings.Contains(err.Error(), "not available locally") {
		t.Fatalf("err=%v", err)
	}
}

func TestDispatcherSpeechHonorsPiperMode(t *testing.T) {
	models := NewModelRegistry()
	if err := models.Register(ModelSpec{ID: "piper:remote", Provider: "piper_local", Capabilities: []provider.Capability{provider.CapabilitySpeech}}); err != nil {
		t.Fatal(err)
	}
	hub := workerhub.New(workerhub.Config{AuthToken: "secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(workerhub.Registration{WorkerID: "remote-mode", Model: "piper:remote", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE runtime_settings (key TEXT PRIMARY KEY, value TEXT, updated_at TEXT DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	store := settings.New(db)
	if err := store.Set(context.Background(), "tts.piper_mode", "local"); err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry()
	voiceDir := t.TempDir()
	modelPath := filepath.Join(voiceDir, "remote.onnx")
	metaPath := modelPath + ".json"
	if err := os.WriteFile(modelPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte(`{"phoneme_type":"espeak","phoneme_id_map":{"a":[1]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	local := piper.New("missing-piper-binary", voiceDir)
	reg.Register(local)
	d := &Dispatcher{Models: models, Credentials: &trackingSource{}, Adapters: reg, Workers: hub, Settings: store}
	_, _, err = d.Speech(context.Background(), provider.SpeechRequest{Model: "piper:remote", Voice: "remote", Input: "hi"})
	if err == nil {
		t.Fatal("local mode should fail when local Piper runtime is unavailable")
	}
	if err := store.Set(context.Background(), "tts.piper_mode", "remote"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, _, err := d.Speech(context.Background(), provider.SpeechRequest{Model: "piper:remote", Voice: "remote", Input: "hi"})
		result <- err
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
	if err := <-result; err != nil {
		t.Fatalf("remote mode request failed: %v", err)
	}
}

func TestDispatcherSpeechRejectsDisabledRemoteVoice(t *testing.T) {
	models := NewModelRegistry()
	if err := models.Register(ModelSpec{ID: "piper:remote", Provider: "piper_local", Capabilities: []provider.Capability{provider.CapabilitySpeech}}); err != nil {
		t.Fatal(err)
	}
	store, err := settingsForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetBool(context.Background(), "tts.voice.remote.enabled", false); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{Models: models, Settings: store}
	_, _, err = d.Speech(context.Background(), provider.SpeechRequest{Model: "piper:remote", Voice: "remote", Input: "hi"})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err=%v", err)
	}
}

func settingsForTest(t *testing.T) (*settings.Store, error) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE runtime_settings (key TEXT PRIMARY KEY, value TEXT, updated_at TEXT DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return nil, err
	}
	return settings.New(db), nil
}
