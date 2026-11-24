package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/admin"
	"sever_provider/internal/tenant"
)

func TestMiddlewarePropagatesTenantIdentity(t *testing.T) {
	db := setupTestDBForGateway(t)
	keys := admin.NewAPIKeys(db)
	meta, plain, err := keys.Create("tenant-isolation-test")
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{APIKeys: keys}
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = tenant.ID(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	router := withMiddleware(next, h, "legacy-static-key", keys, 1024)

	serve := func(method, path, key string) *httptest.ResponseRecorder {
		t.Helper()
		seen = ""
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := serve(http.MethodPost, "/v1/chat/completions", plain); rec.Code != http.StatusOK {
		t.Fatalf("db key status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seen != meta.ID {
		t.Fatalf("db key tenant=%q, want %q", seen, meta.ID)
	}

	if rec := serve(http.MethodPost, "/v1/chat/completions", "legacy-static-key"); rec.Code != http.StatusOK {
		t.Fatalf("legacy key status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seen != legacyTenantID {
		t.Fatalf("legacy key tenant=%q, want %q", seen, legacyTenantID)
	}

	if rec := serve(http.MethodPost, "/v1/chat/completions", "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key status=%d, want 401", rec.Code)
	}
	if seen != "" {
		t.Fatalf("invalid key must not reach handler, tenant=%q", seen)
	}

	if rec := serve(http.MethodGet, "/v1/models", ""); rec.Code != http.StatusOK {
		t.Fatalf("anonymous models status=%d", rec.Code)
	}
	if seen != "" {
		t.Fatalf("anonymous request must not have a tenant, got %q", seen)
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("abc", "abc") {
		t.Fatal("equal strings must compare equal")
	}
	if constantTimeEqual("abc", "abd") || constantTimeEqual("abc", "abcd") || constantTimeEqual("", "x") {
		t.Fatal("different strings must not compare equal")
	}
}
