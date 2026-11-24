package gateway

import (
	"bytes"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"sever_provider/internal/admin"
	"sever_provider/internal/scheduler"
)

func TestAdminLoginIsRateLimitedAfterRepeatedFailures(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "gateway-login-limit-pass")
	db := setupTestDBForGateway(t)
	auth, err := admin.NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, scheduler.NewModelRegistry())
	h.SetAdminAuth(auth)
	srv := httptest.NewServer(NewRouter(h, ""))
	defer srv.Close()

	failBody := []byte(`{"password":"wrong-password"}`)
	for i := 0; i < admin.LoginMaxFailures; i++ {
		resp, err := http.Post(srv.URL+"/admin/api/login", "application/json", bytes.NewReader(failBody))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d, want 401", i+1, resp.StatusCode)
		}
	}

	resp, err := http.Post(srv.URL+"/admin/api/login", "application/json", bytes.NewReader(failBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 after %d failures", resp.StatusCode, admin.LoginMaxFailures+1)
	}
	retryAfter, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || retryAfter <= 0 {
		t.Fatalf("Retry-After=%q is not a positive integer", resp.Header.Get("Retry-After"))
	}

	// Even the correct password must be rejected while the lockout is active.
	okBody := []byte(`{"password":"gateway-login-limit-pass"}`)
	resp, err = http.Post(srv.URL+"/admin/api/login", "application/json", bytes.NewReader(okBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("correct password during lockout status=%d, want 429", resp.StatusCode)
	}
}

func TestAdminCookieSecureHonoursTrustedForwardedProto(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/admin/api/login", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-Proto", "https")
	if !adminCookieSecure(req) {
		t.Fatal("loopback proxy with X-Forwarded-Proto=https must set Secure")
	}

	req.RemoteAddr = "203.0.113.9:12345"
	if adminCookieSecure(req) {
		t.Fatal("untrusted remote peer must not be allowed to force Secure via header")
	}

	req.RemoteAddr = "203.0.113.9:12345"
	req.TLS = &tls.ConnectionState{}
	if !adminCookieSecure(req) {
		t.Fatal("direct TLS must set Secure")
	}

	req = httptest.NewRequest(http.MethodGet, "http://example.test/admin/api/login", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	req.Header.Set("X-Forwarded-Proto", "http")
	if adminCookieSecure(req) {
		t.Fatal("plain HTTP behind proxy must not set Secure")
	}
}
