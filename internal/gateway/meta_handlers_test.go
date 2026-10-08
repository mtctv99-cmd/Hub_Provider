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

func TestMetaAccountsHandler(t *testing.T) {
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
	h.SetIdentityDetails(mgr)
	h.SetAdminAuth(auth)

	// 1. Unauthenticated request -> 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/admin/api/meta/accounts", nil)
	unauthRec := httptest.NewRecorder()
	h.MetaAccounts(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", unauthRec.Code)
	}

	// 2. GET empty list -> 200
	getReq := httptest.NewRequest(http.MethodGet, "/admin/api/meta/accounts", nil)
	getReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	getRec := httptest.NewRecorder()
	h.MetaAccounts(getRec, getReq)
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
		"email":           "user@meta.ai",
		"cookies":         "c_user=10001; datr=abc123xyz; xs=token456",
		"max_concurrency": 2,
	})
	postReq := httptest.NewRequest(http.MethodPost, "/admin/api/meta/accounts", bytes.NewReader(postBody))
	postReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	postRec := httptest.NewRecorder()
	h.MetaAccounts(postRec, postReq)
	if postRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", postRec.Code, postRec.Body.String())
	}

	// 4. GET accounts -> contains created account
	getReq2 := httptest.NewRequest(http.MethodGet, "/admin/api/meta/accounts", nil)
	getReq2.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	getRec2 := httptest.NewRecorder()
	h.MetaAccounts(getRec2, getReq2)
	var listResp struct {
		Accounts []struct {
			Identity struct {
				ID            string `json:"id"`
				Email         string `json:"email"`
				ProviderGroup string `json:"provider_group"`
			} `json:"identity"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(getRec2.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(listResp.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(listResp.Accounts))
	}
	if listResp.Accounts[0].Identity.Email != "user@meta.ai" {
		t.Fatalf("expected user@meta.ai, got %s", listResp.Accounts[0].Identity.Email)
	}

	// Check secret stored
	creds, err := mgr.List(context.Background(), identity.DomainMetaWeb)
	if err != nil || len(creds) == 0 {
		t.Fatalf("expected stored credential, got %v", err)
	}
	secretBytes, err := mgr.ReadSecret(context.Background(), creds[0].ID)
	if err != nil {
		t.Fatalf("failed to read secret: %v", err)
	}
	if !strings.Contains(string(secretBytes), "c_user=10001") {
		t.Fatalf("expected cookies in secret, got %s", string(secretBytes))
	}

	// 5. DELETE account -> 200
	delBody, _ := json.Marshal(map[string]any{
		"identity_id": listResp.Accounts[0].Identity.ID,
	})
	delReq := httptest.NewRequest(http.MethodDelete, "/admin/api/meta/accounts", bytes.NewReader(delBody))
	delReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	delRec := httptest.NewRecorder()
	h.MetaAccounts(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", delRec.Code, delRec.Body.String())
	}

	// 6. GET accounts after delete -> 0
	getReq3 := httptest.NewRequest(http.MethodGet, "/admin/api/meta/accounts", nil)
	getReq3.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	getRec3 := httptest.NewRecorder()
	h.MetaAccounts(getRec3, getReq3)
	var listResp2 struct {
		Accounts []any `json:"accounts"`
	}
	json.NewDecoder(getRec3.Body).Decode(&listResp2)
	if len(listResp2.Accounts) != 0 {
		t.Fatalf("expected 0 accounts after deletion, got %d", len(listResp2.Accounts))
	}
}

func TestMetaAutoLoginEndpoints(t *testing.T) {
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

	// Status unauthenticated -> 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/admin/api/meta/auto-login/status", nil)
	unauthRec := httptest.NewRecorder()
	h.MetaAutoLoginStatus(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthRec.Code)
	}

	// Status authenticated -> 200
	statusReq := httptest.NewRequest(http.MethodGet, "/admin/api/meta/auto-login/status", nil)
	statusReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	statusRec := httptest.NewRecorder()
	h.MetaAutoLoginStatus(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", statusRec.Code)
	}

	// Stop authenticated -> 200
	stopReq := httptest.NewRequest(http.MethodPost, "/admin/api/meta/auto-login/stop", nil)
	stopReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	stopRec := httptest.NewRecorder()
	h.MetaAutoLoginStop(stopRec, stopReq)
	if stopRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", stopRec.Code)
	}
}
