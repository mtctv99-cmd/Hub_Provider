package workerhub

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/provider"
)

func sampleMP4() []byte {
	return append([]byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00"), []byte("fake-moov-payload")...)
}

func videoRequest(model string) provider.VideoRequest {
	return provider.VideoRequest{Model: model, Prompt: "a red tram in the rain", Size: "832x480", Duration: 3}
}

func TestVideoProtocol(t *testing.T) {
	hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(httpHandler(hub))
	defer server.Close()
	conn := dialWorker(t, "ws"+server.URL[len("http"):], Registration{WorkerID: "video", Model: "wan-2.1-1.3b", MaxConcurrency: 1})
	defer conn.Close()

	type result struct {
		resp provider.VideoResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := hub.Video(context.Background(), videoRequest("wan-2.1-1.3b"))
		done <- result{resp, err}
	}()

	var request map[string]any
	if err := conn.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	if request["type"] != "video_request" || request["model"] != "wan-2.1-1.3b" || request["prompt"] != "a red tram in the rain" {
		t.Fatalf("request=%v", request)
	}
	if request["size"] != "832x480" || request["duration"] != float64(3) {
		t.Fatalf("request=%v", request)
	}
	id, _ := request["request_id"].(string)
	if id == "" {
		t.Fatalf("missing request_id: %v", request)
	}
	// Progress events must not be mistaken for the result.
	if err := conn.WriteJSON(map[string]any{"type": "video_progress", "request_id": id, "progress": 0.5}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "video_result", "request_id": id, "mime": "video/mp4",
		"video_base64": base64.StdEncoding.EncodeToString(sampleMP4()), "done": true}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.resp.MIME != "video/mp4" || string(got.resp.Data) != string(sampleMP4()) {
			t.Fatalf("resp=%+v", got.resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("video request hung")
	}
}

func TestVideoRejectsBadWorkerOutput(t *testing.T) {
	cases := []struct {
		name    string
		message map[string]any
		want    string
	}{
		{"wrong mime", map[string]any{"type": "video_result", "mime": "audio/wav", "video_base64": base64.StdEncoding.EncodeToString(sampleMP4())}, "unsupported worker video MIME"},
		{"not mp4", map[string]any{"type": "video_result", "mime": "video/mp4", "video_base64": base64.StdEncoding.EncodeToString([]byte("definitely-not-an-mp4"))}, "not an MP4"},
		{"invalid base64", map[string]any{"type": "video_result", "mime": "video/mp4", "video_base64": "!!!!"}, "invalid or oversized"},
		{"empty", map[string]any{"type": "video_result", "mime": "video/mp4"}, "empty or exceeds limit"},
		{"worker error", map[string]any{"type": "error", "error": "boom"}, "boom"},
		{"ends without result", map[string]any{"type": "video_progress", "progress": 1.0, "done": true}, "ended without video_result"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := New(Config{AuthToken: "test-secret", HeartbeatInterval: time.Hour})
			defer hub.Close()
			server := httptest.NewServer(httpHandler(hub))
			defer server.Close()
			conn := dialWorker(t, "ws"+server.URL[len("http"):], Registration{WorkerID: "video", Model: "ltx-video-2b", MaxConcurrency: 1})
			defer conn.Close()

			done := make(chan error, 1)
			go func() {
				_, err := hub.Video(context.Background(), videoRequest("ltx-video-2b"))
				done <- err
			}()
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				t.Fatal(err)
			}
			tc.message["request_id"] = request["request_id"]
			if err := conn.WriteJSON(tc.message); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err=%v, want contains %q", err, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("video request hung")
			}
		})
	}
}
