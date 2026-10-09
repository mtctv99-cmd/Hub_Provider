package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var ErrProfileAuthRequired = errors.New("profile has no usable Google authentication cookies")

func (m *Manager) RefreshAccountFromProfile(accountID string) (string, error) {
	return m.RefreshAccountFromProfileContext(context.Background(), accountID, "")
}

// restoreCookie must have been validated by the caller before restoring it.
func (m *Manager) RefreshAccountFromProfileContext(ctx context.Context, accountID, restoreCookie string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateProfileID(accountID); err != nil {
		return "", err
	}
	if !m.running.CompareAndSwap(false, true) {
		return "", ErrAutoLoginBusy
	}
	defer m.running.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	profile, err := m.profileDir(accountID)
	if err != nil {
		return "", err
	}
	if _, err = os.Stat(profile); err != nil {
		return "", fmt.Errorf("profile directory unavailable: %w", err)
	}
	port, err := existingProfileCDP(profile)
	if err != nil {
		return "", err
	}
	var done <-chan error
	if port == 0 {
		browser, err := m.FindBrowserPath()
		if err != nil {
			return "", err
		}
		port, err = findFreePort()
		if err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, browser, "--headless=new", fmt.Sprintf("--remote-debugging-port=%d", port), "--remote-debugging-address=127.0.0.1", "--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--disable-blink-features=AutomationControlled", "https://gemini.google.com/app")
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("start refresh browser: %w", err)
		}
		exited := make(chan error, 1)
		done = exited
		go func() { exited <- cmd.Wait(); close(exited) }()
		defer func() {
			closeRefreshBrowser(port)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}()
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	ready, seeded := false, false
	attempts := 0
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("profile refresh deadline (CDP_ready=%t): %w", ready, ctx.Err())
		case err := <-done:
			return "", fmt.Errorf("refresh browser exited before cookie capture: %v", err)
		case <-tick.C:
			attempts++
			if cookie, _, ok := captureCookiesViaCDP(port, profile); ok && cookie != "" {
				return cookie, nil
			}
			target, err := refreshTarget(port)
			if err != nil {
				continue
			}
			ready = true
			if restoreCookie != "" && !seeded {
				if err := restoreGoogleCookies(target, restoreCookie); err != nil {
					return "", fmt.Errorf("restore validated profile cookies: %w", err)
				}
				seeded = true
				continue
			}
			if attempts >= 15 {
				return "", ErrProfileAuthRequired
			}
		}
	}
}

func refreshTarget(port int) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tabs []cdpTab
	if err := json.NewDecoder(resp.Body).Decode(&tabs); err != nil {
		return "", err
	}
	for _, t := range tabs {
		page, e := url.Parse(t.URL)
		endpoint, ee := url.Parse(t.WebSocketDebuggerURL)
		if e == nil && ee == nil && t.Type == "page" && page.Scheme == "https" && (page.Hostname() == "gemini.google.com" || page.Hostname() == "accounts.google.com" || page.Hostname() == "aistudio.google.com") && endpoint.Scheme == "ws" && endpoint.Hostname() == "127.0.0.1" && endpoint.Port() == strconv.Itoa(port) {
			return endpoint.String(), nil
		}
	}
	return "", errors.New("Google CDP page not ready")
}

func restoredCookieParams(raw string) ([]map[string]any, error) {
	var cookies []map[string]any
	for _, part := range strings.Split(raw, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" || strings.ContainsAny(name, "\r\n\t ") || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("invalid stored cookie format")
		}
		c := map[string]any{"name": name, "value": value, "path": "/", "secure": true, "expires": float64(time.Now().Add(30 * 24 * time.Hour).Unix())}
		if strings.HasPrefix(name, "__Host-") {
			c["url"] = "https://accounts.google.com/"
		} else {
			c["domain"] = ".google.com"
		}
		cookies = append(cookies, c)
	}
	if len(cookies) == 0 {
		return nil, errors.New("empty stored cookie")
	}
	return cookies, nil
}

