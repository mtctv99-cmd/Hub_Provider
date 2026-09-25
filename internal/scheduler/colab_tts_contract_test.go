package scheduler

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/provider"
	"sever_provider/internal/workerhub"
)

func TestDispatcherRoutesConfiguredVoicesToColabWorker(t *testing.T) {
	hub := workerhub.New(workerhub.Config{AuthToken: "fixture-token", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	models := NewModelRegistry()
	voices := []string{"ngochuyennew", "th_TH-mms_female-medium", "khmer_tts_v3"}
	modelIDs := make([]string, 0, len(voices))
	for _, voice := range voices {
		modelID := "piper:" + voice
		modelIDs = append(modelIDs, modelID)
		if err := models.Register(ModelSpec{ID: modelID, Provider: "piper_local", Capabilities: []provider.Capability{provider.CapabilitySpeech}, TimeoutSec: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.WriteJSON(workerhub.Registration{WorkerID: "fixture-colab", Model: modelIDs[0], Models: modelIDs, Capabilities: []string{"audio_speech"}, MaxConcurrency: 3, Token: "fixture-token"}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	dispatcher := &Dispatcher{Models: models, Workers: hub}
	for _, voice := range voices {
		result := make(chan struct {
			data []byte
			mime string
			err  error
		}, 1)
		go func(voice string) {
			data, mime, err := dispatcher.Speech(context.Background(), provider.SpeechRequest{Model: "piper:" + voice, Voice: voice, Input: "fixture"})
			result <- struct {
				data []byte
				mime string
				err  error
			}{data, mime, err}
		}(voice)
		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			t.Fatal(err)
		}
		if request["type"] != "tts_request" || request["model"] != "piper:"+voice || request["voice_id"] != voice {
			t.Fatalf("wrong route for %s: %v", voice, request)
		}
		wav := []byte("RIFF\x04\x00\x00\x00WAVE")
		if err := conn.WriteJSON(map[string]any{"type": "audio_result", "request_id": request["request_id"], "mime": "audio/wav", "audio_base64": base64.StdEncoding.EncodeToString(wav), "done": true}); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-result:
			if got.err != nil || got.mime != "audio/wav" || string(got.data) != string(wav) {
				t.Fatalf("voice %s route failed: mime=%q err=%v", voice, got.mime, got.err)
			}
		case <-time.After(time.Second):
			t.Fatalf("voice %s request timed out", voice)
		}
	}
}
