package browser

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	ErrBrowserNotFound  = errors.New("no compatible browser executable found (install Chrome or Edge)")
	ErrAutoLoginBusy    = errors.New("browser login session is already running")
	ErrInvalidAccountID = errors.New("invalid account id")
)

var profileIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// AccountIDForEmail derives a deterministic profile ID from the normalized email,
// avoiding the random `time.Now().Unix()%100000` profile churn that orphaned
// existing profile dirs on every second login.

func validateProfileID(accountID string) error {
	if !profileIDPattern.MatchString(accountID) {
		return ErrInvalidAccountID
	}
	return nil
}

func SafeProfileNameForEmail(email string) string {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return ""
	}
	safe := strings.ReplaceAll(normalized, "@", "_at_")
	safe = strings.ReplaceAll(safe, ".", "_")
	return "google_" + safe
}

func AccountIDForEmail(email string) string {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	id := make([]byte, 0, 8)
	for _, b := range sum[:6] {
		id = append(id, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"[int(b)%36])
	}
	return "Account-" + string(id)
}

func (m *Manager) profileDir(accountID string) (string, error) {
	if err := validateProfileID(accountID); err != nil {
		return "", err
	}
	root, err := filepath.Abs(m.profilesDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, accountID), nil
}

type CaptureCallback func(accountID, email, cookieStr, profileDir string) error

type SessionStatus struct {
	Running   bool   `json:"running"`
	AccountID string `json:"account_id"`
	Captured  bool   `json:"captured"`
	Email     string `json:"email"`
	Message   string `json:"message"`
}

type Manager struct {
	mu           sync.Mutex
	running      atomic.Bool
	curStatus    SessionStatus
	profilesDir  string
	onCaptured   CaptureCallback
	activeCancel context.CancelFunc
}

func NewManager(profilesDir string, onCaptured CaptureCallback) *Manager {
	if profilesDir == "" {
		profilesDir = "data/profiles"
	}
	_ = os.MkdirAll(profilesDir, 0755)
	return &Manager{
		profilesDir: profilesDir,
		onCaptured:  onCaptured,
	}
}

func (m *Manager) Status() SessionStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.curStatus
}