func restoreGoogleCookies(target, raw string) error {
	cookies, err := restoredCookieParams(raw)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, _, err := dialer.Dial(target, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteJSON(map[string]any{"id": 201, "method": "Network.setCookies", "params": map[string]any{"cookies": cookies}}); err != nil {
		return err
	}
	for {
		var reply struct {
			ID    int             `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		if err := conn.ReadJSON(&reply); err != nil {
			return err
		}
		if reply.ID != 201 {
			continue
		}
		if len(reply.Error) > 0 {
			return errors.New("CDP rejected stored cookies")
		}
		break
	}
	return conn.WriteJSON(map[string]any{"id": 202, "method": "Page.reload"})
}

// Reuse a live local CDP only when its process explicitly owns this profile.
// Remove stale Chrome singleton files only after proving their owner is absent.
func existingProfileCDP(profile string) (int, error) {
	lock := filepath.Join(profile, "SingletonLock")
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(lock); os.IsNotExist(err) {
			return 0, nil
		}
		// On Windows, SingletonLock is a regular file. Test if locked by an active process.
		f, openErr := os.OpenFile(lock, os.O_RDWR, 0666)
		if openErr == nil {
			_ = f.Close()
			for _, name := range []string{"SingletonSocket", "SingletonCookie", "SingletonLock"} {
				_ = os.Remove(filepath.Join(profile, name))
			}
			return 0, nil
		}
		log.Printf("[existingProfileCDP] profile is busy (goos=%s profile=%s): %v", runtime.GOOS, profile, openErr)
		return 0, ErrAutoLoginBusy
	}
	target, err := os.Readlink(lock)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil || runtime.GOOS != "linux" {
		log.Printf("[existingProfileCDP] profile is busy on non-linux platform (goos=%s profile=%s): %v", runtime.GOOS, profile, err)
		return 0, ErrAutoLoginBusy
	}
	host, err := os.Hostname()
	index := strings.LastIndex(target, "-")
	if err != nil || index < 0 || target[:index] != host {
		log.Printf("[existingProfileCDP] lock target mismatch (goos=%s host=%s target=%s)", runtime.GOOS, host, target)
		return 0, ErrAutoLoginBusy
	}
	pid, err := strconv.Atoi(target[index+1:])
	if err != nil || pid <= 0 {
		log.Printf("[existingProfileCDP] invalid pid in lock (goos=%s target=%s)", runtime.GOOS, target)
		return 0, ErrAutoLoginBusy
	}
	proc := fmt.Sprintf("/proc/%d", pid)
	if _, err := os.Stat(proc); os.IsNotExist(err) {
		again, e := os.Readlink(lock)
		if e != nil || again != target {
			log.Printf("[existingProfileCDP] lock changed during stale check (goos=%s)", runtime.GOOS)
			return 0, ErrAutoLoginBusy
		}
		for _, name := range []string{"SingletonSocket", "SingletonCookie", "SingletonLock"} {
			if e := os.Remove(filepath.Join(profile, name)); e != nil && !os.IsNotExist(e) {
				return 0, e
			}
		}
		return 0, nil
	} else if err != nil {
		log.Printf("[existingProfileCDP] stat /proc/%d failed (goos=%s): %v", pid, runtime.GOOS, err)
		return 0, ErrAutoLoginBusy
	}
	raw, err := os.ReadFile(filepath.Join(proc, "cmdline"))
	if err != nil {
		log.Printf("[existingProfileCDP] read cmdline for pid %d failed (goos=%s): %v", pid, runtime.GOOS, err)
		return 0, ErrAutoLoginBusy
	}
	var owned string
	port := 0
	for _, arg := range strings.Split(string(raw), "\x00") {
		if strings.HasPrefix(arg, "--user-data-dir=") {
			owned = strings.TrimPrefix(arg, "--user-data-dir=")
		}
		if strings.HasPrefix(arg, "--remote-debugging-port=") {
			port, _ = strconv.Atoi(strings.TrimPrefix(arg, "--remote-debugging-port="))
		}
	}
	actual, e := filepath.EvalSymlinks(owned)
	expected, ee := filepath.EvalSymlinks(profile)
	if e != nil || ee != nil || actual != expected || port <= 0 || port > 65535 {
		log.Printf("[existingProfileCDP] profile ownership mismatch or invalid port %d (goos=%s actual=%s expected=%s)", port, runtime.GOOS, actual, expected)
		return 0, ErrAutoLoginBusy
	}
	return port, nil
}

// Ask owned Chrome to flush its persistent profile before falling back to kill.
func closeRefreshBrowser(port int) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var version struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	if json.NewDecoder(resp.Body).Decode(&version) != nil {
		return
	}
	endpoint, err := url.Parse(version.URL)
	if err != nil || endpoint.Scheme != "ws" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() != strconv.Itoa(port) {
		return
	}
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.Dial(version.URL, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = conn.WriteJSON(map[string]any{"id": 301, "method": "Browser.close"})
}
