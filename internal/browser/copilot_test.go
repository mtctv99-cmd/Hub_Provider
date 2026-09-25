package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func mockCopilotChathubURL(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"exp":%d,"oid":"11111111-2222-3333-4444-555555555555","tid":"66666666-7777-8888-9999-000000000000","preferred_username":"testuser@example.com"}`,
		exp,
	)))
	return "wss://substrate.office.com/m365Copilot/Chathub/11111111-2222-3333-4444-555555555555@66666666-7777-8888-9999-000000000000?access_token=" + header + "." + payload + ".signature"
}

func TestCopilotManagerStatusInitial(t *testing.T) {
	mgr := NewCopilotManager(t.TempDir(), nil)
	status := mgr.Status()
	if status.Running || status.Captured {
		t.Fatalf("expected idle manager status, got %+v", status)
	}
}

func TestGoogleProfileIDValidation(t *testing.T) {
	for _, id := range []string{"../outside", "..", "has/slash", "", "x" + strings.Repeat("a", 64)} {
		if validateProfileID(id) == nil {
			t.Errorf("accepted invalid profile id %q", id)
		}
	}
	if err := validateProfileID("Account-67881_test"); err != nil {
		t.Fatalf("valid profile id rejected: %v", err)
	}
}

func TestGoogleRefreshRejectsInvalidProfileID(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil)
	for _, id := range []string{"../outside", "..", "has/slash", ""} {
		if _, err := mgr.RefreshAccountFromProfile(id); err == nil {
			t.Errorf("RefreshAccountFromProfile accepted invalid id %q", id)
		}
	}
}

func TestGoogleManagerTargetURLValidation(t *testing.T) {
	mgr := NewManager(t.TempDir(), nil)
	if _, err := mgr.StartAutoLogin("valid-acc", "http://insecure.com"); err == nil {
		t.Fatal("expected error for non-https url")
	}
	if _, err := mgr.StartAutoLogin("valid-acc", "https://evil.com/phishing"); err == nil {
		t.Fatal("expected error for untrusted domain")
	}
	if err := mgr.OpenProfile("valid-acc", "https://evil.com/phishing"); err == nil {
		t.Fatal("expected error for untrusted domain in OpenProfile")
	}
}

func TestCopilotManagerInvalidAccountID(t *testing.T) {
	mgr := NewCopilotManager(t.TempDir(), nil)
	// Invalid characters or directory traversal in account id
	invalidIDs := []string{
		"../../etc/passwd",
		"bad id with spaces",
		"bad/slashes",
		"id*with$special",
	}
	for _, id := range invalidIDs {
		if _, err := mgr.Start(id); err == nil {
			t.Fatalf("expected error for account ID %q", id)
		}
	}
}

func TestCopilotManagerStopIdempotent(t *testing.T) {
	mgr := NewCopilotManager(t.TempDir(), nil)
	// Stop when not running should not panic
	mgr.Stop()
	status := mgr.Status()
	if status.Running {
		t.Fatal("expected status not running after Stop()")
	}
}

func TestWatchCopilotCDP(t *testing.T) {
	upgrader := websocket.Upgrader{}
	validWSURL := mockCopilotChathubURL(time.Now().Add(time.Hour).Unix())

	// One local server hosts both the discovery endpoint and the CDP socket.
	var cdpServer *httptest.Server
	cdpServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/list" {
			wsURL := "ws://" + cdpServer.Listener.Addr().String() + "/devtools/page/tab1"
			tabs := []cdpTab{{ID: "tab1", Type: "page", URL: "https://m365.cloud.microsoft/chat", WebSocketDebuggerURL: wsURL}}
			_ = json.NewEncoder(w).Encode(tabs)
			return
		}
		if r.URL.Path != "/devtools/page/tab1" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, command, _ := conn.ReadMessage()
		var first map[string]any
		_ = json.Unmarshal(command, &first)
		if first["method"] != "Network.enable" {
			t.Errorf("unexpected CDP command: %v", first["method"])
		}
		if err := conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
			t.Error(err)
		}
		if _, command, err := conn.ReadMessage(); err == nil {
			t.Errorf("unexpected side-effecting CDP command: %s", command)
		}
		event := map[string]any{"method": "Network.webSocketCreated", "params": map[string]any{"url": validWSURL}}
		data, _ := json.Marshal(event)
		_ = conn.WriteMessage(websocket.TextMessage, data)

	}))
	defer cdpServer.Close()

	u, _ := url.Parse(cdpServer.URL)
	var portInt int
	fmt.Sscanf(u.Port(), "%d", &portInt)

	token, err := watchCopilotCDP(context.Background(), portInt, time.Second)
	if err != nil {
		t.Fatalf("watchCopilotCDP failed: %v", err)
	}
	if token != validWSURL {
		t.Fatalf("expected token %s, got %s", validWSURL, token)
	}
}