func (m *Manager) FindBrowserPath() (string, error) {
	candidates := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`,
		`/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge`,
		`/usr/bin/google-chrome`,
		`/usr/bin/microsoft-edge`,
		`/usr/bin/chromium`,
		`/usr/bin/chromium-browser`,
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	names := []string{"google-chrome", "chrome", "msedge", "edge", "chromium"}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", ErrBrowserNotFound
}

func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (m *Manager) StartAutoLogin(accountID, targetURL string) (string, error) {
	if !m.running.CompareAndSwap(false, true) {
		return "", ErrAutoLoginBusy
	}

	browserPath, err := m.FindBrowserPath()
	if err != nil {
		m.running.Store(false)
		return "", err
	}

	if strings.Contains(accountID, "@") {
		safe := SafeProfileNameForEmail(accountID)
		if dir, err := m.profileDir(safe); err == nil {
			if _, statErr := os.Stat(dir); statErr == nil {
				accountID = safe
			} else {
				accountID = AccountIDForEmail(accountID)
			}
		} else {
			accountID = AccountIDForEmail(accountID)
		}
	}
	if accountID == "" {
		accountID = fmt.Sprintf("Account-%05d", time.Now().Unix()%100000)
	}
	if err := validateProfileID(accountID); err != nil {
		m.running.Store(false)
		return "", err
	}
	if targetURL == "" {
		targetURL = "https://gemini.google.com/app"
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil || parsedURL.Scheme != "https" || (parsedURL.Hostname() != "gemini.google.com" && parsedURL.Hostname() != "aistudio.google.com" && parsedURL.Hostname() != "accounts.google.com") {
		m.running.Store(false)
		return "", errors.New("unsupported Google login URL")
	}

	profileDir, err := m.profileDir(accountID)
	if err != nil {
		m.running.Store(false)
		return "", err
	}
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		m.running.Store(false)
		return "", fmt.Errorf("create browser profile: %w", err)
	}

	cdpPort, err := findFreePort()
	if err != nil {
		m.running.Store(false)
		return "", fmt.Errorf("find free cdp port: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.activeCancel = cancel
	m.curStatus = SessionStatus{
		Running:   true,
		AccountID: accountID,
		Captured:  false,
		Message:   "Đang mở trình duyệt Google... Hãy đăng nhập tài khoản trên cửa sổ vừa mở.",
	}
	m.mu.Unlock()

	cmd := exec.Command(browserPath,
		fmt.Sprintf("--remote-debugging-port=%d", cdpPort),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-blink-features=AutomationControlled",
		targetURL,
	)

	if err := cmd.Start(); err != nil {
		m.running.Store(false)
		cancel()
		return "", fmt.Errorf("start browser: %w", err)
	}

	go m.monitorLogin(ctx, cmd, cdpPort, accountID, profileDir)

	return accountID, nil
}

func (m *Manager) OpenProfile(accountID, targetURL string) error {
	browserPath, err := m.FindBrowserPath()
	if err != nil {
		return err
	}
	if accountID == "" {
		return errors.New("account id is required")
	}
	if targetURL == "" {
		targetURL = "https://gemini.google.com/app"
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil || parsedURL.Scheme != "https" || (parsedURL.Hostname() != "gemini.google.com" && parsedURL.Hostname() != "aistudio.google.com" && parsedURL.Hostname() != "accounts.google.com") {
		return errors.New("unsupported Google login URL")
	}
	profileDir, err := m.profileDir(accountID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		return fmt.Errorf("create browser profile: %w", err)
	}

	cmd := exec.Command(browserPath,
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-blink-features=AutomationControlled",
		targetURL,
	)
	return cmd.Start()
}

func (m *Manager) monitorLogin(ctx context.Context, cmd *exec.Cmd, cdpPort int, accountID, profileDir string) {
	defer func() {
		m.running.Store(false)
		m.mu.Lock()
		m.curStatus.Running = false
		m.mu.Unlock()
		cleanSingletonLocks(profileDir)
	}()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Wait for browser process exit in background
	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	var captured bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-cmdDone:
			return
		case <-ticker.C:
			if captured {
				continue
			}
			cookieStr, email, ok := captureCookiesViaCDP(cdpPort, profileDir)
			if ok && cookieStr != "" {
				if email == "" {

					m.mu.Lock()
					m.curStatus.Message = "Đã thấy phiên Google nhưng đang chờ trang hiển thị Gmail thật..."
					m.mu.Unlock()
					continue
				}
				captured = true
				m.mu.Lock()
				m.curStatus.Captured = true
				m.curStatus.Email = email
				m.curStatus.Message = fmt.Sprintf("Đã tự động bắt cookie cho %s thành công!", email)
				m.mu.Unlock()

				if m.onCaptured != nil {
					if err := m.onCaptured(accountID, email, cookieStr, profileDir); err != nil {
						log.Printf("[AutoLogin] callback error for %s: %v", accountID, err)
						m.mu.Lock()
						m.curStatus.Captured = false
						m.curStatus.Message = "Bắt được cookie nhưng lưu tài khoản thất bại."
						m.mu.Unlock()
						continue
					}
				}
				gracefulBrowserClose(cmd, cdpPort)
				terminateBrowserProcess(cmd)
				return
			}
		}
	}
}

type cdpTab struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func profileEmail(profileDir string) string {
	path := filepath.Join(profileDir, "Default", "Preferences")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var prefs struct {
		AccountInfo []struct {
			Email string `json:"email"`
		} `json:"account_info"`
	}
	if json.Unmarshal(data, &prefs) != nil {
		return ""
	}
	for _, account := range prefs.AccountInfo {
		candidate := strings.TrimSpace(account.Email)
		if emailPattern.MatchString(candidate) {
			return emailPattern.FindString(candidate)
		}
	}
	return ""
}

func captureCookiesViaCDP(cdpPort int, profileDir string) (cookieStr string, email string, ok bool) {
	tabsURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", cdpPort)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(tabsURL)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()

	var tabs []cdpTab
	if err := json.NewDecoder(resp.Body).Decode(&tabs); err != nil || len(tabs) == 0 {
		return "", "", false
	}

	var targetWS string
	for _, tab := range tabs {
		page, parseErr := url.Parse(tab.URL)
		endpoint, endpointErr := url.Parse(tab.WebSocketDebuggerURL)
		if tab.Type == "page" && parseErr == nil && page.Scheme == "https" &&
			(page.Hostname() == "gemini.google.com" || page.Hostname() == "aistudio.google.com" || page.Hostname() == "accounts.google.com") &&
			endpointErr == nil && endpoint.Scheme == "ws" && endpoint.Hostname() == "127.0.0.1" && endpoint.Port() == strconv.Itoa(cdpPort) {
			targetWS = endpoint.String()
			break
		}
	}
	if targetWS == "" {
		return "", "", false
	}

	conn, _, err := websocket.DefaultDialer.Dial(targetWS, nil)
	if err != nil {
		return "", "", false
	}
	defer conn.Close()

	// 1. Gửi Network.getCookies
	reqID := 101
	req := map[string]any{
		"id":     reqID,
		"method": "Network.getCookies",
		"params": map[string]any{
			"urls": []string{
				"https://gemini.google.com",
				"https://aistudio.google.com",
				"https://google.com",
			},
		},
	}
	if err := conn.WriteJSON(req); err != nil {
		return "", "", false
	}

	var rawCookie string
	var hasSapisid, hasPsid bool
	deadline := time.Now().Add(3 * time.Second)
	_ = conn.SetReadDeadline(deadline)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var res struct {
			ID     int `json:"id"`
			Result struct {
				Cookies []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"cookies"`
			} `json:"result"`
		}
		if json.Unmarshal(msg, &res) == nil && res.ID == reqID {
			var parts []string
			for _, c := range res.Result.Cookies {
				parts = append(parts, fmt.Sprintf("%s=%s", c.Name, c.Value))
				if c.Name == "SAPISID" {
					hasSapisid = true
				}
				if c.Name == "__Secure-1PSID" {
					hasPsid = true
				}
			}
			rawCookie = strings.Join(parts, "; ")
			break
		}
	}

	if !hasSapisid || !hasPsid {
		return "", "", false
	}

	// 2. Thử trích xuất email qua Runtime.evaluate
	evalID := 102
	evalReq := map[string]any{
		"id":     evalID,
		"method": "Runtime.evaluate",
		"params": map[string]any{
			"expression":    `document.title || ""`,
			"returnByValue": true,
		},
	}
	_ = conn.WriteJSON(evalReq)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var evalRes struct {
			ID     int `json:"id"`
			Result struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			} `json:"result"`
		}
		if json.Unmarshal(msg, &evalRes) == nil && evalRes.ID == evalID {
			email = strings.TrimSpace(evalRes.Result.Result.Value)
			if !emailPattern.MatchString(email) {
				email = ""
			} else {
				email = emailPattern.FindString(email)
			}
			break
		}

	}

	if profileCandidate := profileEmail(profileDir); profileCandidate != "" {
		email = profileCandidate
	} else {
		email = ""
	}
	return rawCookie, email, true
}

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// gracefulBrowserClose asks Chrome/Edge to persist its profile (cookies, Preferences)
// via Browser.close before falling back to kill. CDP endpoint is restricted to
// the local debugging port of this login session.
func gracefulBrowserClose(cmd *exec.Cmd, cdpPort int) bool {
	if cdpPort <= 0 || cdpPort > 65535 {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", cdpPort))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var version struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return false
	}
	endpoint, err := url.Parse(version.URL)
	if err != nil || endpoint.Scheme != "ws" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() != strconv.Itoa(cdpPort) {
		return false
	}
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.Dial(version.URL, nil)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := conn.WriteJSON(map[string]any{"id": 401, "method": "Browser.close"}); err != nil {
		return false
	}
	if cmd == nil {
		return true
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case <-wait:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

func terminateBrowserProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
		return
	}
	_ = cmd.Process.Kill()
}

func cleanSingletonLocks(profileDir string) {
	for _, lockName := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		_ = os.Remove(filepath.Join(profileDir, lockName))
	}
}
