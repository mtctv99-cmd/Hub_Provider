package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/db"
	"sever_provider/internal/identity"
	"sever_provider/internal/secret"
)

func copilotTestURL() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"oid":"11111111-2222-3333-4444-555555555555","tid":"66666666-7777-8888-9999-000000000000","preferred_username":"user@example.com"}`, time.Now().Add(time.Hour).Unix())))
	return "wss://substrate.office.com/m365Copilot/Chathub/11111111-2222-3333-4444-555555555555@66666666-7777-8888-9999-000000000000?access_token=" + header + "." + payload + ".signature"
}

func TestCopilotImportAndList(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := secret.NewStore("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	manager := identity.NewManager(database.DB, store)
	var adminSecret [24]byte
	if _, err := rand.Read(adminSecret[:]); err != nil {
		t.Fatal(err)
	}
	adminPassword := hex.EncodeToString(adminSecret[:])
	t.Setenv("ADMIN_PASSWORD", adminPassword)
	auth := admin.NewFromEnv()
	h := NewHandler(nil, nil)
	h.SetCredentials(manager)
	h.SetIdentityDetails(manager)
	h.SetAdminAuth(auth)
	server := httptest.NewServer(NewRouter(h, ""))
	defer server.Close()

	tokenURL := copilotTestURL()
	body := func(email, id string) []byte {
		payload, _ := json.Marshal(map[string]any{
			"identity":   map[string]any{"email": email, "id": id},
			"credential": map[string]any{"token_url": tokenURL, "max_concurrency": 2, "pool_role": "AUTO"},
		})
		return payload
	}
	request := func(method, path string, data []byte, cookies []*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(data))
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		NewRouter(h, "").ServeHTTP(w, r)
		return w
	}
	if got := request(http.MethodPost, "/admin/api/copilot/add", body("user@example.com", ""), nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", got.Code)
	}
	login, err := http.Post(server.URL+"/admin/api/login", "application/json", strings.NewReader(fmt.Sprintf(`{"password":%q}`, adminPassword)))
	if err != nil {
		t.Fatal(err)
	}
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login returned %d", login.StatusCode)
	}
	cookies := login.Cookies()
	callLogin := func(method string, handler http.HandlerFunc, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/admin/api/copilot/auto-login", nil)
		if authenticated {
			for _, cookie := range cookies {
				r.AddCookie(cookie)
			}
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	if got := callLogin(http.MethodGet, h.CopilotAutoLoginStatus, false); got.Code != http.StatusUnauthorized {
		t.Fatalf("expected protected status, got %d", got.Code)
	}
	if got := callLogin(http.MethodGet, h.CopilotAutoLoginStatus, true); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"captured":false`) || strings.Contains(got.Body.String(), "access_token") {
		t.Fatalf("unexpected status response: %d %s", got.Code, got.Body.String())
	}
	if got := callLogin(http.MethodGet, h.CopilotAutoLogin, true); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for login start, got %d", got.Code)
	}
	if got := callLogin(http.MethodPost, h.CopilotAutoLoginStop, true); got.Code != http.StatusOK {
		t.Fatalf("expected stop success, got %d", got.Code)
	}
	if got := request(http.MethodGet, "/admin/api/copilot/add", nil, cookies); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", got.Code)
	}
	invalid := body("invalid@example.com", "")
	invalid = bytes.Replace(invalid, []byte("substrate.office.com"), []byte("substrate.office.com.evil.test"), 1)
	if got := request(http.MethodPost, "/admin/api/copilot/add", invalid, cookies); got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for untrusted host, got %d", got.Code)
	}
	added := request(http.MethodPost, "/admin/api/copilot/add", body("user@example.com", ""), cookies)
	if added.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", added.Code, added.Body.String())
	}
	if strings.Contains(added.Body.String(), tokenURL) || !strings.Contains(added.Body.String(), `"DEGRADED"`) {
		t.Fatalf("unsafe response or status: %s", added.Body.String())
	}
	listed := request(http.MethodGet, "/admin/api/copilot/accounts", nil, cookies)
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), tokenURL) || !strings.Contains(listed.Body.String(), "user@example.com") || strings.Contains(listed.Body.String(), "source_ref") || strings.Contains(listed.Body.String(), "fingerprint") {
		t.Fatalf("unexpected account list: %d %s", listed.Code, listed.Body.String())
	}
	if got := request(http.MethodGet, "/admin/api/copilot/accounts", nil, nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("expected protected account list, got %d", got.Code)
	}
	var saved struct {
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(added.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	plaintext, err := manager.ReadSecret(context.Background(), saved.CredentialID)
	if err != nil || !strings.Contains(string(plaintext), tokenURL) {
		t.Fatalf("encrypted credential not retrievable: %v", err)
	}
	var encrypted []byte
	if err := database.DB.QueryRow(`SELECT encrypted_payload FROM credential_secrets WHERE credential_id=?`, saved.CredentialID).Scan(&encrypted); err != nil || bytes.Contains(encrypted, []byte(tokenURL)) {
		t.Fatalf("credential was not encrypted: %v", err)
	}
	repeated := request(http.MethodPost, "/admin/api/copilot/add", body("user@example.com", ""), cookies)
	if repeated.Code != http.StatusCreated {
		t.Fatalf("repeat import returned %d: %s", repeated.Code, repeated.Body.String())
	}
	var badPayload map[string]any
	if err := json.Unmarshal(body("another@example.com", ""), &badPayload); err != nil {
		t.Fatal(err)
	}
	badPayload["credential"].(map[string]any)["id"] = saved.CredentialID
	badBody, _ := json.Marshal(badPayload)
	if got := request(http.MethodPost, "/admin/api/copilot/add", badBody, cookies); got.Code != http.StatusUnprocessableEntity || strings.Contains(got.Body.String(), tokenURL) {
		t.Fatalf("expected redacted conflict response, got %d: %s", got.Code, got.Body.String())
	}
	var count int
	if err := database.DB.QueryRow(`SELECT count(*) FROM identities WHERE normalized_email='another@example.com'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("identity was left behind after conflict: count=%d err=%v", count, err)
	}

	// Test import with empty email auto-populates from token
	autoEmail := request(http.MethodPost, "/admin/api/copilot/add", body("", ""), cookies)
	if autoEmail.Code != http.StatusCreated {
		t.Fatalf("expected 201 with empty email, got %d: %s", autoEmail.Code, autoEmail.Body.String())
	}

	// Test /admin/api/copilot/token endpoint (used by grab_token.py)
	tokenPayload, _ := json.Marshal(map[string]any{"url": tokenURL})
	tokenResp := request(http.MethodPost, "/admin/api/copilot/token", tokenPayload, cookies)
	if tokenResp.Code != http.StatusOK || !strings.Contains(tokenResp.Body.String(), `"success":true`) {
		t.Fatalf("token endpoint failed: %d %s", tokenResp.Code, tokenResp.Body.String())
	}

	// Test /admin/api/copilot/status endpoint
	statusResp := request(http.MethodGet, "/admin/api/copilot/status", nil, cookies)
	if statusResp.Code != http.StatusOK || !strings.Contains(statusResp.Body.String(), `"accounts_count"`) {
		t.Fatalf("status endpoint failed: %d %s", statusResp.Code, statusResp.Body.String())
	}

	// Test /admin/api/copilot/delete endpoint
	var tokenData struct {
		IdentityID string `json:"identity_id"`
	}
	_ = json.Unmarshal(tokenResp.Body.Bytes(), &tokenData)
	delPayload, _ := json.Marshal(map[string]any{"id": tokenData.IdentityID})
	delResp := request(http.MethodPost, "/admin/api/copilot/delete", delPayload, cookies)
	if delResp.Code != http.StatusOK || !strings.Contains(delResp.Body.String(), `"deleted_id"`) {
		t.Fatalf("delete endpoint failed: %d %s", delResp.Code, delResp.Body.String())
	}
}
