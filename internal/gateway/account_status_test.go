package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"sever_provider/internal/identity"
	"sever_provider/internal/scheduler"
)

type detailReader struct{}

func (detailReader) List(context.Context, string) ([]identity.Credential, error) { return nil, nil }
func (detailReader) ListIdentityDetails(context.Context, string) ([]identity.IdentityDetail, error) {
	return []identity.IdentityDetail{{Identity: identity.Identity{ID: "g1", Email: "user@example.com", ProviderGroup: "google", GlobalStatus: identity.IdentityPartial}, Credentials: []*identity.Credential{{Domain: "gemini_web", Status: identity.StatusHealthy}, {Domain: "google_colab", Status: identity.StatusAuthRequired}}}}, nil
}
func (detailReader) GetIdentityDetail(context.Context, string) (identity.IdentityDetail, error) {
	return identity.IdentityDetail{}, nil
}

func TestDashboardStatusIncludesCanonicalAccounts(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "m", Provider: "mock"})
	h := NewHandler(nil, models)
	h.SetCredentials(detailReader{})
	h.SetIdentityDetails(detailReader{})
	r := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/internal/status", nil)
	h.DashboardStatus(r, req)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d", r.Code)
	}
	if r.Body.Len() == 0 {
		t.Fatal("empty status response")
	}
}
