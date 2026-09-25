package workerhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHubRoutesLeastLoadedAndStreams(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()

	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	wsURL := "ws" + server.URL[len("http"):]

	worker1 := dialWorker(t, wsURL, Registration{WorkerID: "worker-a", Model: "gemma", MaxConcurrency: 2})
	defer worker1.Close()
	worker2 := dialWorker(t, wsURL, Registration{WorkerID: "worker-b", Model: "gemma", MaxConcurrency: 2})
	defer worker2.Close()

	first := make(chan struct{})
	_, err := hub.Submit(context.Background(), "gemma", map[string]any{"text": "first"})
	if err != nil {
		t.Fatal(err)
	}
	go serveWorkerRequest(t, worker1, first, "first")
	waitForActive(t, hub, "worker-a", 1)

	stream, err := hub.Submit(context.Background(), "gemma", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	go serveWorkerRequest(t, worker2, nil, "second")
	if event := nextEvent(t, stream); event.Text != "second" {
		t.Fatalf("text=%q, want second", event.Text)
	}
	if err := nextStreamErr(t, stream); err != nil {
		t.Fatal(err)
	}

	close(first)
}

func TestHubClampsSlotsAndFailsRequestsOnDisconnect(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	wsURL := "ws" + server.URL[len("http"):]
	worker := dialWorker(t, wsURL, Registration{WorkerID: "worker-a", Model: "gemma", MaxConcurrency: 100})

	items := hub.Snapshot()
	if len(items) != 1 || items[0].MaxConcurrency != maxWorkerSlots {
		t.Fatalf("snapshot=%+v", items)
	}
	stream, err := hub.Submit(context.Background(), "gemma", map[string]string{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_ = worker.Close()
	select {
	case err := <-stream.Err:
		if !errors.Is(err, ErrWorkerDisconnected) {
			t.Fatalf("disconnect error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not fail after disconnect")
	}
}

func TestHubRoutesPiperAudioResult(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	wsURL := "ws" + server.URL[len("http"):]
	worker := dialWorker(t, wsURL, Registration{WorkerID: "piper-a", Model: "piper:voice-a", Models: []string{"piper:voice-a"}, Capabilities: []string{"tts"}, MaxConcurrency: 1})
	defer worker.Close()
	stream, err := hub.Submit(context.Background(), "piper:voice-a", map[string]any{"type": "tts_request", "voice_id": "voice-a", "text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_, message, readErr := worker.ReadMessage()
	if readErr != nil {
		t.Fatal(readErr)
	}
	var request map[string]any
	if err := json.Unmarshal(message, &request); err != nil {
		t.Fatal(err)
	}
	requestID, _ := request["request_id"].(string)
	if err := worker.WriteJSON(map[string]any{"type": "audio_result", "request_id": requestID, "mime": "audio/wav", "audio_base64": "YXVkaW8=", "done": true}); err != nil {
		t.Fatal(err)
	}
	event := nextEvent(t, stream)
	if event.AudioBase64 != "YXVkaW8=" || event.MIME != "audio/wav" || !event.Done {
		t.Fatalf("event=%+v", event)
	}
	if err := nextStreamErr(t, stream); err != nil {
		t.Fatal(err)
	}
}

func TestHubCancelsRequest(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	wsURL := "ws" + server.URL[len("http"):]
	worker := dialWorker(t, wsURL, Registration{WorkerID: "worker-a", Model: "gemma", MaxConcurrency: 1})
	defer worker.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := hub.Submit(ctx, "gemma", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-stream.Err:
		if !errors.Is(err, ErrRequestCanceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not cancel")
	}
}

func TestHubScaleMultipleWorkersSameID(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	wsURL := "ws" + server.URL[len("http"):]

	w1 := dialWorker(t, wsURL, Registration{WorkerID: "scale-worker", Model: "gemma", MaxConcurrency: 2})
	defer w1.Close()
	w2 := dialWorker(t, wsURL, Registration{WorkerID: "scale-worker", Model: "gemma", MaxConcurrency: 2})
	defer w2.Close()

	items := hub.Snapshot()
	if len(items) != 2 {
		t.Fatalf("expected 2 active workers for scaling, got %d: %+v", len(items), items)
	}
	if items[0].WorkerID == items[1].WorkerID {
		t.Fatalf("workers must have unique IDs, got identical: %s", items[0].WorkerID)
	}
}

func httpHandler(hub *Hub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = hub.ServeWebSocket(w, r) })
}

func dialWorker(t *testing.T, rawURL string, reg Registration) *websocket.Conn {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("token", "test-secret")
	u.RawQuery = query.Encode()
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(reg); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	_, _, err = conn.ReadMessage()
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return conn
}

func serveWorkerRequest(t *testing.T, conn *websocket.Conn, wait <-chan struct{}, text string) {
	t.Helper()
	for {
		var message map[string]any
		if err := conn.ReadJSON(&message); err != nil {
			return
		}
		if message["type"] == "ping" {
			_ = conn.WriteJSON(map[string]string{"type": "pong"})
			continue
		}
		if message["type"] != "chat_request" {
			continue
		}
		if wait != nil {
			<-wait
		}
		_ = conn.WriteJSON(map[string]any{"type": "job_result", "request_id": message["request_id"], "text": text})
		return
	}
}

func waitForActive(t *testing.T, hub *Hub, workerID string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, item := range hub.Snapshot() {
			if item.WorkerID == workerID && item.Active == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("worker %q did not reach active=%d: %+v", workerID, want, hub.Snapshot())
}

func nextEvent(t *testing.T, stream *Stream) Event {
	t.Helper()
	select {
	case event, ok := <-stream.Events:
		if !ok {
			t.Fatal("stream ended before event")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
	return Event{}
}

func nextStreamErr(t *testing.T, stream *Stream) error {
	t.Helper()
	select {
	case err := <-stream.Err:
		return err
	case <-time.After(time.Second):
		return nil
	}
}
