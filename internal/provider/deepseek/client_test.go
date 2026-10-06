package deepseek

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sever_provider/internal/provider/deepseek/pow"
)

func TestClientAndroidHeaders(t *testing.T) {
	c := NewClient("", nil)
	h := c.AndroidHeaders("test-token-123")

	if h.Get("User-Agent") != AndroidUserAgent {
		t.Fatalf("User-Agent = %q, want %q", h.Get("User-Agent"), AndroidUserAgent)
	}
	if h.Get("x-client-platform") != "android" {
		t.Fatalf("x-client-platform = %q, want android", h.Get("x-client-platform"))
	}
	if h.Get("x-client-version") != "2.0.4" {
		t.Fatalf("x-client-version = %q, want 2.0.4", h.Get("x-client-version"))
	}
	if h.Get("x-client-locale") != "zh_CN" {
		t.Fatalf("x-client-locale = %q, want zh_CN", h.Get("x-client-locale"))
	}
	if h.Get("Authorization") != "Bearer test-token-123" {
		t.Fatalf("Authorization = %q, want Bearer test-token-123", h.Get("Authorization"))
	}
}

func TestClientGetPowChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DefaultCreatePowPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer mock-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["target_path"] != DefaultCompletionPath {
			t.Errorf("target_path = %v", req["target_path"])
		}

		resp := map[string]any{
			"code": 0,
			"msg":  "",
			"data": map[string]any{
				"biz_code": 0,
				"biz_data": map[string]any{
					"challenge": map[string]any{
						"algorithm":   "DeepSeekHashV1",
						"challenge":   "64e56598553dae00c3b06cf72a5a542b2608a0dfcecdfd9b9bf81c201e7e4479",
						"salt":        "salt123",
						"expire_at":   1775322810,
						"difficulty":  1000,
						"signature":   "sig123",
						"target_path": DefaultCompletionPath,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client())
	ch, err := c.GetPowChallenge(context.Background(), "mock-token", DefaultCompletionPath)
	if err != nil {
		t.Fatalf("GetPowChallenge failed: %v", err)
	}
	if ch.Algorithm != "DeepSeekHashV1" || ch.Salt != "salt123" {
		t.Fatalf("unexpected challenge: %+v", ch)
	}
}

func TestClientCreateSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DefaultCreateSessionPath {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"code": 0,
			"msg":  "",
			"data": map[string]any{
				"biz_code": 0,
				"biz_data": map[string]any{
					"id": "chat-sess-xyz-999",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client())
	sessionID, err := c.CreateSession(context.Background(), "mock-token")
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if sessionID != "chat-sess-xyz-999" {
		t.Fatalf("sessionID = %q, want chat-sess-xyz-999", sessionID)
	}
}

func TestClientSendPromptAutoPow(t *testing.T) {
	var receivedPowHeader string
	var receivedPayload map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case DefaultCreateSessionPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"biz_data": map[string]any{
						"id": "sess-auto-1",
					},
				},
			})
		case DefaultCreatePowPath:
			// Calculate target hash for answer 12
			salt := "salttest"
			expireAt := int64(1800000000)
			prefix := pow.BuildPrefix(salt, expireAt)
			h := pow.DeepSeekHashV1([]byte(prefix + "12"))
			target := hex.EncodeToString(h[:])

			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"biz_data": map[string]any{
						"challenge": map[string]any{
							"algorithm":   "DeepSeekHashV1",
							"challenge":   target,
							"salt":        salt,
							"expire_at":   expireAt,
							"difficulty":  1000,
							"signature":   "sig-test",
							"target_path": DefaultCompletionPath,
						},
					},
				},
			})
		case DefaultCompletionPath:
			receivedPowHeader = r.Header.Get(pow.HeaderName)
			_ = json.NewDecoder(r.Body).Decode(&receivedPayload)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"v\":\"hello world\"}\n\ndata: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, &http.Client{Timeout: 5 * time.Second})
	resp, sessID, err := c.SendPrompt(context.Background(), "token-1", "", "Hi DeepSeek", false, false)
	if err != nil {
		t.Fatalf("SendPrompt failed: %v", err)
	}
	defer resp.Body.Close()

	if sessID != "sess-auto-1" {
		t.Fatalf("sessID = %q, want sess-auto-1", sessID)
	}
	if receivedPowHeader == "" {
		t.Fatal("expected x-ds-pow-response header, got empty")
	}
	if receivedPayload["prompt"] != "Hi DeepSeek" {
		t.Fatalf("prompt = %v, want 'Hi DeepSeek'", receivedPayload["prompt"])
	}
	if receivedPayload["chat_session_id"] != "sess-auto-1" {
		t.Fatalf("chat_session_id = %v", receivedPayload["chat_session_id"])
	}
}
