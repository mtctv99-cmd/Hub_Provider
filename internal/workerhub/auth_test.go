package workerhub

import (
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWorkerAuthentication(t *testing.T) {
	hub := New(Config{AuthToken: "worker-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	wsURL := "ws" + server.URL[len("http"):]

	// A connection without a valid token must be rejected after registration.
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(Registration{WorkerID: "unauthorized", Model: "gemma", MaxConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		conn.Close()
		t.Fatal("expected unauthorized worker connection to close")
	}
	_ = conn.Close()

	// Query parameter authentication is accepted.
	u, _ := url.Parse(wsURL)
	q := u.Query()
	q.Set("token", "worker-secret")
	u.RawQuery = q.Encode()
	conn, _, err = websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(Registration{WorkerID: "authorized-query", Model: "gemma", MaxConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("query-authenticated worker was not acknowledged: %v", err)
	}

	// Registration payload authentication is accepted too.
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	if err := conn2.WriteJSON(Registration{WorkerID: "authorized-registration", Model: "gemma", MaxConcurrency: 1, Token: "worker-secret"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn2.ReadMessage(); err != nil {
		t.Fatalf("registration-authenticated worker was not acknowledged: %v", err)
	}
}
