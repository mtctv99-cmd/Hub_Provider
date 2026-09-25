package copilot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/provider"
)

func TestTokenValidation(t *testing.T) {
	if IsTokenValid("not-a-token", 1) {
		t.Fatal("invalid token accepted")
	}
	if _, _, _, err := BuildInvocationURL("wss://example.test", nil); err != nil {
		t.Fatal(err)
	}
}

func TestFormatMessagesAndUpstreamRoundTrip(t *testing.T) {
	text := formatMessages([]provider.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}})
	if !strings.Contains(text, "User: hello") || !strings.Contains(text, "Assistant: hi") {
		t.Fatalf("text=%q", text)
	}
	state := upstreamFromSession(Session{ConversationID: "c", ChatSessionID: "s", InvocationID: 2})
	if state.ConversationID != "c" || state.ChatSessionID != "s" || state.InvocationID != 2 {
		t.Fatalf("state=%+v", state)
	}
	got := sessionFromUpstream(state)
	if got.ConversationID != "c" || got.ChatSessionID != "s" || got.InvocationID != 2 {
		t.Fatalf("session=%+v", got)
	}
}

func TestExtractImageURLs(t *testing.T) {
	urls := extractImageURLs(map[string]any{"messages": []any{map[string]any{"text": "![image](https://example.test/a.png)"}}})
	if len(urls) != 1 || urls[0] != "https://example.test/a.png" {
		t.Fatalf("urls=%v", urls)
	}
}

func TestDownloadImageRejectsLocalHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("jpeg"))
	}))
	defer server.Close()
	if _, err := downloadImage(context.Background(), server.URL+"/image"); err == nil {
		t.Fatal("expected local HTTP image rejection")
	}
}

func TestCopilotImageSSRFControls(t *testing.T) {
	ctx := context.Background()
	unsupportedSchemes := []string{
		"http://example.com/image.png",
		"ftp://example.com/image.png",
		"file:///etc/passwd",
		"gopher://example.com",
	}
	for _, raw := range unsupportedSchemes {
		if _, err := downloadImage(ctx, raw); err == nil {
			t.Fatalf("expected scheme rejection for %s", raw)
		}
	}

	dangerousHosts := []string{
		"https://127.0.0.1/test.png",
		"https://169.254.169.254/latest/meta-data",
		"https://10.0.0.1/test.png",
		"https://192.168.1.1/test.png",
		"https://localhost/test.png",
		"https://app.localhost/test.png",
		"https://internal-host/test.png",
		"https://example.com:8080/test.png",
	}
	for _, raw := range dangerousHosts {
		if _, err := downloadImage(ctx, raw); err == nil {
			t.Fatalf("expected SSRF host rejection for %s", raw)
		}
	}
}

func TestPublicImageIP(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.1", "192.168.1.1", "100.64.0.1", "198.18.0.1", "203.0.113.1", "::1", "fc00::1", "2001:db8::1", "::ffff:127.0.0.1"} {
		if publicImageIP(netip.MustParseAddr(raw)) {
			t.Fatalf("accepted nonpublic IP %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicImageIP(netip.MustParseAddr(raw)) {
			t.Fatalf("rejected public IP %s", raw)
		}
	}
}

func TestCopilotAdapterImageValidation(t *testing.T) {
	adapter := New(nil)
	caps := adapter.Capabilities()
	hasImage := false
	for _, c := range caps {
		if c == provider.CapabilityImage {
			hasImage = true
			break
		}
	}
	if !hasImage {
		t.Fatal("expected copilot adapter to support CapabilityImage")
	}

	// Secret missing token_url
	if _, err := adapter.Image(context.Background(), provider.ImageRequest{Prompt: "test"}, []byte(`{}`)); err == nil {
		t.Fatal("expected error on empty secret")
	}

	// Prompt empty
	validTokenURL := testTokenURL("substrate.office.com", "/m365Copilot/Chathub/11111111-2222-3333-4444-555555555555@66666666-7777-8888-9999-000000000000", time.Now().Add(time.Hour).Unix())
	secretJSON := fmt.Sprintf(`{"token_url":%q}`, validTokenURL)
	if _, err := adapter.Image(context.Background(), provider.ImageRequest{Prompt: "   "}, []byte(secretJSON)); err == nil {
		t.Fatal("expected error on empty prompt")
	}
}
