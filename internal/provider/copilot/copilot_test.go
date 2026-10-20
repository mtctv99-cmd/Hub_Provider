package copilot

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/provider"
)

func TestExtractImageDataURIsKeepsAllImages(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	messages := []provider.Message{{Role: "user", Parts: []provider.ContentPart{
		{Type: "image_url", ImageURL: &provider.ImageURL{URL: uri}},
		{Type: "image_url", ImageURL: &provider.ImageURL{URL: uri}},
	}}}
	got, err := extractImageDataURIs(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != uri || got[1] != uri {
		t.Fatalf("expected both images to be retained; got %d", len(got))
	}
}

func TestExtractImageDataURIsRejectsInvalidAndUnfetchableImages(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "invalid data URI", url: "data:image/png;base64,not-base64!"},
		{name: "local URL", url: "http://127.0.0.1/image.png"},
		{name: "unsupported scheme", url: "file:///tmp/image.png"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			messages := []provider.Message{{Role: "user", Parts: []provider.ContentPart{{
				Type: "image_url", ImageURL: &provider.ImageURL{URL: tc.url},
			}}}}
			if got, err := extractImageDataURIs(context.Background(), messages); err == nil || got != nil {
				t.Fatalf("expected image processing error; images=%d err=%v", len(got), err)
			}
		})
	}
}

func TestFetchImageAsDataURIOverHTTPS(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(image)
	}))
	defer server.Close()
	if _, err := fetchImageAsDataURI(context.Background(), server.URL); err == nil {
		t.Fatal("expected local HTTPS image host to be rejected")
	}
}

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
