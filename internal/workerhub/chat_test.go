package workerhub

import (
	"context"
	"errors"
	"net/http/httptest"
	"sever_provider/internal/provider"
	"testing"
	"time"
)

func TestChatRejectsMismatchedModel(t *testing.T) {
	hub := New(Config{AuthToken: "secret"})
	defer hub.Close()
	if hub.Available("gemma-4-colab") {
		t.Fatal("empty hub available")
	}
	_, err := hub.Chat(context.Background(), chatRequest("gemma-4-colab"))
	if !errors.Is(err, ErrNoWorker) {
		t.Fatalf("err=%v", err)
	}
}

func TestRegistrationRequiresAuth(t *testing.T) {
	hub := New(Config{AuthToken: "secret"})
	defer hub.Close()
	_, err := hub.Register(nil, Registration{WorkerID: "bypass", Model: "gemma-4-colab"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("register err=%v", err)
	}
}

func chatRequest(model string) provider.ChatRequest {
	return provider.ChatRequest{Model: model, Messages: []provider.Message{{Role: "user", Content: "hi"}}}
}

func TestHubChatCancellationReleasesSlot(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	conn := dialWorker(t, "ws"+server.URL[len("http"):], Registration{WorkerID: "c", Model: "gemma", MaxConcurrency: 1})
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := hub.Chat(ctx, chatRequest("gemma")); result <- err }()
	var request map[string]any
	if err := conn.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, ErrRequestCanceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation hung")
	}
	if !hub.Available("gemma") {
		t.Fatal("slot was not released")
	}
}
