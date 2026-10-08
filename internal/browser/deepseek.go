package browser

import (
	"context"
	"encoding/base64"
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

const deepseekHome = "https://chat.deepseek.com/sign_in"

var deepseekProfileID = regexp.MustCompile(`^[a-zA-Z0-9_.@+\-]{1,64}$`)

func validDeepSeekProfileID(s string) bool {
	if strings.Contains(s, "..") {
		return false
	}
	return deepseekProfileID.MatchString(s)
}

// DeepSeekCapture được gọi khi bắt thành công userToken hợp lệ từ DeepSeek Web.
type DeepSeekCapture func(ctx context.Context, accountID, email, token string) error

// DeepSeekStatus phản ánh trạng thái tiến trình đăng nhập browser DeepSeek.
type DeepSeekStatus struct {
	Running   bool   `json:"running"`
	Captured  bool   `json:"captured"`
	AccountID string `json:"account_id,omitempty"`
	Email     string `json:"email,omitempty"`
	Message   string `json:"message"`
}

// DeepSeekManager quản lý vòng đời trình duyệt tự động đăng nhập DeepSeek.
type DeepSeekManager struct {
	mu          sync.Mutex
	status      DeepSeekStatus
	cancel      context.CancelFunc
	profilesDir string
	capture     DeepSeekCapture
}

// NewDeepSeekManager khởi tạo DeepSeekManager mới.
func NewDeepSeekManager(profilesDir string, capture DeepSeekCapture) *DeepSeekManager {
	if profilesDir == "" {
		profilesDir = "data/profiles"
	}
	return &DeepSeekManager{profilesDir: profilesDir, capture: capture}
}

// Status trả về trạng thái hiện tại của quá trình đăng nhập.
func (m *DeepSeekManager) Status() DeepSeekStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Start khởi chạy trình duyệt mở trang đăng nhập DeepSeek Web.
func (m *DeepSeekManager) Start(accountID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return "", ErrAutoLoginBusy
	}
	if accountID == "" {
		accountID = "deepseek-" + randomProfileID()
	}
	if !validDeepSeekProfileID(accountID) {
		return "", errors.New("invalid account id")
	}
	browserPath, err := (&Manager{}).FindBrowserPath()
	if err != nil {
		return "", err
	}
	profile, err := filepath.Abs(filepath.Join(m.profilesDir, "deepseek", accountID))
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
		deepseekHome,
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
	m.status = DeepSeekStatus{
		Running:   true,
		AccountID: accountID,
		Message:   "Trình duyệt đã mở. Hãy đăng nhập tài khoản DeepSeek trên cửa sổ trình duyệt.",
	}
	go m.monitor(ctx, cmd, port, accountID, profile)
	return accountID, nil
}

// Stop dừng phiên đăng nhập trình duyệt.
func (m *DeepSeekManager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *DeepSeekManager) setMessage(message string) {
	m.mu.Lock()
	m.status.Message = message
	m.mu.Unlock()
}

func (m *DeepSeekManager) monitor(ctx context.Context, cmd *exec.Cmd, port int, accountID, profile string) {
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
			candidate, err := watchDeepSeekCDP(ctx, port, 3*time.Second)
			if err != nil || candidate == "" {
				continue
			}

			// Chỉ chấp nhận token khi đã đăng nhập thật (gọi thành công API users/current)
			email, ok := validateDeepSeekToken(candidate)
			if !ok {
				m.setMessage("Đang chờ bạn hoàn tất đăng nhập trên trình duyệt DeepSeek...")
				continue
			}

			if m.capture == nil {
				m.setMessage("Lỗi: Credential store không khả dụng.")
				return
			}

			if err := m.capture(ctx, accountID, email, candidate); err != nil {
				m.setMessage("Bắt được token nhưng lưu tài khoản thất bại: " + err.Error())
				continue
			}

			m.mu.Lock()
			m.status.Captured = true
			m.status.Email = email
			m.status.Message = fmt.Sprintf("Đã bắt và lưu tài khoản DeepSeek (%s) thành công!", email)
			m.mu.Unlock()
			return
		}
	}
}

// validateDeepSeekToken gửi request kiểm tra token thật với máy chủ DeepSeek.
// Chỉ khi mã phản hồi code == 0 mới được coi là đã đăng nhập thành công.
func validateDeepSeekToken(token string) (string, bool) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) < 20 {
		return "", false
	}

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "https://chat.deepseek.com/api/v0/users/current", nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://chat.deepseek.com")
	req.Header.Set("Referer", "https://chat.deepseek.com/")

	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	var res struct {
		Code int `json:"code"`
		Data struct {
			BizCode int `json:"biz_code"`
			BizData struct {
				ID           string `json:"id"`
				Email        string `json:"email"`
				Name         string `json:"name"`
				MobileNumber string `json:"mobile_number"`
				User         struct {
					Email  string `json:"email"`
					Name   string `json:"name"`
					Mobile string `json:"mobile"`
				} `json:"user"`
			} `json:"biz_data"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"data"`
	}

	if json.NewDecoder(resp.Body).Decode(&res) != nil {
		return "", false
	}

	if res.Code != 0 || res.Data.BizCode != 0 {
		return "", false
	}

	email := res.Data.BizData.Email
	if email == "" {
		email = res.Data.BizData.User.Email
	}
	if email == "" {
		email = res.Data.Email
	}
	if email == "" {
		email = res.Data.BizData.Name
	}
	if email == "" {
		email = res.Data.BizData.User.Name
	}
	if email == "" {
		email = res.Data.BizData.MobileNumber
	}
	if email == "" {
		email = res.Data.BizData.ID
	}
	if email == "" {
		email = "deepseek_user"
	}
	return email, true
}

