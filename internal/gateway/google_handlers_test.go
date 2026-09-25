package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/admin"
	"sever_provider/internal/identity"
)

func TestPublicGoogleDetailsHideInternalCredentialMetadata(t *testing.T) {
	accounts := []identity.IdentityDetail{{Credentials: []*identity.Credential{{
		ID: "cred", SourceRef: "browser_profile:local", Fingerprint: "local-fingerprint", LastErrorKind: "private-error", Domain: "gemini_web",
	}}}}
	got := publicGoogleDetails(accounts)
	if len(got) != 1 || len(got[0].Credentials) != 1 {
		t.Fatalf("unexpected details: %+v", got)
	}
	credential := got[0].Credentials[0]
	if credential.SourceRef != "" || credential.Fingerprint != "" || credential.LastErrorKind != "" {
		t.Fatal("internal metadata was not filtered")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"browser_profile:local", "local-fingerprint", "private-error"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("internal metadata leaked")
		}
	}
}

type fakeDetailsReader struct{}

func (fakeDetailsReader) List(context.Context, string) ([]identity.Credential, error) {
	return nil, nil
}
func (fakeDetailsReader) ListIdentityDetails(context.Context, string) ([]identity.IdentityDetail, error) {
	return []identity.IdentityDetail{{Credentials: []*identity.Credential{{
		ID: "cred", SourceRef: "browser_profile:local", Fingerprint: "local-fingerprint", LastErrorKind: "private-error", Domain: "gemini_web",
	}}}}, nil
}
func (fakeDetailsReader) GetIdentityDetail(context.Context, string) (identity.IdentityDetail, error) {
	return identity.IdentityDetail{Credentials: []*identity.Credential{{
		ID: "cred", SourceRef: "browser_profile:local", Fingerprint: "local-fingerprint", LastErrorKind: "private-error", Domain: "gemini_web",
	}}}, nil
}

func TestGoogleAccountsHandlerFiltersMetadata(t *testing.T) {
	h := NewHandler(nil, nil)
	auth := admin.NewFromEnv()
	token, err := auth.Login(admin.DefaultPassword)
	if err != nil {
		t.Fatal(err)
	}
	h.SetAdminAuth(auth)
	h.SetIdentityDetails(fakeDetailsReader{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/google/accounts", nil)
	req.AddCookie(&http.Cookie{Name: admin.CookieName, Value: token})
	h.GoogleAccounts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"browser_profile:local", "local-fingerprint", "private-error"} {
		if strings.Contains(body, secret) {
			t.Fatalf("internal metadata %q leaked in response: %s", secret, body)
		}
	}
}
