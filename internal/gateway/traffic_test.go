package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/db"
)

func TestTrafficStoreAndEndpoint(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	t.Setenv("ADMIN_PASSWORD", "traffic-admin-pass")
	auth := admin.NewFromEnv()
	h := NewHandler(nil, nil)
	h.SetAdminAuth(auth)
	h.Traffic = NewTrafficStore(database.DB)
	router := NewRouter(h, "")
	server := httptest.NewServer(router)
	defer server.Close()

	now := time.Now().UTC()
	ctx := context.Background()
	_ = h.Traffic.Record(ctx, now.Add(-30*time.Minute), 50*time.Millisecond)
	_ = h.Traffic.Record(ctx, now.Add(-10*time.Minute), 60*time.Millisecond)
	_ = h.Traffic.Record(ctx, now.Add(-48*time.Hour), 80*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/traffic?period=24h", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	login, err := http.Post(server.URL+"/admin/api/login", "application/json", strings.NewReader(`{"password":"traffic-admin-pass"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer login.Body.Close()
	cookies := login.Cookies()

	doGet := func(url string) *http.Response {
		r, _ := http.NewRequest(http.MethodGet, url, nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := doGet(server.URL + "/admin/api/traffic?period=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var data struct {
		Period   string          `json:"period"`
		Requests int64           `json:"requests"`
		Buckets  []trafficBucket `json:"buckets"`
		From     int64           `json:"from"`
		To       int64           `json:"to"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if data.Period != "24h" || data.Requests != 2 || len(data.Buckets) == 0 {
		t.Fatalf("unexpected 24h traffic: %+v", data)
	}

	resp7d := doGet(server.URL + "/admin/api/traffic?period=7d")
	var data7d struct {
		Period   string          `json:"period"`
		Requests int64           `json:"requests"`
		Buckets  []trafficBucket `json:"buckets"`
	}
	if err := json.NewDecoder(resp7d.Body).Decode(&data7d); err != nil {
		t.Fatal(err)
	}
	_ = resp7d.Body.Close()
	if data7d.Period != "7d" || data7d.Requests != 3 || len(data7d.Buckets) < 2 {
		t.Fatalf("unexpected 7d traffic: %+v", data7d)
	}

	respAll := doGet(server.URL + "/admin/api/traffic?period=all")
	if respAll.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from all, got %d", respAll.StatusCode)
	}
	_ = respAll.Body.Close()

	badResp := doGet(server.URL + "/admin/api/traffic?period=invalid")
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 from invalid, got %d", badResp.StatusCode)
	}
	_ = badResp.Body.Close()

	postReq, _ := http.NewRequest(http.MethodPost, server.URL+"/admin/api/traffic?period=24h", nil)
	for _, c := range cookies {
		postReq.AddCookie(c)
	}
	postResp, _ := server.Client().Do(postReq)
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 from POST, got %d", postResp.StatusCode)
	}
	_ = postResp.Body.Close()
}

func TestIsConsumerTraffic(t *testing.T) {
	cases := map[string]bool{
		"/v1/chat/completions":      true,
		"/v1/audio/speech":          true,
		"/api/tts/generate":         true,
		"/api/llm/jobs":             true,
		"/api/llm/ws/worker":        false,
		"/v1/models":                false,
		"/api/tts/voices":           false,
		"/admin/api/tts/tiktok/acc": false,
		"/admin/api/traffic":        false,
		"/health":                   false,
		"/web/":                     false,
		"/internal/status":          false,
	}
	for path, expected := range cases {
		if got := isConsumerTraffic(path); got != expected {
			t.Errorf("isConsumerTraffic(%q) = %v, expected %v", path, got, expected)
		}
	}
}
