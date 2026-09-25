package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	_ "modernc.org/sqlite"
	"sever_provider/internal/admin"
	"sever_provider/internal/identity"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/secret"
)

func setupTestDBForGateway(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	schema := `
	CREATE TABLE IF NOT EXISTS identities (
		id TEXT PRIMARY KEY,
		provider_group TEXT NOT NULL,
		email TEXT NOT NULL,
		normalized_email TEXT,
		priority INTEGER NOT NULL DEFAULT 0,
		global_status TEXT NOT NULL DEFAULT 'HEALTHY',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS credentials (
		id TEXT PRIMARY KEY,
		identity_id TEXT NOT NULL REFERENCES identities(id) ON DELETE CASCADE,
		domain TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'HEALTHY',
		max_concurrency INTEGER NOT NULL DEFAULT 1,
		consecutive_errors INTEGER DEFAULT 0,
		source_ref TEXT,
		fingerprint TEXT,
		pool_role TEXT NOT NULL DEFAULT 'AUTO',
		last_error_kind TEXT,
		last_used_at DATETIME,
		cooldown_until DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(identity_id, domain)
	);
	CREATE TABLE IF NOT EXISTS credential_secrets (
		credential_id TEXT PRIMARY KEY REFERENCES credentials(id) ON DELETE CASCADE,
		encrypted_payload BLOB NOT NULL,
		nonce BLOB NOT NULL,
		key_version INTEGER NOT NULL DEFAULT 1,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS api_keys (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		key_hash TEXT NOT NULL UNIQUE,
		active INTEGER NOT NULL DEFAULT 1,
		scopes TEXT NOT NULL DEFAULT 'all',
		expires_at DATETIME,
		rate_limit_rpm INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_used_at DATETIME
	);
	CREATE TABLE IF NOT EXISTS api_key_rate_windows (
		key_id TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
		window_start INTEGER NOT NULL,
		request_count INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(key_id, window_start)
	);
		CREATE TABLE IF NOT EXISTS observability_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT,
			event_type TEXT NOT NULL,
			provider TEXT,
			model TEXT,
			account_id TEXT,
			latency_ms REAL,
			error_kind TEXT,
			detail TEXT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS admin_credentials (
			id TEXT PRIMARY KEY,
			password_hash TEXT NOT NULL,
			salt TEXT NOT NULL,
			must_change_password INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			password_changed_at DATETIME
		);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to create schema: %v", err)
	}
	return db
}

func TestAPIKeyMiddlewareAndEndpoints(t *testing.T) {
	db := setupTestDBForGateway(t)
	apiKeys := admin.NewAPIKeys(db)
	auditLog := admin.NewAuditLogger(db)
	os.Setenv("ADMIN_PASSWORD", "test-admin-secret")
	defer os.Unsetenv("ADMIN_PASSWORD")
	auth := admin.NewFromEnv()

	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "mock-chat", Provider: "mock", Capabilities: nil, TimeoutSec: 10})
	handler := NewHandler(nil, models)
	handler.SetAdminAuth(auth)
	handler.SetAPIKeys(apiKeys)
	handler.SetAudit(auditLog)

	router := NewRouterWithLimits(handler, "", 1024*1024)
	ts := httptest.NewServer(router)
	defer ts.Close()

	// 1. Without any active keys in DB and no static apiKey, public endpoint /health should succeed
	resp, err := http.Get(ts.URL + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /health, got %v (%d)", err, resp.StatusCode)
	}

	// 2. Admin login to obtain token
	loginBody, _ := json.Marshal(map[string]string{"password": "test-admin-secret"})
	resp, err = http.Post(ts.URL+"/admin/api/login", "application/json", bytes.NewReader(loginBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login failed: %v (%d)", err, resp.StatusCode)
	}
	var loginRes struct {
		Authenticated bool `json:"authenticated"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&loginRes)
	if !loginRes.Authenticated {
		t.Fatalf("login response indicated not authenticated")
	}
	cookies := resp.Cookies()

	// Helper for admin requests
	doAdminReq := func(method, path string, body []byte) *http.Response {
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request %s %s: %v", method, path, err)
		}
		return r
	}

	// 3. Create an API key via Admin API
	createBody, _ := json.Marshal(map[string]string{"name": "production-service"})
	resp = doAdminReq(http.MethodPost, "/admin/api/apikeys", createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from POST /admin/api/apikeys, got %d", resp.StatusCode)
	}
	var created struct {
		Key      string       `json:"key"`
		Metadata admin.APIKey `json:"metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Key == "" || created.Metadata.ID == "" {
		t.Fatalf("expected non-empty key and ID, got key=%q id=%q", created.Key, created.Metadata.ID)
	}

	// 4. List API keys
	resp = doAdminReq(http.MethodGet, "/admin/api/apikeys", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from GET /admin/api/apikeys, got %d", resp.StatusCode)
	}
	var listRes struct {
		Keys []admin.APIKey `json:"keys"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listRes)
	if len(listRes.Keys) != 1 || listRes.Keys[0].ID != created.Metadata.ID {
		t.Fatalf("expected 1 key in list, got %+v", listRes.Keys)
	}

	// 5. Now that active SQLite API key exists, request to protected /v1/models without auth should fail
	resp, err = http.Get(ts.URL + "/v1/models")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized when active key exists without auth header, got %d", resp.StatusCode)
	}

	// 6. Request to /v1/models with the newly created key should succeed
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK with valid API key, got %v (%d)", err, resp.StatusCode)
	}

	// 7. Revoke the API key
	resp = doAdminReq(http.MethodPost, "/admin/api/apikeys/revoke/"+created.Metadata.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from revoke, got %d", resp.StatusCode)
	}

	// 8. Request with revoked key should now fail
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized with revoked API key, got %d", resp.StatusCode)
	}

	// 9. Delete the API key
	resp = doAdminReq(http.MethodDelete, "/admin/api/apikeys/delete/"+created.Metadata.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from delete, got %d", resp.StatusCode)
	}
}

