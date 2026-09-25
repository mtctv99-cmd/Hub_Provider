package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sever_provider/internal/admin"
	"sever_provider/internal/db"
	"sever_provider/internal/identity"
	"sever_provider/internal/secret"
)

func TestDeepSeekAccountsHandler(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	store, err := secret.NewStore("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	mgr := identity.NewManager(database.DB, store)
	auth := admin.NewFromEnv()
	token, err := auth.Login(admin.DefaultPassword)
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(nil, nil)
	h.SetCredentials(mgr)
	h.SetAdminAuth(auth)

	// 1. Unauthenticated request -> 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/admin/api/deepseek/accounts", nil)
	unauthRec := httptest.NewRecorder()
	h.DeepSeekAccounts(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", unauthRec.Code)
	}

	// 2. GET empty list -> 200
	getReq := httptest.NewRequest(http.MethodGet, "/admin/api/deepseek/accounts", nil)
	getReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	getRec := httptest.NewRecorder()
	h.DeepSeekAccounts(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", getRec.Code)
	}
	var emptyResp struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.NewDecoder(getRec.Body).Decode(&emptyResp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(emptyResp.Accounts) != 0 {
		t.Fatalf("expected 0 accounts, got %d", len(emptyResp.Accounts))
	}

	// 3. POST create valid account -> 201
	postBody, _ := json.Marshal(map[string]any{
		"email":           "test@deepseek.com",
		"token":           "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.dummy_token",
		"max_concurrency": 2,
	})
	postReq := httptest.NewRequest(http.MethodPost, "/admin/api/deepseek/accounts", bytes.NewReader(postBody))
	postReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	postRec := httptest.NewRecorder()
	h.DeepSeekAccounts(postRec, postReq)
	if postRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", postRec.Code, postRec.Body.String())
	}

	// 4. GET accounts -> contains created account
	getReq2 := httptest.NewRequest(http.MethodGet, "/admin/api/deepseek/accounts", nil)
	getReq2.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	getRec2 := httptest.NewRecorder()
	h.DeepSeekAccounts(getRec2, getReq2)
	var listResp struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.NewDecoder(getRec2.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(listResp.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(listResp.Accounts))
	}

	// Check secret was stored
	credID := "deepseek-test@deepseek.com-deepseek_web"
	secretBytes, err := mgr.ReadSecret(context.Background(), credID)
	if err != nil {
		t.Fatalf("failed to read secret: %v", err)
	}
	if !strings.Contains(string(secretBytes), "dummy_token") {
		t.Fatalf("expected token in secret, got %s", string(secretBytes))
	}

	// 5. DELETE account -> 200
	delBody, _ := json.Marshal(map[string]any{
		"identity_id": "deepseek-test@deepseek.com",
	})
	delReq := httptest.NewRequest(http.MethodDelete, "/admin/api/deepseek/accounts", bytes.NewReader(delBody))
	delReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	delRec := httptest.NewRecorder()
	h.DeepSeekAccounts(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", delRec.Code, delRec.Body.String())
	}

	// 6. GET accounts after delete -> 0
	getReq3 := httptest.NewRequest(http.MethodGet, "/admin/api/deepseek/accounts", nil)
	getReq3.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	getRec3 := httptest.NewRecorder()
	h.DeepSeekAccounts(getRec3, getReq3)
	var listResp2 struct {
		Accounts []map[string]any `json:"accounts"`
	}
	json.NewDecoder(getRec3.Body).Decode(&listResp2)
	if len(listResp2.Accounts) != 0 {
		t.Fatalf("expected 0 accounts after deletion, got %d", len(listResp2.Accounts))
	}
}

func TestDeepSeekAutoLoginEndpoints(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	store, err := secret.NewStore("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	mgr := identity.NewManager(database.DB, store)
	auth := admin.NewFromEnv()
	token, err := auth.Login(admin.DefaultPassword)
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(nil, nil)
	h.SetCredentials(mgr)
	h.SetAdminAuth(auth)

	// Status check unauthenticated -> 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/admin/api/deepseek/auto-login/status", nil)
	unauthRec := httptest.NewRecorder()
	h.DeepSeekAutoLoginStatus(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthRec.Code)
	}

	// Status check authenticated -> 200
	statusReq := httptest.NewRequest(http.MethodGet, "/admin/api/deepseek/auto-login/status", nil)
	statusReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	statusRec := httptest.NewRecorder()
	h.DeepSeekAutoLoginStatus(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", statusRec.Code)
	}

	// Stop authenticated -> 200
	stopReq := httptest.NewRequest(http.MethodPost, "/admin/api/deepseek/auto-login/stop", nil)
	stopReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	stopRec := httptest.NewRecorder()
	h.DeepSeekAutoLoginStop(stopRec, stopReq)
	if stopRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", stopRec.Code)
	}
}
