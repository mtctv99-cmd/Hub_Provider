package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"sever_provider/internal/admin"
	"sever_provider/internal/db"
	"sever_provider/internal/identity"
	"sever_provider/internal/secret"
)

type mockPoolSessionAdder struct {
	added []string
}

func (m *mockPoolSessionAdder) AddSession(s string) {
	m.added = append(m.added, s)
}
func (m *mockPoolSessionAdder) RemoveSession(s string) {
	for i, item := range m.added {
		if item == s {
			m.added = append(m.added[:i], m.added[i+1:]...)
			return
		}
	}
}

func TestTikTokAccountsHandlerAuthAndPoolSync(t *testing.T) {
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
	mockPool := &mockPoolSessionAdder{}

	h := NewHandler(nil, nil)
	h.SetCredentials(mgr)
	h.SetAdminAuth(auth)
	h.SetTikTokPool(mockPool)

	// 1. Unauthenticated request to /admin/api/tts/tiktok/accounts should be 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/admin/api/tts/tiktok/accounts", nil)
	unauthRec := httptest.NewRecorder()
	h.TikTokAccounts(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauth, got %d", unauthRec.Code)
	}

	// 2. Reject concurrency outside the documented capacity range.
	for _, concurrency := range []int{0, 17, 1000} {
		invalidBody, _ := json.Marshal(map[string]any{
			"email": "invalid@example.com", "session_id": "ignored-test-session", "max_concurrency": concurrency,
		})
		invalidReq := httptest.NewRequest(http.MethodPost, "/admin/api/tts/tiktok/accounts", bytes.NewReader(invalidBody))
		invalidReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
		invalidRec := httptest.NewRecorder()
		h.TikTokAccounts(invalidRec, invalidReq)
		if invalidRec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("concurrency %d: expected 422, got %d: %s", concurrency, invalidRec.Code, invalidRec.Body.String())
		}
	}

	// 2. Authenticated POST imports credential and updates pool
	body, _ := json.Marshal(map[string]any{
		"email":           "creator@example.com",
		"session_id":      "sess_mock_token_123",
		"max_concurrency": 4,
	})
	postReq := httptest.NewRequest(http.MethodPost, "/admin/api/tts/tiktok/accounts", bytes.NewReader(body))
	postReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	postRec := httptest.NewRecorder()
	h.TikTokAccounts(postRec, postReq)
	if postRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", postRec.Code, postRec.Body.String())
	}
	if len(mockPool.added) != 1 || mockPool.added[0] != "sess_mock_token_123" {
		t.Fatalf("pool was not updated: %+v", mockPool.added)
	}

	// 3. Authenticated GET lists the imported account
	getReq := httptest.NewRequest(http.MethodGet, "/admin/api/tts/tiktok/accounts", nil)
	getReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	getRec := httptest.NewRecorder()
	h.TikTokAccounts(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", getRec.Code)
	}
	var res struct {
		Accounts []struct {
			ID             string `json:"id"`
			IdentityID     string `json:"identity_id"`
			Status         string `json:"status"`
			MaxConcurrency int    `json:"max_concurrency"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Accounts) != 1 || res.Accounts[0].MaxConcurrency != 4 {
		t.Fatalf("unexpected accounts list: %+v", res.Accounts)
	}

	delBody, _ := json.Marshal(map[string]string{"identity_id": res.Accounts[0].IdentityID})
	delReq := httptest.NewRequest(http.MethodDelete, "/admin/api/tts/tiktok/accounts", bytes.NewReader(delBody))
	delReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	delRec := httptest.NewRecorder()
	h.TikTokAccounts(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("expected 200 delete, got %d: %s", delRec.Code, delRec.Body.String())
	}
	if len(mockPool.added) != 0 {
		t.Fatalf("deleted session remained in pool: %+v", mockPool.added)
	}
	postReadReq := httptest.NewRequest(http.MethodGet, "/admin/api/tts/tiktok/accounts", nil)
	postReadReq.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	postReadRec := httptest.NewRecorder()
	h.TikTokAccounts(postReadRec, postReadReq)
	if !bytes.Contains(postReadRec.Body.Bytes(), []byte(`"accounts":[]`)) {
		t.Fatalf("deleted account still listed: %s", postReadRec.Body.String())
	}
}
