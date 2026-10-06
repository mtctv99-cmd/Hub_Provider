package deepseek

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sever_provider/internal/provider/deepseek/pow"
	"testing"
	"time"
)

func TestClientRejectsMutedHTTP200AndPreservesParent(t *testing.T) {
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":0,"data":{"biz_code":5,"biz_data":{"is_muted":1,"mute_until":1800000000}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, &http.Client{Timeout: 5 * time.Second})
	resp, _, err := c.SendPromptWithParent(context.Background(), "token-1", "", "Hi DeepSeek", false, false, 42)
	var muted *RateLimitError
	if !errors.As(err, &muted) || resp != nil || muted.Until.Unix() != 1800000000 {
		t.Fatalf("HTTP200 business error accepted: %v", err)
	}
	if receivedPayload["parent_message_id"] != float64(42) {
		t.Fatal("parent message lost")
	}
	if receivedPowHeader == "" {
		t.Fatal("missing pow")
	}
}
