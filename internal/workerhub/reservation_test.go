package workerhub

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestConcurrentDispatchWaitsForSlot(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	conn := dialWorker(t, "ws"+server.URL[len("http"):], Registration{WorkerID: "one", Model: "gemma", MaxConcurrency: 1})
	defer conn.Close()
	go func() {
		for {
			var req map[string]any
			if conn.ReadJSON(&req) != nil {
				return
			}
			time.Sleep(time.Millisecond)
			if conn.WriteJSON(map[string]any{"type": "job_result", "request_id": req["request_id"], "text": "ok", "done": true}) != nil {
				return
			}
		}
	}()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 80)
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := hub.Submit(ctx, "gemma", nil)
			if err != nil {
				errs <- err
				return
			}
			for range stream.Events {
			}
			if err := <-stream.Err; err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("connected worker incorrectly rejected burst: %v", err)
	}
	if snap := hub.Snapshot(); len(snap) != 1 || snap[0].Active != 0 {
		t.Fatal("slot leaked")
	}
}
