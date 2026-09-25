package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"sever_provider/internal/provider"
)

const fixtureImageURL = "https://lh3.googleusercontent.com/gg-dl/fixture-image"

var fixturePNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 2, 0, 0, 0}

func imageFrame(t *testing.T, attachment any, text string) string {
	t.Helper()
	candidate := make([]any, 13)
	candidate[0], candidate[1], candidate[12] = "rc_fixture", []any{text}, attachment
	inner := []any{nil, []any{"c_fixture", "r_fixture"}, nil, nil, []any{candidate}}
	encoded, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(encoded)}})
	if err != nil {
		t.Fatal(err)
	}
	return ")]}'\n123\n" + string(frame) + "\n"
}

func TestExtractGeneratedImageURL(t *testing.T) {
	got, err := extractGeneratedImageURL(imageFrame(t, []any{nil, []any{fixtureImageURL}}, "hello"))
	if err != nil || got != fixtureImageURL {
		t.Fatalf("url=%q err=%v", got, err)
	}
	for name, input := range map[string]string{
		"prose":            imageFrame(t, nil, fixtureImageURL),
		"other attachment": imageFrame(t, []any{"https://evil.example/image.png"}, ""),
		"ambiguous":        imageFrame(t, []any{fixtureImageURL, "https://lh3.googleusercontent.com/gg-dl/another"}, ""),
		"malformed frame":  `[["wrb.fr",null,"not json"]]`,
		"oversize":         strings.Repeat("a", maxImageRPCBytes+1),
		"too many frames":  strings.Repeat("\n", maxImageLines),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := extractGeneratedImageURL(input); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestGeneratedImageURLAllowlist(t *testing.T) {
	for _, raw := range []string{
		"http://lh3.googleusercontent.com/gg-dl/id",
		"https://lh3.googleusercontent.com.evil.test/gg-dl/id",
		"https://lh3.googleusercontent.com:443/gg-dl/id",
		"https://user@lh3.googleusercontent.com/gg-dl/id",
		"https://lh3.googleusercontent.com/gg-dl/id?redirect=evil",
		"https://lh3.googleusercontent.com/a/avatar",
		"https://lh3.googleusercontent.com/gg-dl/%2fadmin",
		"https://lh3.googleusercontent.com/gg-dl/id#fragment",
	} {
		if _, ok := generatedImageURL(raw); ok {
			t.Errorf("accepted %q", raw)
		}
	}
	if _, ok := generatedImageURL(fixtureImageURL); !ok {
		t.Fatal("valid image URL rejected")
	}
}

func TestImageStreamGenerateAndCDNDownload(t *testing.T) {
	var rpcCalls, cdnCalls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "lh3.googleusercontent.com":
			cdnCalls++
			if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
				t.Error("credential leaked to CDN")
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(fixturePNG)
		default:
			if r.Method == http.MethodGet && r.URL.Path == "/app" {
				_, _ = io.WriteString(w, `{"SNlM0e":"xsrf","cfb2h":"bl","FdrFJe":"fsid"}`)
				return
			}
			rpcCalls++
			if !strings.Contains(r.URL.Path, "StreamGenerate") {
				t.Errorf("unexpected RPC path %s", r.URL.Path)
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("at") != "xsrf" || !strings.Contains(r.Form.Get("f.req"), "Generate an image of: a blue bird") {
				t.Error("missing token or prompt in payload")
			}
			_, _ = io.WriteString(w, imageFrame(t, []any{[]any{fixtureImageURL}}, ""))
		}
	}))
	defer server.Close()
	client := server.Client()
	originalTransport := client.Transport
	client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "lh3.googleusercontent.com" {
			copyReq := req.Clone(req.Context())
			copyReq.URL = cloneURL(req.URL)
			copyReq.URL.Host = strings.TrimPrefix(server.URL, "https://")
			return originalTransport.RoundTrip(copyReq)
		}
		return originalTransport.RoundTrip(req)
	})
	a := New(client)
	a.SetBaseURL(server.URL)
	secret := []byte(`{"cookie":"__Secure-1PSID=session; SAPISID=sapisid"}`)
	resp, err := a.Image(context.Background(), provider.ImageRequest{Model: "gemini-3.8-flash", Prompt: "a blue bird"}, secret)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resp.Data, fixturePNG) || resp.MIME != "image/png" || resp.URL != "" {
		t.Fatalf("unexpected image response: mime=%q bytes=%d url=%q", resp.MIME, len(resp.Data), resp.URL)
	}
	if rpcCalls != 1 || cdnCalls != 1 {
		t.Fatalf("RPC=%d CDN=%d", rpcCalls, cdnCalls)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func cloneURL(u *url.URL) *url.URL                                             { v := *u; return &v }

func TestImageRejectsUnsupportedBeforeNetwork(t *testing.T) {
	a := New(nil)
	for _, req := range []provider.ImageRequest{
		{Model: "gemini-3.1-pro", Prompt: "bird"},
		{Model: "gemini-3.8-flash", Prompt: "bird", Size: "1024x1024"},
		{Model: "gemini-3.8-flash", Prompt: " "},
	} {
		if _, err := a.Image(context.Background(), req, nil); !errors.Is(err, provider.ErrCapabilityUnsupported) {
			t.Fatalf("request=%+v err=%v", req, err)
		}
	}
	if _, err := a.Image(context.Background(), provider.ImageRequest{Model: "gemini-3.8-flash", Prompt: "bird"}, nil); err == nil {
		t.Fatal("guest image generation should fail closed")
	}
}

func TestDownloadGeneratedImageValidation(t *testing.T) {
	cases := []struct {
		name     string
		mime     string
		data     []byte
		status   int
		redirect string
	}{
		{name: "valid", mime: "image/png", data: fixturePNG},
		{name: "wrong MIME", mime: "text/html", data: fixturePNG},
		{name: "wrong bytes", mime: "image/png", data: []byte("<html>not an image</html>")},
		{name: "oversize", mime: "image/png", data: bytes.Repeat([]byte("a"), maxGeneratedImageBytes+1)},
		{name: "redirect", status: 302, redirect: "http://127.0.0.1/private"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.redirect != "" {
					w.Header().Set("Location", tc.redirect)
					w.WriteHeader(tc.status)
					return
				}
				w.Header().Set("Content-Type", tc.mime)
				_, _ = w.Write(tc.data)
			}))
			defer server.Close()
			client := server.Client()
			base := client.Transport
			client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "lh3.googleusercontent.com" {
					t.Errorf("unexpected host: %s", req.URL.Host)
				}
				copyReq := req.Clone(req.Context())
				copyReq.URL = cloneURL(req.URL)
				copyReq.URL.Host = strings.TrimPrefix(server.URL, "https://")
				return base.RoundTrip(copyReq)
			})
			a := New(client)
			response, err := a.downloadGeneratedImage(context.Background(), fixtureImageURL)
			if tc.name == "valid" {
				if err != nil || response.MIME != "image/png" {
					t.Fatalf("response=%+v err=%v", response, err)
				}
			} else if err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
