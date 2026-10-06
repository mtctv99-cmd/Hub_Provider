package workerhub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/provider"
)

func speechWAV() []byte { return []byte("RIFF\x04\x00\x00\x00WAVE") }

func TestSpeechProtocol(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	conn := dialWorker(t, "ws"+server.URL[len("http"):], Registration{WorkerID: "piper", Model: "piper:voice-a", MaxConcurrency: 1})
	defer conn.Close()
	result := make(chan struct {
		audio []byte
		mime  string
		err   error
	}, 1)
	go func() {
		audio, mime, err := hub.Speech(context.Background(), provider.SpeechRequest{Model: "piper:voice-a", Input: "hello", Voice: "voice-a"})
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
	for key, want := range map[string]string{"type": "tts_request", "model": "piper:voice-a", "text": "hello", "voice_id": "voice-a"} {
		if request[key] != want {
			t.Fatalf("%s=%v", key, request[key])
		}
	}
	id, ok := request["request_id"].(string)
	if !ok || id == "" {
		t.Fatalf("missing request_id: %v", request)
	}
	if err := conn.WriteJSON(map[string]any{"type": "audio_result", "request_id": id, "audio_base64": base64.StdEncoding.EncodeToString(speechWAV()), "mime": "audio/wav"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || string(got.audio) != string(speechWAV()) || got.mime != "audio/wav" {
			t.Fatalf("result=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("speech hung without done flag")
	}
	if !hub.Available("piper:voice-a") {
		t.Fatal("slot not released")
	}
}

func TestDispatchHonorsPerModelConcurrency(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	conn := dialWorker(t, "ws"+server.URL[len("http"):], Registration{
		WorkerID: "piper-cap", Model: "piper:voice-a", Models: []string{"piper:voice-a", "piper:voice-b"},
		MaxConcurrency: 10, ModelConcurrency: map[string]int{"piper:voice-a": 1, "piper:voice-b": 2},
	})
	defer conn.Close()

	_, err := hub.Dispatch(context.Background(), OutboundRequest{Model: "piper:voice-a", Type: "tts_request", Payload: map[string]string{"text": "first"}})
	if err != nil {
		t.Fatal(err)
	}
	_, firstMsg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var firstRequest map[string]any
	if err := json.Unmarshal(firstMsg, &firstRequest); err != nil {
		t.Fatal(err)
	}
	firstID := firstRequest["request_id"].(string)

	secondResult := make(chan *Stream, 1)
	secondErr := make(chan error, 1)
	go func() {
		stream, dispatchErr := hub.Dispatch(context.Background(), OutboundRequest{Model: "piper:voice-a", Type: "tts_request", Payload: map[string]string{"text": "second"}})
		if dispatchErr != nil {
			secondErr <- dispatchErr
			return
		}
		secondResult <- stream
	}()
	select {
	case <-secondResult:
		t.Fatal("second request passed the per-model concurrency limit")
	case err := <-secondErr:
		t.Fatal(err)
	case <-time.After(75 * time.Millisecond):
	}

	if err := conn.WriteJSON(map[string]any{"type": "audio_result", "request_id": firstID, "audio_base64": "YQ==", "mime": "audio/wav", "done": true}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondErr:
		t.Fatal(err)
	case <-secondResult:
	case <-time.After(time.Second):
		t.Fatal("second request was not dispatched after a per-model slot became free")
	}
}

func TestSpeechRejectsInvalidWorkerAudio(t *testing.T) {
	tests := []struct{ name, typ, mime, audio string }{
		{"wrong event", "job_result", "audio/wav", base64.StdEncoding.EncodeToString(speechWAV())},
		{"missing audio", "audio_result", "audio/wav", ""},
		{"invalid base64", "audio_result", "audio/wav", "%%%"},
		{"unsupported mime", "audio_result", "audio/mpeg", base64.StdEncoding.EncodeToString(speechWAV())},
		{"not wav", "audio_result", "audio/wav", base64.StdEncoding.EncodeToString([]byte("not a wav file"))},
		{"oversized", "audio_result", "audio/wav", strings.Repeat("A", maxSpeechAudioBytes*4/3+8)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
			defer hub.Close()
			server := httptest.NewServer(httpHandler(hub))
			defer server.Close()
			conn := dialWorker(t, "ws"+server.URL[len("http"):], Registration{WorkerID: "piper", Model: "piper:voice-a"})
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, _, err := hub.Speech(ctx, provider.SpeechRequest{Model: "piper:voice-a", Input: "hello"})
				result <- err
			}()
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				t.Fatal(err)
			}
			if err := conn.WriteJSON(map[string]any{"type": tt.typ, "request_id": request["request_id"], "mime": tt.mime, "audio_base64": tt.audio, "done": true}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("accepted invalid worker audio")
				}
			case <-ctx.Done():
				t.Fatal("speech did not reject invalid response")
			}
		})
	}
}

func TestSpeechCancellationAndNoWorker(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	_, _, err := hub.Speech(context.Background(), provider.SpeechRequest{Model: "piper:missing", Input: "hi"})
	if !errors.Is(err, ErrNoWorker) {
		t.Fatalf("err=%v", err)
	}
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	conn := dialWorker(t, "ws"+server.URL[len("http"):], Registration{WorkerID: "piper", Model: "piper:voice-a"})
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err = hub.Speech(ctx, provider.SpeechRequest{Model: "piper:voice-a", Input: "hi"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	deadline := time.Now().Add(time.Second)
	for !hub.Available("piper:voice-a") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hub.Available("piper:voice-a") {
		t.Fatal("slot not released after timeout")
	}
}
