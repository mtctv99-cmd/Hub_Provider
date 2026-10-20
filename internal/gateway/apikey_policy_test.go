package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/scheduler"
)

func TestAPIKeyScopeExpiryRateAndMutations(t *testing.T) {
	db := setupTestDBForGateway(t)
	keys := admin.NewAPIKeys(db)
	t.Setenv("ADMIN_PASSWORD", "policy-test-pass")
	h := NewHandler(nil, scheduler.NewModelRegistry())
	h.SetAdminAuth(admin.NewFromEnv())
	h.SetAPIKeys(keys)
	server := httptest.NewServer(NewRouter(h, ""))
	defer server.Close()
	login, err := http.Post(server.URL+"/admin/api/login", "application/json", strings.NewReader(`{"password":"policy-test-pass"}`))
	if err != nil || login.StatusCode != 200 {
		t.Fatalf("login: %v %v", err, login)
	}
	cookies := login.Cookies()
	_ = login.Body.Close()
	do := func(method, path string, body []byte, key string, authenticated bool) *http.Response {
		r, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if authenticated {
			for _, c := range cookies {
				r.AddCookie(c)
			}
		}
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	create := do("POST", "/admin/api/apikeys", []byte(`{"name":"restricted","scopes":["models"],"expires_in_days":7,"rate_limit_rpm":2}`), "", true)
	if create.StatusCode != 201 {
		t.Fatalf("create: %d", create.StatusCode)
	}
	var data struct {
		Key      string       `json:"key"`
		Metadata admin.APIKey `json:"metadata"`
	}
	if err := json.NewDecoder(create.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	_ = create.Body.Close()
	if data.Metadata.ExpiresAt == nil || data.Metadata.RateLimitRPM != 2 {
		t.Fatalf("metadata: %+v", data.Metadata)
	}
	for i := 0; i < 2; i++ {
		r := do("GET", "/v1/models", nil, data.Key, false)
		if r.StatusCode != 200 {
			t.Fatalf("models %d: %d", i, r.StatusCode)
		}
		_ = r.Body.Close()
	}
	r := do("POST", "/v1/chat/completions", nil, data.Key, false)
	if r.StatusCode != 403 {
		t.Fatalf("scope status %d", r.StatusCode)
	}
	_ = r.Body.Close()
	r = do("POST", "/admin/api/apikeys/delete/"+data.Metadata.ID, nil, "", true)
	if r.StatusCode != 405 {
		t.Fatalf("wrong route method %d", r.StatusCode)
	}
	_ = r.Body.Close()
	r = do("POST", "/admin/api/apikeys/revoke/"+data.Metadata.ID, nil, "", true)
	if r.StatusCode != 200 {
		t.Fatalf("revoke status %d", r.StatusCode)
	}
	_ = r.Body.Close()
	r = do("GET", "/v1/models", nil, "", false)
	if r.StatusCode != 200 {
		t.Fatalf("models list after revoke should remain public: %d", r.StatusCode)
	}
	_ = r.Body.Close()
	r = do("POST", "/admin/api/apikeys/activate/"+data.Metadata.ID, nil, "", true)
	if r.StatusCode != 200 {
		t.Fatalf("activate status %d", r.StatusCode)
	}
	_ = r.Body.Close()
	if _, err := db.Exec(`UPDATE api_keys SET expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC(), data.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	r = do("GET", "/v1/models", nil, data.Key, false)
	if r.StatusCode != 200 {
		t.Fatalf("models list after expiry should remain public: %d", r.StatusCode)
	}
	_ = r.Body.Close()
	r = do("POST", "/admin/api/apikeys/activate/"+data.Metadata.ID, nil, "", true)
	if r.StatusCode != 409 {
		t.Fatalf("expired activation status %d", r.StatusCode)
	}
	_ = r.Body.Close()
}

func TestModelsListRemainsPublicWhenAPIKeysExist(t *testing.T) {
	db := setupTestDBForGateway(t)
	keys := admin.NewAPIKeys(db)
	if _, _, err := keys.Create("existing-client-key"); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, scheduler.NewModelRegistry())
	h.SetAPIKeys(keys)
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models list status=%d, want %d", resp.StatusCode, http.StatusOK)
	}

	chat, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"missing","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Body.Close()
	if chat.StatusCode != http.StatusUnauthorized {
		t.Fatalf("chat without key status=%d, want %d", chat.StatusCode, http.StatusUnauthorized)
	}
}