// watchDeepSeekCDP quét qua DevTools protocol để lấy token từ Network request hoặc localStorage.
func watchDeepSeekCDP(ctx context.Context, port int, wait time.Duration) (string, error) {
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
		if err != nil || tab.Type != "page" || tab.WebSocketDebuggerURL == "" || page.Scheme != "https" ||
			!strings.Contains(page.Hostname(), "deepseek.com") {
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

		// 1. Kích hoạt Network và Runtime
		_ = conn.WriteJSON(map[string]any{"id": 1, "method": "Network.enable"})
		_ = conn.WriteJSON(map[string]any{"id": 2, "method": "Runtime.enable"})

		// 2. Thử evaluate localStorage.getItem('userToken')
		evalReq := map[string]any{
			"id":     10,
			"method": "Runtime.evaluate",
			"params": map[string]any{
				"expression":    `localStorage.getItem('userToken') || ''`,
				"returnByValue": true,
			},
		}
		_ = conn.WriteJSON(evalReq)

		// 3. Thử lấy cookies từ chat.deepseek.com
		cookieReq := map[string]any{
			"id":     11,
			"method": "Network.getCookies",
			"params": map[string]any{
				"urls": []string{"https://chat.deepseek.com"},
			},
		}
		_ = conn.WriteJSON(cookieReq)

		_ = conn.SetReadDeadline(time.Now().Add(wait))
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				break
			}

			// Kiểm tra kết quả Runtime.evaluate
			var evalResp struct {
				ID     int `json:"id"`
				Result struct {
					Result struct {
						Type  string `json:"type"`
						Value any    `json:"value"`
					} `json:"result"`
				} `json:"result"`
			}
			if json.Unmarshal(data, &evalResp) == nil && evalResp.ID == 10 {
				if valStr, ok := evalResp.Result.Result.Value.(string); ok && valStr != "" {
					cleanedToken := extractRawTokenString(valStr)
					if cleanedToken != "" {
						conn.Close()
						return cleanedToken, nil
					}
				}
			}

				// Kiểm tra kết quả Network.getCookies
				var cookieResp struct {
					ID     int `json:"id"`
					Result struct {
						Cookies []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"cookies"`
					} `json:"result"`
				}
				if json.Unmarshal(data, &cookieResp) == nil && cookieResp.ID == 11 {
					for _, c := range cookieResp.Result.Cookies {
						if strings.EqualFold(c.Name, "userToken") || strings.EqualFold(c.Name, "ds_user_token") {
							val := extractRawTokenString(c.Value)
							if val != "" {
								conn.Close()
								return val, nil
							}
						}
					}
				}

				// Kiểm tra sự kiện Network.requestWillBeSent
			var event struct {
				Method string `json:"method"`
				Params struct {
					Request struct {
						URL     string            `json:"url"`
						Headers map[string]string `json:"headers"`
					} `json:"request"`
				} `json:"params"`
			}
			if json.Unmarshal(data, &event) == nil && event.Method == "Network.requestWillBeSent" {
				authHdr := event.Params.Request.Headers["authorization"]
				if authHdr == "" {
					authHdr = event.Params.Request.Headers["Authorization"]
				}
				if strings.HasPrefix(authHdr, "Bearer ") {
					tok := strings.TrimSpace(strings.TrimPrefix(authHdr, "Bearer "))
					if len(tok) > 20 {
						conn.Close()
						return tok, nil
					}
				}
			}
		}
		conn.Close()
	}
	return "", nil
}

// extractRawTokenString bóc tách token nếu userToken lưu dạng JSON object trong localStorage.
func extractRawTokenString(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "undefined" {
		return ""
	}
	if strings.HasPrefix(raw, "{") {
		var obj struct {
			Value     string `json:"value"`
			UserToken string `json:"userToken"`
			Token     string `json:"token"`
		}
		if json.Unmarshal([]byte(raw), &obj) == nil {
			if obj.Value != "" {
				return obj.Value
			}
			if obj.UserToken != "" {
				return obj.UserToken
			}
			if obj.Token != "" {
				return obj.Token
			}
		}
	}
	if strings.HasPrefix(raw, "\"") && strings.HasSuffix(raw, "\"") && len(raw) > 2 {
		return raw[1 : len(raw)-1]
	}
	return raw
}

// ExtractEmailFromToken decodes the JWT payload and returns the best identity claim.
func ExtractEmailFromToken(jwtToken string) string { return extractEmailFromToken(jwtToken) }

// extractEmailFromToken giải mã phần payload của JWT để tìm trường email hoặc name nếu có.
func extractEmailFromToken(jwtToken string) string {
	parts := strings.Split(jwtToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payloadBytes, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return ""
	}
	var claims struct {
		Email    string `json:"email"`
		Sub      string `json:"sub"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(payloadBytes, &claims) == nil {
		if claims.Email != "" && strings.Contains(claims.Email, "@") {
			return claims.Email
		}
		if claims.Username != "" {
			return claims.Username
		}
		if claims.Name != "" {
			return claims.Name
		}
		if claims.Sub != "" {
			return "user-" + claims.Sub
		}
	}
	return ""
}
