package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

func TestEmbeddedDashboard(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "local", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat}})
	r := NewRouter(NewHandler(nil, models), "")
	request := httptest.NewRequest(http.MethodGet, "/web/", nil)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "Sever Provider") {
		t.Fatal("dashboard title missing")
	}
	if !strings.Contains(response.Body.String(), "id=\"add-copilot-modal\"") {
		t.Fatal("copilot modal missing from embedded HTML")
	}
	if !strings.Contains(response.Body.String(), "id=\"copilot-account-list\"") {
		t.Fatal("copilot account list container missing from embedded HTML")
	}
	for _, period := range []string{"24h", "7d", "1m", "all"} {
		if !strings.Contains(response.Body.String(), "data-traffic-period=\""+period+"\"") {
			t.Fatalf("traffic period %s missing from embedded HTML", period)
		}
	}
	if strings.Contains(response.Body.String(), "Live API Traffic") {
		t.Fatal("obsolete live traffic label found")
	}
	if !strings.Contains(response.Body.String(), "id=\"worker-management-title\"") || !strings.Contains(response.Body.String(), "id=\"worker-piper-remote\"") || !strings.Contains(response.Body.String(), "id=\"worker-colab\"") {
		t.Fatal("worker management controls missing from Settings")
	}
	for _, id := range []string{"status-google-requests", "status-tiktok-requests", "status-colab-requests", "status-copilot-requests"} {
		if !strings.Contains(response.Body.String(), "id=\""+id+"\"") {
			t.Fatalf("provider request counter %s missing from embedded HTML", id)
		}
	}
}