type mockGeminiValidator struct{}

func (mockGeminiValidator) ValidateCookie(context.Context, string) error { return nil }

func TestGoogleCredentialRouting(t *testing.T) {
	db := setupTestDBForGateway(t)
	secStore, _ := secret.NewStore("")
	idManager := identity.NewManager(db, secStore)
	os.Setenv("ADMIN_PASSWORD", "admin-test-pass")
	defer os.Unsetenv("ADMIN_PASSWORD")
	auth := admin.NewFromEnv()

	handler := NewHandler(nil, nil)
	handler.SetCredentials(idManager)
	handler.SetIdentityDetails(idManager)
	handler.SetGeminiValidator(mockGeminiValidator{})
	handler.SetAdminAuth(auth)

	router := NewRouter(handler, "")
	ts := httptest.NewServer(router)
	defer ts.Close()

	// Seed identity and credential
	ctx := context.Background()
	_, err := idManager.UpsertGoogleAccount(ctx, "testuser@gmail.com", 1, []identity.GoogleCredentialSpec{
		{
			Domain:         "gemini_web",
			ID:             "cred-gemini-1",
			MaxConcurrency: 2,
			Secret:         []byte(`{"cookie":"dummy"}`),
		},
	})
	if err != nil {
		t.Fatalf("failed to seed google account: %v", err)
	}

	// Login
	loginBody, _ := json.Marshal(map[string]string{"password": "admin-test-pass"})
	resp, err := http.Post(ts.URL+"/admin/api/login", "application/json", bytes.NewReader(loginBody))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("admin login failed: %v", err)
	}
	cookies := resp.Cookies()

	doAdmin := func(method, path string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("doAdmin %s %s: %v", method, path, err)
		}
		return r
	}

	// 1. POST /admin/api/google/credentials/cred-gemini-1/disable
	// Verify that the route handler targets "cred-gemini-1" and NOT "disable" as the ID!
	resp = doAdmin(http.MethodPost, "/admin/api/google/credentials/cred-gemini-1/disable")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from disable, got %d", resp.StatusCode)
	}
	var disableRes map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&disableRes)
	if disableRes["credential_id"] != "cred-gemini-1" || disableRes["status"] != "DISABLED" {
		t.Fatalf("unexpected disable response: %+v", disableRes)
	}

	// 2. POST /admin/api/google/credentials/cred-gemini-1/validate
	resp = doAdmin(http.MethodPost, "/admin/api/google/credentials/cred-gemini-1/validate")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from validate, got %d", resp.StatusCode)
	}
	var valRes map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&valRes)
	if valRes["credential_id"] != "cred-gemini-1" || valRes["status"] != "HEALTHY" {
		t.Fatalf("unexpected validate response: %+v", valRes)
	}

	// 3. DELETE /admin/api/google/credentials/cred-gemini-1
	resp = doAdmin(http.MethodDelete, "/admin/api/google/credentials/cred-gemini-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from delete credential, got %d", resp.StatusCode)
	}
}

func TestAuditLogEndpoints(t *testing.T) {
	db := setupTestDBForGateway(t)
	auditLog := admin.NewAuditLogger(db)
	os.Setenv("ADMIN_PASSWORD", "admin-test-pass")
	defer os.Unsetenv("ADMIN_PASSWORD")
	auth := admin.NewFromEnv()

	handler := NewHandler(nil, nil)
	handler.SetAdminAuth(auth)
	handler.SetAudit(auditLog)

	router := NewRouter(handler, "")
	ts := httptest.NewServer(router)
	defer ts.Close()

	// Record a log entry
	_ = auditLog.Record(context.Background(), admin.AuditEvent{
		RequestID: "req-123",
		EventType: "tts_generation",
		Provider:  "piper_local",
		Detail:    "synthesized audio",
	})

	// Login
	loginBody, _ := json.Marshal(map[string]string{"password": "admin-test-pass"})
	resp, _ := http.Post(ts.URL+"/admin/api/login", "application/json", bytes.NewReader(loginBody))
	cookies := resp.Cookies()

	doAdmin := func(method, path string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("doAdmin %s %s: %v", method, path, err)
		}
		return r
	}

	// GET /admin/api/logs
	resp = doAdmin(http.MethodGet, "/admin/api/logs")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /admin/api/logs, got %d", resp.StatusCode)
	}
	var logsRes struct {
		Lines []admin.AuditLine `json:"lines"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&logsRes)
	if len(logsRes.Lines) != 1 || logsRes.Lines[0].EventType != "tts_generation" {
		t.Fatalf("unexpected logs: %+v", logsRes.Lines)
	}

	// POST /admin/api/logs/clear
	resp = doAdmin(http.MethodPost, "/admin/api/logs/clear")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /admin/api/logs/clear, got %d", resp.StatusCode)
	}

	// Verify empty logs
	resp = doAdmin(http.MethodGet, "/admin/api/logs")
	_ = json.NewDecoder(resp.Body).Decode(&logsRes)
	if len(logsRes.Lines) != 0 {
		t.Fatalf("expected 0 logs after clear, got %d", len(logsRes.Lines))
	}
}
