package browser

import (
	"context"
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
)

const (
	DefaultMetaHome = "https://muse.ai/"
	AltMetaHome     = "https://www.meta.ai/"
)

var metaProfileID = regexp.MustCompile(`^[a-zA-Z0-9_.@+\-]{1,64}$`)

func validMetaProfileID(s string) bool {
	if strings.Contains(s, "..") {
		return false
	}
	return metaProfileID.MatchString(s)
}

// MetaCapture is invoked when a valid Meta / muse.ai session is captured.
type MetaCapture func(ctx context.Context, accountID, email, cookieStr, tokenStr string) error

// MetaStatus reflects the status of the Meta browser login process.
type MetaStatus struct {
	Running   bool   `json:"running"`
	Captured  bool   `json:"captured"`
	AccountID string `json:"account_id,omitempty"`
	Email     string `json:"email,omitempty"`
	Message   string `json:"message"`
}

// MetaManager manages the browser lifecycle for Meta / muse.ai auto-login.
type MetaManager struct {
	mu          sync.Mutex
	status      MetaStatus
	cancel      context.CancelFunc
	profilesDir string
	capture     MetaCapture
}

func NewMetaManager(profilesDir string, capture MetaCapture) *MetaManager {
	if profilesDir == "" {
		profilesDir = "data/profiles"
	}
	return &MetaManager{profilesDir: profilesDir, capture: capture}
}

func (m *MetaManager) Status() MetaStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *MetaManager) Start(accountID string) (string, error) {
	return m.StartWithURL(accountID, DefaultMetaHome)
}

func (m *MetaManager) StartWithURL(accountID, targetURL string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return "", ErrAutoLoginBusy
	}
	if accountID == "" {
		accountID = "meta-" + randomProfileID()
	}
	if !validMetaProfileID(accountID) {
		return "", errors.New("invalid account id")
	}
	if targetURL == "" {
		targetURL = DefaultMetaHome
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil || parsedURL.Scheme != "https" ||
		(!strings.Contains(parsedURL.Hostname(), "muse.ai") &&
			!strings.Contains(parsedURL.Hostname(), "meta.ai") &&
			!strings.Contains(parsedURL.Hostname(), "facebook.com")) {
		return "", errors.New("unsupported Meta login URL")
	}

	browserPath, err := (&Manager{}).FindBrowserPath()
	if err != nil {
		return "", err
	}
	profile, err := filepath.Abs(filepath.Join(m.profilesDir, "meta", accountID))
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
	cmd := exec.Command(browserPath,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-blink-features=AutomationControlled",
		targetURL,
	)
	cmd.Env = os.Environ()
	hasDisplay := false
	for _, env := range cmd.Env {
		if strings.HasPrefix(env, "DISPLAY=") {
			hasDisplay = true
			break
		}
	}
	if !hasDisplay {
		cmd.Env = append(cmd.Env, "DISPLAY=:1")
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return "", errors.New("unable to start browser")
	}

	m.cancel = cancel
	m.status = MetaStatus{
		Running:   true,
		AccountID: accountID,
		Message:   "Trình duyệt đã mở. Hãy đăng nhập tài khoản Meta (muse.ai) trên cửa sổ vừa mở.",
	}
	go m.monitor(ctx, cmd, port, accountID, profile)
	return accountID, nil
}

func (m *MetaManager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *MetaManager) setMessage(message string) {
	m.mu.Lock()
	m.status.Message = message
	m.mu.Unlock()
}

func (m *MetaManager) monitor(ctx context.Context, cmd *exec.Cmd, port int, accountID, profile string) {
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
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.setMessage("Đã dừng quá trình đăng nhập.")
			return
		case <-done:
			m.setMessage("Trình duyệt đã đóng trước khi hoàn tất đăng nhập.")
			return
		case <-deadline.C:
			m.setMessage("Quá thời gian chờ đăng nhập (10 phút).")
			return
		case <-ticker.C:
			cookieStr, tokenStr, email, ok := watchMetaCDP(ctx, port, 3*time.Second)
			if !ok || (cookieStr == "" && tokenStr == "") {
				continue
			}

			if m.capture == nil {
				m.setMessage("Lỗi: Credential store không khả dụng.")
				return
			}

			if err := m.capture(ctx, accountID, email, cookieStr, tokenStr); err != nil {
				m.setMessage("Bắt được phiên nhưng lưu tài khoản thất bại: " + err.Error())
				continue
			}

			m.mu.Lock()
			m.status.Captured = true
			m.status.Email = email
			m.status.Message = fmt.Sprintf("Đã bắt và lưu tài khoản Meta (%s) thành công!", email)
			m.mu.Unlock()
			return
		}
	}
}

