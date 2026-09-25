package workerhub

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWorkerAuthFailsClosedWhenTokenUnset(t *testing.T) {
	hub := New(Config{HeartbeatInterval: time.Hour})
	defer hub.Close()
	srv := httptest.NewServer(httpHandler(hub))
	defer srv.Close()
	ws := "ws" + strings.TrimPrefix(srv.URL, "http")
	for _, endpoint := range []string{ws, ws + "?token=arbitrary"} {
		conn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
		if err != nil {
			continue
		} // Pre-upgrade rejection is preferred.
		_ = conn.WriteJSON(Registration{WorkerID: "untrusted", Model: "gemma-4-colab", Token: "arbitrary"})
		conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err = conn.ReadMessage(); err == nil {
			t.Fatal("unauthenticated worker was accepted")
		}
		conn.Close()
	}
	if got := hub.Snapshot(); len(got) != 0 {
		t.Fatalf("unauthenticated sessions: %+v", got)
	}
}

func TestWorkerAuthorizationRequiresOneValidCredential(t *testing.T) {
	hub := New(Config{AuthToken: "correct", HeartbeatInterval: time.Hour})
	defer hub.Close()
	srv := httptest.NewServer(httpHandler(hub))
	defer srv.Close()
	ws := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(ws+"?token=wrong", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteJSON(Registration{WorkerID: "w", Model: "gemma", Token: "correct"})
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("valid registration token rejected: %v", err)
	}
}
