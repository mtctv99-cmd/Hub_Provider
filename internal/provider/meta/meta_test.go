package meta

import (
	"strings"
	"testing"

	"sever_provider/internal/provider"
)

func TestAdapterNameAndCapabilities(t *testing.T) {
	a := New(nil)
	if got := a.Name(); got != "meta_web" {
		t.Fatalf("expected meta_web, got %s", got)
	}
	caps := a.Capabilities()
	has := func(c provider.Capability) bool {
		for _, x := range caps {
			if x == c {
				return true
			}
		}
		return false
	}
	for _, c := range []provider.Capability{
		provider.CapabilityChat,
		provider.CapabilityStreaming,
		provider.CapabilityVision,
		provider.CapabilityImage,
		provider.CapabilitySpeech,
	} {
		if !has(c) {
			t.Fatalf("missing capability %s", c)
		}
	}
}

func TestParseCredentialSecret(t *testing.T) {
	cookies, token, err := parseCredentialSecret([]byte(`{"cookies":"c_user=1; datr=2","token":"abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cookies != "c_user=1; datr=2" || token != "abc" {
		t.Fatalf("unexpected parse: %q %q", cookies, token)
	}

	cookies, token, err = parseCredentialSecret([]byte(`c_user=1; datr=2`))
	if err != nil || cookies == "" || token != "" {
		t.Fatalf("expected raw cookie string, got %q %q err=%v", cookies, token, err)
	}

	if _, _, err := parseCredentialSecret(nil); err == nil {
		t.Fatal("expected error for empty secret")
	}
}

func TestJoinMessages(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "system", Content: "be concise"},
			{Role: "user", Content: "hello"},
		},
	}
	joined := joinMessages(req)
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "be concise") {
		t.Fatalf("unexpected joined prompt: %q", joined)
	}
}

func TestExtractImages(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{
				Role: "user",
				Parts: []provider.ContentPart{
					{Type: "text", Text: "describe"},
					{Type: "image_url", ImageURL: &provider.ImageURL{URL: "data:image/png;base64,AAA"}},
				},
			},
		},
	}
	images := extractImages(req)
	if len(images) != 1 || images[0] != "data:image/png;base64,AAA" {
		t.Fatalf("unexpected images: %v", images)
	}
}

func TestExtractImageURL(t *testing.T) {
	u, err := extractImageURL([]byte(`{"url":"https://example.com/a.png"}`))
	if err != nil || u != "https://example.com/a.png" {
		t.Fatalf("expected image url, got %q err=%v", u, err)
	}
	if _, err := extractImageURL([]byte(`{"nothing":"here"}`)); err == nil {
		t.Fatal("expected error when no url present")
	}
}
