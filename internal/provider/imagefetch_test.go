package provider

import (
	"errors"
	"net/netip"
	"testing"
)

func TestPublicImageIPRejectsUnroutableRanges(t *testing.T) {
	// 0.0.0.0/8 is not private/loopback per netip but routes to the local host
	// on Linux, so a DNS name resolving there must not pass.
	for _, raw := range []string{"0.0.0.0", "0.1.2.3", "10.1.2.3", "100.64.1.1", "127.0.0.1", "169.254.1.1", "::1", "fc00::1"} {
		if PublicImageIP(netip.MustParseAddr(raw)) {
			t.Fatalf("%s accepted as public", raw)
		}
	}
	if !PublicImageIP(netip.MustParseAddr("93.184.216.34")) {
		t.Fatal("public address rejected")
	}
}

func TestValidateImageURL(t *testing.T) {
	allowedHTTPS := []string{
		"https://example.com/a.png",
		"https://cdn.example.com:443/a.png",
		"http://example.com/a.png",
	}
	for _, raw := range allowedHTTPS {
		if err := ValidateImageURL(raw, true); err != nil {
			t.Fatalf("allowHTTP: %s rejected: %v", raw, err)
		}
	}
	if err := ValidateImageURL("https://example.com/a.png", false); err != nil {
		t.Fatalf("https rejected: %v", err)
	}

	// allowHTTP=false still refuses plain http; everything below is refused in
	// both modes because the host itself is not publicly reachable.
	if err := ValidateImageURL("http://example.com/a.png", false); !errors.Is(err, ErrInvalidImageURL) {
		t.Fatalf("http accepted with allowHTTP=false: %v", err)
	}
	rejected := []string{
		"https://127.0.0.1/x.png",
		"https://169.254.169.254/latest/meta-data",
		"https://10.0.0.1/x.png",
		"https://192.168.1.1/x.png",
		"https://100.64.0.1/x.png",
		"https://localhost/x.png",
		"https://app.localhost/x.png",
		"https://internal-host/x.png",
		"https://example.com:8080/x.png",
		"http://example.com:8443/x.png",
		"https://user:pass@example.com/x.png",
		"https://example.com/x.png#frag",
		"file:///etc/passwd",
		"ftp://example.com/x.png",
		"not a url",
	}
	for _, raw := range rejected {
		if err := ValidateImageURL(raw, true); !errors.Is(err, ErrInvalidImageURL) {
			t.Fatalf("%s: err=%v, want ErrInvalidImageURL", raw, err)
		}
	}
}

func TestImageMimeType(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	html := []byte("<!doctype html><html></html>")

	valid := []struct {
		contentType string
		data        []byte
		want        string
	}{
		{"image/png", png, "image/png"},
		{"image/png; charset=binary", png, "image/png"},
		{"", png, "image/png"},
		{"application/octet-stream", png, "image/png"},
		// A declared image type wins over sniffing so avif/heic survive.
		{"image/avif", []byte("....ftypavif"), "image/avif"},
	}
	for _, tc := range valid {
		got, err := ImageMimeType(tc.contentType, tc.data)
		if err != nil || got != tc.want {
			t.Fatalf("ImageMimeType(%q)=%q,%v want %q", tc.contentType, got, err, tc.want)
		}
	}

	if _, err := ImageMimeType("text/html", html); !errors.Is(err, ErrInvalidImageURL) {
		t.Fatalf("html accepted: %v", err)
	}
	if _, err := ImageMimeType("", html); !errors.Is(err, ErrInvalidImageURL) {
		t.Fatalf("sniffed html accepted: %v", err)
	}
}
