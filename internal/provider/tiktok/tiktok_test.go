package tiktok

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"sever_provider/internal/provider"
)

func TestPoolRoundRobinAndCooldown(t *testing.T) {
	p := NewPool([]string{"a", "b"})
	first, err := p.Acquire(time.Now())
	if err != nil || first != "a" {
		t.Fatalf("first acquire: %q %v", first, err)
	}
	second, err := p.Acquire(time.Now())
	if err != nil || second != "b" {
		t.Fatalf("second acquire: %q %v", second, err)
	}
	p.RateLimited("a")
	got, err := p.Acquire(time.Now())
	if err != nil || got != "b" {
		t.Fatalf("cooldown acquire: %q %v", got, err)
	}
}

func TestPoolRemoveSession(t *testing.T) {
	pool := NewPool([]string{"a", "b", "c"})
	pool.RemoveSession("b")
	if pool.Size() != 2 {
		t.Fatalf("pool size=%d, want 2", pool.Size())
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		id, err := pool.Acquire(time.Now().Add(time.Duration(i) * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		seen[id] = true
	}
	if seen["b"] || !seen["a"] || !seen["c"] {
		t.Fatalf("remaining sessions=%v", seen)
	}
}

func TestEndpointFallbackAndPenalty(t *testing.T) {
	var deadCalls atomic.Int32
	var goodCalls atomic.Int32

	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadCalls.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer deadServer.Close()

	goodServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		b64 := base64.StdEncoding.EncodeToString([]byte("mp3data"))
		w.Write([]byte(`{"message":"success","data":{"v_str":"` + b64 + `"}}`))
	}))
	defer goodServer.Close()

	pool := NewPool([]string{"session-test"})
	adapter := New(&http.Client{Timeout: 5 * time.Second}, pool)
	adapter.Endpoints = []string{deadServer.URL, goodServer.URL}

	audio, mime, err := adapter.Speech(context.Background(), provider.SpeechRequest{Input: "test sentence", Voice: "BV075_streaming"}, nil)
	if err != nil {
		t.Fatalf("speech failed: %v", err)
	}
	if mime != "audio/mpeg" || string(audio) != "mp3data" {
		t.Fatalf("unexpected audio output: %s %s", mime, string(audio))
	}
	if deadCalls.Load() != 1 || goodCalls.Load() != 1 {
		t.Fatalf("expected dead=1 good=1, got dead=%d good=%d", deadCalls.Load(), goodCalls.Load())
	}
	// Second speech call must skip dead endpoint due to penalty
	_, _, err = adapter.Speech(context.Background(), provider.SpeechRequest{Input: "test sentence 2", Voice: "BV075_streaming"}, nil)
	if err != nil {
		t.Fatalf("second speech failed: %v", err)
	}
	if deadCalls.Load() != 1 {
		t.Fatalf("penalized endpoint should have been skipped, deadCalls=%d", deadCalls.Load())
	}
}

func TestSplitTextUTF8(t *testing.T) {
	parts := splitText("Xin chào thế giới này", 10)
	if len(parts) < 2 {
		t.Fatalf("expected chunks, got %#v", parts)
	}
	for _, part := range parts {
		if len([]byte(part)) > 10 {
			t.Fatalf("chunk exceeds byte limit: %q", part)
		}
	}
}