// watchMetaCDP inspects DevTools via CDP to capture cookies, tokens, and identity info.
func watchMetaCDP(ctx context.Context, port int, wait time.Duration) (cookieStr, tokenStr, email string, ok bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return "", "", "", false
	}
	defer resp.Body.Close()

	var tabs []cdpTab
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&tabs); err != nil {
		return "", "", "", false
	}

	for _, tab := range tabs {
		page, err := url.Parse(tab.URL)
		if err != nil || tab.Type != "page" || tab.WebSocketDebuggerURL == "" || page.Scheme != "https" {
			continue
		}
		host := page.Hostname()
		if !strings.Contains(host, "muse.ai") && !strings.Contains(host, "meta.ai") && !strings.Contains(host, "facebook.com") {
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

		// Enable Network & Runtime
		_ = conn.WriteJSON(map[string]any{"id": 1, "method": "Network.enable"})
		_ = conn.WriteJSON(map[string]any{"id": 2, "method": "Runtime.enable"})

		// 1. Get cookies across all relevant domains
		cookieReq := map[string]any{
			"id":     10,
			"method": "Network.getCookies",
			"params": map[string]any{
				"urls": []string{
					"https://muse.ai",
					"https://auth.muse.ai",
					"https://www.meta.ai",
					"https://www.facebook.com",
				},
			},
		}
		_ = conn.WriteJSON(cookieReq)

		// 2. Evaluate localStorage
		evalReq := map[string]any{
			"id":     20,
			"method": "Runtime.evaluate",
			"params": map[string]any{
				"expression":    `localStorage.getItem('token') || localStorage.getItem('access_token') || localStorage.getItem('userToken') || ''`,
				"returnByValue": true,
			},
		}
		_ = conn.WriteJSON(evalReq)

		// 3. Evaluate title/email candidate
		titleReq := map[string]any{
			"id":     30,
			"method": "Runtime.evaluate",
			"params": map[string]any{
				"expression":    `document.title || ''`,
				"returnByValue": true,
			},
		}
		_ = conn.WriteJSON(titleReq)

		_ = conn.SetReadDeadline(time.Now().Add(wait))

		var foundCookies []string
		var hasCUser, hasDatr, hasAbraSess, hasXs bool
		var cUserVal string

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				break
			}

			// Handle cookie response
			var cookieResp struct {
				ID     int `json:"id"`
				Result struct {
					Cookies []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"cookies"`
				} `json:"result"`
			}
				if json.Unmarshal(data, &cookieResp) == nil && cookieResp.ID == 10 {
					for _, c := range cookieResp.Result.Cookies {
						foundCookies = append(foundCookies, fmt.Sprintf("%s=%s", c.Name, c.Value))
						switch strings.ToLower(c.Name) {
						case "c_user":
							hasCUser = true
							cUserVal = c.Value
						case "datr":
							hasDatr = true
						case "abra_sess", "meta_ai_session":
							hasAbraSess = true
						case "xs", "fr":
							hasXs = true
						}
					}
				}

			// Handle localStorage eval
			var evalResp struct {
				ID     int `json:"id"`
				Result struct {
					Result struct {
						Value any `json:"value"`
					} `json:"result"`
				} `json:"result"`
			}
			if json.Unmarshal(data, &evalResp) == nil && evalResp.ID == 20 {
				if val, ok := evalResp.Result.Result.Value.(string); ok && val != "" {
					tokenStr = strings.TrimSpace(val)
				}
			}

			// Handle title eval
			if json.Unmarshal(data, &evalResp) == nil && evalResp.ID == 30 {
				if val, ok := evalResp.Result.Result.Value.(string); ok && val != "" {
					if emailPattern.MatchString(val) {
						email = emailPattern.FindString(val)
					}
				}
			}

			// Handle Network.requestWillBeSent for Bearer tokens
			var event struct {
				Method string `json:"method"`
				Params struct {
					Request struct {
						Headers map[string]string `json:"headers"`
					} `json:"request"`
				} `json:"params"`
			}
			if json.Unmarshal(data, &event) == nil && event.Method == "Network.requestWillBeSent" {
				hdr := event.Params.Request.Headers["authorization"]
				if hdr == "" {
					hdr = event.Params.Request.Headers["Authorization"]
				}
				if strings.HasPrefix(hdr, "Bearer ") {
					cand := strings.TrimSpace(strings.TrimPrefix(hdr, "Bearer "))
					if len(cand) > 20 {
						tokenStr = cand
					}
				}
			}
		}
		conn.Close()

		// Determine if session is captured
		if len(foundCookies) > 0 {
			cookieStr = strings.Join(foundCookies, "; ")
		}

		// A valid Meta / muse.ai session exists if:
		// 1) c_user cookie exists (logged in Meta/Facebook account)
		// 2) abra_sess or meta_ai_session cookie exists
		// 3) xs + datr pair exists (Facebook session cookies)
		// 4) a Bearer token or access_token was extracted
		isAuth := hasCUser || (hasDatr && hasAbraSess) || (hasDatr && hasXs) || len(tokenStr) > 20

		if isAuth {
			if email == "" {
				if tokenStr != "" {
					email = extractEmailFromToken(tokenStr)
				}
				if email == "" && cUserVal != "" {
					email = "meta_user_" + cUserVal + "@meta.ai"
				}
				if email == "" {
					email = "meta_user_" + randomProfileID()[:6] + "@meta.ai"
				}
			}
			return cookieStr, tokenStr, email, true
		}
	}
	return "", "", "", false
}
