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
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/provider/copilot"
)

// CaptureCopilotTokenFromProfile opens an already signed-in Copilot Chrome
// profile headless, loads the Copilot chat page and captures the Chathub token
// URL the live session requests. It needs no human input; a signed-out profile
// returns ErrProfileAuthRequired.
func CaptureCopilotTokenFromProfile(ctx context.Context, profilesDir, profileID string) (string, string, time.Time, error) {
	if !validCopilotProfileID(profileID) {
		return "", "", time.Time{}, errors.New("invalid copilot profile id")
	}
	root, err := filepath.Abs(profilesDir)
	if err != nil {
		return "", "", time.Time{}, errors.New("invalid profile directory")
	}
	profile := filepath.Join(root, "copilot", profileID)
	if _, err := os.Stat(profile); err != nil {
		return "", "", time.Time{}, fmt.Errorf("copilot profile unavailable: %w", err)
	}
	port, err := existingProfileCDP(profile)
	if err != nil {
		return "", "", time.Time{}, err
	}
	var done <-chan error
	if port == 0 {
		browserPath, err := (&Manager{}).FindBrowserPath()
		if err != nil {
			return "", "", time.Time{}, err
		}
		port, err = findFreePort()
		if err != nil {
			return "", "", time.Time{}, err
		}
		cleanSingletonLocks(profile)
		cmd := exec.CommandContext(ctx, browserPath, "--headless=new", fmt.Sprintf("--remote-debugging-port=%d", port), "--remote-debugging-address=127.0.0.1", "--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--disable-blink-features=AutomationControlled", copilotHome)
		if err := cmd.Start(); err != nil {
			return "", "", time.Time{}, fmt.Errorf("start copilot refresh browser: %w", err)
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
			cleanSingletonLocks(profile)
		}()
	}
	if err := waitForCDP(ctx, port, 20*time.Second); err != nil {
		return "", "", time.Time{}, err
	}
	token, email, expiry, err := captureCopilotToken(ctx, port)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("copilot profile signed out or chat UI unavailable: %w", err)
	}
	return token, email, expiry, nil
}

// waitForCDP polls the DevTools endpoint until Chrome is accepting connections.
func waitForCDP(ctx context.Context, port int, limit time.Duration) error {
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return errors.New("copilot refresh browser did not expose DevTools")
}

// captureCopilotToken holds one DevTools connection for the whole capture: it
// enables Network before navigating, so websocket creation events (where the
// Chathub token appears) are observed instead of missed.
func captureCopilotToken(ctx context.Context, port int) (string, string, time.Time, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer resp.Body.Close()
	var tabs []cdpTab
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&tabs); err != nil {
		return "", "", time.Time{}, err
	}
	var endpoint string
	for _, tab := range tabs {
		page, err := url.Parse(tab.URL)
		if err != nil || tab.Type != "page" || tab.WebSocketDebuggerURL == "" || page.Scheme != "https" ||
			(page.Hostname() != "m365.cloud.microsoft" && page.Hostname() != "copilot.microsoft.com") {
			continue
		}
		endpoint = tab.WebSocketDebuggerURL
		break
	}
	if endpoint == "" {
		return "", "", time.Time{}, errors.New("no copilot chat tab found")
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer conn.Close()
	send := func(id int, method string, params map[string]any) {
		_ = conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params})
	}
	send(1, "Network.enable", map[string]any{})
	send(2, "Runtime.enable", map[string]any{})
	send(3, "Page.enable", map[string]any{})
	send(4, "Page.navigate", map[string]any{"url": copilotHome})

	deadline := time.Now().Add(40 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	// gorilla panics if a connection is read again after any read error, so the
	// reader runs once in its own goroutine and the loop only consumes events.
	events := make(chan string, 64)
	go func() {
		defer close(events)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case events <- string(data):
			case <-ctx.Done():
				return
			}
		}
	}()
	nudge := time.NewTimer(12 * time.Second)
	defer nudge.Stop()
	nudges := 0
	for {
		select {
		case <-ctx.Done():
			return "", "", time.Time{}, ctx.Err()
		case <-time.After(time.Until(deadline)):
			return "", "", time.Time{}, ErrProfileAuthRequired
		case <-nudge.C:
			if nudges < 3 {
				nudges++
				nudgeOnConn(conn)
				nudge.Reset(9 * time.Second)
			}
		case text, ok := <-events:
			if !ok {
				return "", "", time.Time{}, errors.New("devtools connection closed")
			}
			if !strings.Contains(text, "Chathub") || !strings.Contains(text, "access_token") {
				continue
			}
			candidate := extractCDPURL(text)
			if candidate == "" {
				continue
			}
			if expiry, email, err := copilot.ValidateCapturedToken(candidate, 60); err == nil {
				return candidate, email, expiry, nil
			}
		}
	}
}

// extractCDPURL pulls a URL out of a Network event frame without assuming the
// exact event shape (webSocketCreated carries url, requests nest it).
func extractCDPURL(text string) string {
	var event struct {
		Params struct {
			URL     string `json:"url"`
			Request struct {
				URL string `json:"url"`
			} `json:"request"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(text), &event) != nil {
		return ""
	}
	if event.Params.URL != "" {
		return event.Params.URL
	}
	return event.Params.Request.URL
}

// nudgeOnConn focuses the composer, inserts a minimal prompt and presses Enter
// on the already-open DevTools connection to start a conversation.
func nudgeOnConn(conn *websocket.Conn) {
	send := func(id int, method string, params map[string]any) {
		_ = conn.WriteJSON(map[string]any{"id": 100 + id, "method": method, "params": params})
	}
	send(1, "Runtime.evaluate", map[string]any{
		"expression":    `(() => { const el = document.querySelector('textarea, [contenteditable="true"]'); if (!el) return false; el.focus(); return true; })()`,
		"returnByValue": true,
	})
	send(2, "Input.insertText", map[string]any{"text": "hi"})
	send(3, "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13})
	send(4, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13})
}
