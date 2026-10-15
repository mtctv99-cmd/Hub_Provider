package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/db"
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

func TestEmbeddedDashboardIgnoresStaleExternalWebDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "index.html"), []byte("STALE_EXTERNAL_DASHBOARD"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	h := NewHandler(nil, nil)
	router := NewRouter(h, "")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/web/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Sever Provider") || strings.Contains(w.Body.String(), "STALE_EXTERNAL_DASHBOARD") {
		t.Fatalf("expected dashboard embedded in the deployed binary, status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDashboardStatusUsesPersistedTrafficSummary(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "dashboard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "local", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat}})
	h := NewHandler(nil, models)
	h.Traffic = NewTrafficStore(database.DB)
	now := time.Now().UTC()
	if err := h.Traffic.Record(context.Background(), now, 125*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := h.Traffic.RecordMetrics(context.Background(), now, 0, 42, 0); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.DashboardStatus(w, httptest.NewRequest(http.MethodGet, "/internal/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var status map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["metrics_source"] != "persistent" || status["requests_total"] != float64(1) || status["ai_tokens_total"] != float64(42) || status["total_duration_ms"] != float64(125) {
		t.Fatalf("dashboard did not return persistent metrics: %+v", status)
	}
}
