package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/provider/copilot"
)

const copilotHome = "https://m365.cloud.microsoft/chat"

var copilotProfileID = regexp.MustCompile(`^[a-zA-Z0-9_.@+\-]{1,64}$`)

func validCopilotProfileID(s string) bool {
	if strings.Contains(s, "..") {
		return false
	}
	return copilotProfileID.MatchString(s)
}

// CopilotCapture is invoked only after a captured Chathub URL passes structural,
// expiry and user checks. Never return the URL to HTTP clients or logs.
type CopilotCapture func(ctx context.Context, accountID, email, tokenURL string, expiresAt time.Time) error

type CopilotStatus struct {
	Running   bool      `json:"running"`
	Captured  bool      `json:"captured"`
	AccountID string    `json:"account_id,omitempty"`
	Email     string    `json:"email,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Message   string    `json:"message"`
}

type CopilotManager struct {
	mu          sync.Mutex
	status      CopilotStatus
	cancel      context.CancelFunc
	profilesDir string
	capture     CopilotCapture
}

func NewCopilotManager(profilesDir string, capture CopilotCapture) *CopilotManager {
	return &CopilotManager{profilesDir: profilesDir, capture: capture}
}
func (m *CopilotManager) Status() CopilotStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}
func (m *CopilotManager) Start(accountID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return "", ErrAutoLoginBusy
	}
	if accountID == "" {
		accountID = "copilot-" + randomProfileID()
	}
	if !validCopilotProfileID(accountID) {
		return "", errors.New("invalid account id")
	}
	browserPath, err := (&Manager{}).FindBrowserPath()
	if err != nil {
		return "", err
	}
	profile, err := filepath.Abs(filepath.Join(m.profilesDir, "copilot", accountID))
	if err != nil {
		return "", errors.New("invalid profile directory")
	}
	if err := os.MkdirAll(profile, 0700); err != nil {
		return "", errors.New("unable to create browser profile")
	}
	port, err := findFreePort()
	if err != nil {
		return "", errors.New("unable to allocate browser port")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command(browserPath, fmt.Sprintf("--remote-debugging-port=%d", port), "--remote-debugging-address=127.0.0.1", "--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check", copilotHome)
	if err := cmd.Start(); err != nil {
		cancel()
		return "", errors.New("unable to start browser")
	}
	m.cancel = cancel
	m.status = CopilotStatus{Running: true, AccountID: accountID, Message: "Browser opened. Sign in to Microsoft manually and start a Copilot chat."}
	go m.monitor(ctx, cmd, port, accountID, profile)
	return accountID, nil
}
func randomProfileID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes[:])
}
func (m *CopilotManager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (m *CopilotManager) setMessage(message string) {
	m.mu.Lock()
	m.status.Message = message
	m.mu.Unlock()
}
func (m *CopilotManager) monitor(ctx context.Context, cmd *exec.Cmd, port int, accountID, profile string) {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() {
		terminateBrowserProcess(cmd)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		cleanSingletonLocks(profile)
		m.mu.Lock()
		m.cancel = nil
		m.status.Running = false
		m.mu.Unlock()
	}()
	deadline := time.NewTimer(10 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.setMessage("Capture stopped.")
			return
		case <-done:
			m.setMessage("Browser closed before capture.")
			return
		case <-deadline.C:
			m.setMessage("Capture timed out.")
			return
		case <-ticker.C:
			token, err := watchCopilotCDP(ctx, port, 4*time.Second)
			if err != nil || token == "" {
				continue
			}
			expiry, email, err := copilot.ValidateCapturedToken(token, 30)
			if err != nil {
				m.setMessage("Waiting for a valid Copilot session with a matching user.")
				continue
			}
			if m.capture == nil {
				m.setMessage("Credential store is unavailable.")
				return
			}
			if err := m.capture(ctx, accountID, email, token, expiry); err != nil {
				m.setMessage("Capture succeeded but could not store the account.")
				continue
			}
			m.mu.Lock()
			m.status.Captured = true
			m.status.Email = email
			m.status.ExpiresAt = expiry
			m.status.Message = "Copilot credential stored; upstream connection is not verified."
			m.mu.Unlock()
			return
		}
	}
}

// watchCopilotCDP observes only websocket creation metadata from local DevTools.
// It does not inspect page input, cookies or passwords.
func watchCopilotCDP(ctx context.Context, port int, wait time.Duration) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tabs []cdpTab
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&tabs); err != nil {
		return "", err
	}
	for _, tab := range tabs {
		page, err := url.Parse(tab.URL)
		if err != nil || tab.Type != "page" || tab.WebSocketDebuggerURL == "" || page.Scheme != "https" || (page.Hostname() != "m365.cloud.microsoft" && page.Hostname() != "copilot.microsoft.com") {
			continue
		}
		endpoint, err := url.Parse(tab.WebSocketDebuggerURL)
		if err != nil || endpoint.Scheme != "ws" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() != fmt.Sprint(port) {
			continue
		}
		dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
		conn, _, err := dialer.DialContext(ctx, endpoint.String(), nil)
		if err != nil {
			continue
		}
		_ = conn.WriteJSON(map[string]any{"id": 1, "method": "Network.enable"})
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var event struct {
				Method string `json:"method"`
				Params struct {
					URL     string `json:"url"`
					Request struct {
						URL string `json:"url"`
					} `json:"request"`
				} `json:"params"`
			}
			if json.Unmarshal(data, &event) == nil {
				candidate := event.Params.URL
				if candidate == "" {
					candidate = event.Params.Request.URL
				}
				if strings.Contains(candidate, "Chathub") && strings.Contains(candidate, "access_token") {
					if _, _, err := copilot.ValidateCapturedToken(candidate, 30); err == nil {
						conn.Close()
						return candidate, nil
					}
				}
			}
		}
		conn.Close()

	}
	return "", nil
}
