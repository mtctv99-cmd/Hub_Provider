package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/admin"
	"sever_provider/internal/colabsupervisor"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/workerhub"
)

type mockLocalPiper struct {
	*piper.Adapter
}

func (mockLocalPiper) Name() string { return "piper_local" }
func (mockLocalPiper) Ready() bool  { return true }
func (mockLocalPiper) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilitySpeech}
}
func (mockLocalPiper) Voices() []piper.Voice {
	return []piper.Voice{{ID: "th_TH-mms_female-medium", Name: "th_TH-mms_female-medium"}}
}

func connectMockWorker(t *testing.T, serverURL, token string, reg workerhub.Registration) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(serverURL)
	u.Scheme = "ws"
	u.Path = "/api/llm/ws/worker"
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	conn, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("dial worker failed: %v", err)
	}
	_ = resp.Body.Close()
	if err := conn.WriteJSON(reg); err != nil {
		t.Fatalf("send registration failed: %v", err)
	}
	return conn
}

func TestAdminWorkersAndDisconnect(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "worker-pass")
	auth := admin.NewFromEnv()
	hub := workerhub.New(workerhub.Config{AuthToken: "hub-secret"})
	defer hub.Close()

	registry := scheduler.NewModelRegistry()
	adapters := provider.NewRegistry()
	adapters.Register(mockLocalPiper{Adapter: piper.New("piper", t.TempDir())})
	dispatcher := &scheduler.Dispatcher{Adapters: adapters, Models: registry}

	h := NewHandler(dispatcher, registry)
	h.SetAdminAuth(auth)
	h.SetWorkers(hub)
	h.TikTokAsyncSlots = 8

	tempDir := t.TempDir()
	supervisor := colabsupervisor.New(nil, colabsupervisor.Config{TokenDir: tempDir})
	h.ColabSupervisor = supervisor

	router := NewRouter(h, "")
	server := httptest.NewServer(router)
	defer server.Close()

	login, err := http.Post(server.URL+"/admin/api/login", "application/json", strings.NewReader(`{"password":"worker-pass"}`))
	if err != nil || login.StatusCode != 200 {
		t.Fatalf("login failed: %v", err)
	}
	cookies := login.Cookies()
	_ = login.Body.Close()

	doReq := func(method, path string, body []byte) *http.Response {
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(string(body)))
		for _, c := range cookies {
			req.AddCookie(c)
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	pConn := connectMockWorker(t, server.URL, "hub-secret", workerhub.Registration{
		WorkerID:       "piper-remote-1",
		Models:         []string{"piper:ngochuyennew"},
		Capabilities:   []string{"audio_speech"},
		MaxConcurrency: 2,
	})
	defer pConn.Close()

	cConn := connectMockWorker(t, server.URL, "hub-secret", workerhub.Registration{
		WorkerID:       "colab-worker-1",
		Models:         []string{"gemma-4-colab"},
		MaxConcurrency: 4,
	})
	defer cConn.Close()

	time.Sleep(50 * time.Millisecond)

	resp := doReq("GET", "/admin/api/workers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from GET /admin/api/workers, got %d", resp.StatusCode)
	}
	var data struct {
		PiperLocal struct {
			Ready  bool     `json:"ready"`
			Voices []string `json:"voices"`
		} `json:"piper_local"`
		PiperRemote []workerhub.WorkerSnapshot `json:"piper_remote"`
		ColabWorker []workerhub.WorkerSnapshot `json:"colab_workers"`
		Slots       int                        `json:"tiktok_async_slots"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if !data.PiperLocal.Ready || len(data.PiperLocal.Voices) != 1 || data.PiperLocal.Voices[0] != "th_TH-mms_female-medium" {
		t.Fatalf("piper local unexpected: %+v", data.PiperLocal)
	}
	if len(data.PiperRemote) != 1 || data.PiperRemote[0].WorkerID != "piper-remote-1" {
		t.Fatalf("piper remote unexpected: %+v", data.PiperRemote)
	}
	if len(data.ColabWorker) != 1 || data.ColabWorker[0].WorkerID != "colab-worker-1" {
		t.Fatalf("colab remote unexpected: %+v", data.ColabWorker)
	}
	if data.Slots != 8 {
		t.Fatalf("tiktok slots unexpected: %d", data.Slots)
	}

	dc := doReq("POST", "/admin/api/workers/piper-remote-1/disconnect", nil)
	if dc.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from disconnect, got %d", dc.StatusCode)
	}
	_ = dc.Body.Close()

	dcAgain := doReq("POST", "/admin/api/workers/piper-remote-1/disconnect", nil)
	if dcAgain.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 disconnecting absent worker, got %d", dcAgain.StatusCode)
	}
	_ = dcAgain.Body.Close()

	// Colab supervisor test: eligible account can be started
	_ = os.WriteFile(filepath.Join(tempDir, "token_account1.json"), []byte("{}"), 0644)
	supervisorCommand := "true"
	if runtime.GOOS == "windows" {
		supervisorCommand = "cmd.exe /c exit 0"
	}
	supervisorWithCmd := colabsupervisor.New(nil, colabsupervisor.Config{TokenDir: tempDir, Command: supervisorCommand})
	h.ColabSupervisor = supervisorWithCmd

	startResp := doReq("POST", "/admin/api/workers/colab/start", []byte(`{"account_id":"colab-account1"}`))
	if startResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(startResp.Body)
		t.Fatalf("expected 200 starting eligible account, got %d: %s", startResp.StatusCode, body)
	}
	_ = startResp.Body.Close()

	badAccountResp := doReq("POST", "/admin/api/workers/colab/start", []byte(`{"account_id":"colab-unknown"}`))
	if badAccountResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 starting un-discovered account, got %d", badAccountResp.StatusCode)
	}
	_ = badAccountResp.Body.Close()
}
