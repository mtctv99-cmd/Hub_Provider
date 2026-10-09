package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/workerhub"
)

func inboundColab(t *testing.T) (*Handler, *httptest.Server, *websocket.Conn) {
	t.Helper()
	models := scheduler.NewModelRegistry()
	if err := models.Register(scheduler.ModelSpec{ID: "gemma-4-colab", Provider: "google_colab", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}, TimeoutSec: 2}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&scheduler.Dispatcher{Models: models}, models)
	hub := workerhub.New(workerhub.Config{AuthToken: "test-worker-secret", HeartbeatInterval: time.Hour})
	h.SetWorkers(hub)
	srv := httptest.NewServer(NewRouter(h, ""))
	t.Cleanup(func() { srv.Close(); hub.Close() })
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/llm/ws/worker?token=test-worker-secret"
	conn, _, err := websocket.DefaultDialer.Dial(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err = conn.WriteJSON(workerhub.Registration{WorkerID: "w1", Model: "other-model", MaxConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err = conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	return h, srv, conn
}

func postColab(t *testing.T, srv *httptest.Server, stream bool) *http.Response {
	t.Helper()
	body := `{"model":"gemma-4-colab","messages":[{"role":"user","content":"hello"}],"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `}`
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestInboundColabModelMismatchAndAvailability(t *testing.T) {
	h, srv, conn := inboundColab(t)
	defer conn.Close()
	resp := postColab(t, srv, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("mismatch status %d", resp.StatusCode)
	}
	list := httptest.NewRecorder()
	h.ModelsList(list, httptest.NewRequest("GET", "/v1/models", nil))
	var payload struct {
		Data []struct {
			ID        string `json:"id"`
			Available bool   `json:"available"`
		} `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, m := range payload.Data {
		if m.ID == "gemma-4-colab" && m.Available {
			t.Fatal("mismatched worker advertised model as available")
		}
	}
}

func TestInboundColabJSONAndDisconnect(t *testing.T) {
	_, srv, _ := inboundColab(t)
	// Replace mismatched worker with matching registration.
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/llm/ws/worker?token=test-worker-secret"
	worker, _, err := websocket.DefaultDialer.Dial(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	worker.WriteJSON(workerhub.Registration{WorkerID: "w1", Model: "gemma-4-colab", MaxConcurrency: 1})
	var ack map[string]any
	if err := worker.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	result := make(chan *http.Response, 1)
	go func() { result <- postColab(t, srv, false) }()
	var request map[string]any
	if err := worker.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	if request["model"] != "gemma-4-colab" || request["type"] != "chat_request" {
		t.Fatalf("request=%v", request)
	}
	worker.WriteJSON(map[string]any{"type": "token", "request_id": request["request_id"], "delta": "hello "})
	worker.WriteJSON(map[string]any{"type": "token_end", "request_id": request["request_id"], "text": "world", "usage": map[string]int{"total_tokens": 5}})
	resp := <-result
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(data), "hello world") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	go func() { result <- postColab(t, srv, false) }()
	if err := worker.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	worker.Close()
	resp = <-result
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || strings.Contains(string(data), "[DONE]") {
		t.Fatalf("disconnect status=%d body=%s", resp.StatusCode, data)
	}
}

func TestInboundColabSSE(t *testing.T) {
	_, srv, _ := inboundColab(t)
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/llm/ws/worker?token=test-worker-secret"
	worker, _, err := websocket.DefaultDialer.Dial(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	worker.WriteJSON(workerhub.Registration{WorkerID: "w1", Model: "gemma-4-colab", MaxConcurrency: 1})
	var ack map[string]any
	if err := worker.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	result := make(chan *http.Response, 1)
	go func() { result <- postColab(t, srv, true) }()
	var request map[string]any
	if err := worker.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	worker.WriteJSON(map[string]any{"type": "token", "request_id": request["request_id"], "delta": "piece"})
	worker.WriteJSON(map[string]any{"type": "token_end", "request_id": request["request_id"]})
	resp := <-result
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"content":"piece"`) || !strings.Contains(string(data), "data: [DONE]") {
		t.Fatalf("stream status=%d body=%s", resp.StatusCode, data)
	}
}

func TestInboundColabStreamDisconnectDoesNotSignalDone(t *testing.T) {
	_, srv, _ := inboundColab(t)
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/llm/ws/worker?token=test-worker-secret"
	worker, _, err := websocket.DefaultDialer.Dial(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker.WriteJSON(workerhub.Registration{WorkerID: "w1", Model: "gemma-4-colab", MaxConcurrency: 1})
	var ack map[string]any
	if err := worker.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	result := make(chan *http.Response, 1)
	go func() { result <- postColab(t, srv, true) }()
	var request map[string]any
	if err := worker.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	worker.WriteJSON(map[string]any{"type": "token", "request_id": request["request_id"], "delta": "partial"})
	worker.Close()
	resp := <-result
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(data), "data: [DONE]") || !strings.Contains(string(data), "worker_unavailable") {
		t.Fatalf("unexpected terminal SSE: %s", data)
	}
}

func TestInboundColabReasoningContentSSE(t *testing.T) {
	_, srv, _ := inboundColab(t)
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/llm/ws/worker?token=test-worker-secret"
	worker, _, err := websocket.DefaultDialer.Dial(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	worker.WriteJSON(workerhub.Registration{WorkerID: "w1", Model: "gemma-4-colab", MaxConcurrency: 1})
	var ack map[string]any
	if err := worker.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	result := make(chan *http.Response, 1)
	go func() { result <- postColab(t, srv, true) }()
	var request map[string]any
	if err := worker.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	worker.WriteJSON(map[string]any{"type": "token", "request_id": request["request_id"], "delta": "result"})
	worker.WriteJSON(map[string]any{"type": "token_end", "request_id": request["request_id"]})
	resp := <-result
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"content":"result"`) {
		t.Fatalf("reasoning stream status=%d body=%s", resp.StatusCode, data)
	}
}
