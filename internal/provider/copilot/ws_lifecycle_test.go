package copilot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var testUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func TestGenerateTerminalInvalidRequest(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Handshake protocol message
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{}`+recordSeparator))
		// Request dispatch message
		_, _, _ = conn.ReadMessage()
		// Server responds with Type 2 Terminal frame containing InvalidRequest
		errFrame := `{"type":2,"item":{"result":{"value":"InvalidRequest","message":"Sorry, I wasn't able to respond."},"messages":[]}}` + recordSeparator
		_ = conn.WriteMessage(websocket.TextMessage, []byte(errFrame))
	}))
	defer s.Close()

	wsURL := "ws://" + s.Listener.Addr().String() + "/chathub"
	adapter := New(nil)
	sess := Session{InvocationID: 0}
	account := Account{TokenURL: wsURL}

	_, err := adapter.generate(context.Background(), account, &sess, "hello", "copilot", nil, nil)
	if err == nil {
		t.Fatal("expected error on InvalidRequest, got nil")
	}
	if !strings.Contains(err.Error(), "rejected continuation") && !strings.Contains(err.Error(), "InvalidRequest") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if sess.InvocationID != 0 {
		t.Fatalf("sess.InvocationID must remain 0 on error, got %d", sess.InvocationID)
	}
}

func TestGenerateTerminalSuccessIncrementsCounter(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{}`+recordSeparator))
		_, _, _ = conn.ReadMessage()

		resp := `{"type":2,"item":{"result":{"value":"Success"},"conversationTransferToken":"tok-abc","turnState":"state-xyz","messages":[{"author":"bot","text":"Hello human!"}]}}` + recordSeparator
		_ = conn.WriteMessage(websocket.TextMessage, []byte(resp))
	}))
	defer s.Close()

	wsURL := "ws://" + s.Listener.Addr().String() + "/chathub"
	adapter := New(nil)
	sess := Session{InvocationID: 0}
	account := Account{TokenURL: wsURL}

	full, err := adapter.generate(context.Background(), account, &sess, "hello", "copilot", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if full != "Hello human!" {
		t.Fatalf("got full %q, expected 'Hello human!'", full)
	}
	if sess.InvocationID != 1 {
		t.Fatalf("sess.InvocationID must be 1 on success, got %d", sess.InvocationID)
	}
	if sess.ConversationTransferToken != "tok-abc" {
		t.Fatalf("sess.ConversationTransferToken=%q", sess.ConversationTransferToken)
	}
	if sess.TurnState != "state-xyz" {
		t.Fatalf("sess.TurnState=%q", sess.TurnState)
	}
}

func TestGenerateMidStreamDisconnectReturnsError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{}`+recordSeparator))
		_, _, _ = conn.ReadMessage()

		// Stream partial token in Type 1 frame
		streamFrame := `{"type":1,"arguments":[{"messages":[{"author":"bot","text":"Partial answer..."}]}]}` + recordSeparator
		_ = conn.WriteMessage(websocket.TextMessage, []byte(streamFrame))
		// Immediately close socket without Type 2 completion
		conn.Close()
	}))
	defer s.Close()

	wsURL := "ws://" + s.Listener.Addr().String() + "/chathub"
	adapter := New(nil)
	sess := Session{InvocationID: 0}
	account := Account{TokenURL: wsURL}

	_, err := adapter.generate(context.Background(), account, &sess, "hello", "copilot", nil, nil)
	if err == nil {
		t.Fatal("expected error on midstream socket drop, got nil (swallowed error!)")
	}
	if sess.InvocationID != 0 {
		t.Fatalf("sess.InvocationID must not increment on aborted stream, got %d", sess.InvocationID)
	}
}

func TestGenerateContextCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{}`+recordSeparator))
		_, _, _ = conn.ReadMessage()
		// Sleep without sending any terminal frame
		time.Sleep(2 * time.Second)
	}))
	defer s.Close()

	wsURL := "ws://" + s.Listener.Addr().String() + "/chathub"
	adapter := New(nil)
	sess := Session{InvocationID: 0}
	account := Account{TokenURL: wsURL}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := adapter.generate(ctx, account, &sess, "hello", "copilot", nil, nil)
	if err == nil {
		t.Fatal("expected error on context cancellation, got nil")
	}
	if sess.InvocationID != 0 {
		t.Fatalf("sess.InvocationID must remain 0 on cancellation, got %d", sess.InvocationID)
	}
}

func TestGenerateTerminalEmptyTextPreservesSessionAndReturnsEmptyResponse(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{}`+recordSeparator))
		_, _, _ = conn.ReadMessage()

		// Server declares Success in result, but provides NO bot messages or only empty loader
		resp := `{"type":2,"item":{"result":{"value":"Success"},"conversationTransferToken":"should-not-capture","turnState":"should-not-capture","messages":[]}}` + recordSeparator
		_ = conn.WriteMessage(websocket.TextMessage, []byte(resp))
	}))
	defer s.Close()

	wsURL := "ws://" + s.Listener.Addr().String() + "/chathub"
	adapter := New(nil)
	sess := Session{InvocationID: 5, ConversationTransferToken: "initial-token"}
	account := Account{TokenURL: wsURL}

	_, err := adapter.generate(context.Background(), account, &sess, "hello", "copilot", nil, nil)
	if err == nil {
		t.Fatal("expected empty text to return error")
	}
	if !strings.Contains(err.Error(), "empty_response") {
		t.Fatalf("expected error to contain empty_response, got %v", err)
	}
	// Verify session was NOT updated
	if sess.InvocationID != 5 {
		t.Fatalf("sess.InvocationID must remain 5, got %d", sess.InvocationID)
	}
	if sess.ConversationTransferToken != "initial-token" {
		t.Fatalf("sess.ConversationTransferToken must remain initial-token, got %q", sess.ConversationTransferToken)
	}
}
