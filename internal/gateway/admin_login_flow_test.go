package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sever_provider/internal/admin"
	"sever_provider/internal/scheduler"
)

func TestAdminLoginFlowAndFirstLoginPrompt(t *testing.T) {
	db := setupTestDBForGateway(t)
	auth, err := admin.NewFromEnvWithDB(db)
	if err != nil {
		t.Fatalf("failed to init auth: %v", err)
	}

	models := scheduler.NewModelRegistry()
	handler := NewHandler(nil, models)
	handler.SetAdminAuth(auth)
	handler.SetAPIKeys(admin.NewAPIKeys(db))

	router := NewRouterWithLimits(handler, "", 1024*1024)
	ts := httptest.NewServer(router)
	defer ts.Close()

	// 1. Initial auth check: not authenticated, first_login is true
	resp, err := http.Get(ts.URL + "/admin/api/auth/check")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("auth check failed: %v", err)
	}
	var checkRes struct {
		Authenticated bool `json:"authenticated"`
		FirstLogin    bool `json:"first_login"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&checkRes)
	if checkRes.Authenticated {
		t.Fatalf("expected not authenticated initially")
	}
	if !checkRes.FirstLogin {
		t.Fatalf("expected first_login true initially")
	}

	// 2. Login with wrong password should fail with 401
	badLogin, _ := json.Marshal(map[string]string{"password": "wrong-password"})
	resp, err = http.Post(ts.URL+"/admin/api/login", "application/json", bytes.NewReader(badLogin))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d", resp.StatusCode)
	}

	// 3. Login with default password "123456"
	loginBody, _ := json.Marshal(map[string]string{"password": admin.DefaultPassword})
	resp, err = http.Post(ts.URL+"/admin/api/login", "application/json", bytes.NewReader(loginBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("default password login failed: %v (%d)", err, resp.StatusCode)
	}
	var loginRes struct {
		Authenticated bool `json:"authenticated"`
		FirstLogin    bool `json:"first_login"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&loginRes)
	if !loginRes.Authenticated || !loginRes.FirstLogin {
		t.Fatalf("expected authenticated and first_login true, got %+v", loginRes)
	}
	cookies := resp.Cookies()

	// 4. Change password via /admin/api/change-password
	changeBody, _ := json.Marshal(map[string]string{
		"current_password": admin.DefaultPassword,
		"new_password":     "admin-new-pass-999",
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/change-password", bytes.NewReader(changeBody))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{}
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("change password failed: %v (%d)", err, resp.StatusCode)
	}

	// 5. Verify old password no longer works
	resp, err = http.Post(ts.URL+"/admin/api/login", "application/json", bytes.NewReader(loginBody))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password should be rejected after change, got %d", resp.StatusCode)
	}

	// 6. Verify new password logs in and first_login is now false
	newLoginBody, _ := json.Marshal(map[string]string{"password": "admin-new-pass-999"})
	resp, err = http.Post(ts.URL+"/admin/api/login", "application/json", bytes.NewReader(newLoginBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("new password login failed: %v (%d)", err, resp.StatusCode)
	}
	var newLoginRes struct {
		Authenticated bool `json:"authenticated"`
		FirstLogin    bool `json:"first_login"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&newLoginRes)
	if !newLoginRes.Authenticated || newLoginRes.FirstLogin {
		t.Fatalf("expected authenticated and first_login false, got %+v", newLoginRes)
	}
}

func TestAdminDismissFirstLoginFlow(t *testing.T) {
	db := setupTestDBForGateway(t)
	auth, err := admin.NewFromEnvWithDB(db)
	if err != nil {
		t.Fatalf("failed to init auth: %v", err)
	}

	models := scheduler.NewModelRegistry()
	handler := NewHandler(nil, models)
	handler.SetAdminAuth(auth)

	router := NewRouterWithLimits(handler, "", 1024*1024)
	ts := httptest.NewServer(router)
	defer ts.Close()

	// Login with default password
	loginBody, _ := json.Marshal(map[string]string{"password": admin.DefaultPassword})
	resp, err := http.Post(ts.URL+"/admin/api/login", "application/json", bytes.NewReader(loginBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %v", err)
	}
	cookies := resp.Cookies()

	// Dismiss prompt via /admin/api/first-login/dismiss
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/first-login/dismiss", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{}
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("dismiss failed: %v (%d)", err, resp.StatusCode)
	}

	// Next auth check should show first_login = false
	reqCheck, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/auth/check", nil)
	for _, c := range cookies {
		reqCheck.AddCookie(c)
	}
	resp, err = client.Do(reqCheck)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("auth check failed: %v", err)
	}
	var checkRes struct {
		Authenticated bool `json:"authenticated"`
		FirstLogin    bool `json:"first_login"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&checkRes)
	if !checkRes.Authenticated || checkRes.FirstLogin {
		t.Fatalf("expected authenticated=true, first_login=false after dismiss, got %+v", checkRes)
	}
}
