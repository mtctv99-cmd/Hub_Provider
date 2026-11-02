package gemini

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/provider"
)

func TestImageRPCHeaderRejectsNonImageModel(t *testing.T) {
	if _, err := imageRPCHeader("gemini-3.1-pro"); !errors.Is(err, provider.ErrCapabilityUnsupported) {
		t.Fatalf("err=%v", err)
	}
	header, err := imageRPCHeader("gemini-3.8-flash")
	want := `[1,null,null,null,"56fdd199312815e2",null,null,0,[4,5,6,8,4,5,6,8],null,null,2,null,null,1,1,"37148B01-D513-431C-B2FD-18F42A6EFE22"]`
	if err != nil || header != want {
		t.Fatalf("header=%q err=%v", header, err)
	}
	alias, err := imageRPCHeader("gemini-3.8-flash-low")
	if err != nil || alias != header {
		t.Fatalf("alias header=%q err=%v", alias, err)
	}
	a := New(nil)
	if _, err := a.Image(context.Background(), provider.ImageRequest{Model: "gemini-3.1-pro", Prompt: "bird"}, nil); !errors.Is(err, provider.ErrCapabilityUnsupported) {
		t.Fatalf("non-image model must be rejected, got %v", err)
	}
}

func TestCleanTextStripsImageMarkers(t *testing.T) {
	got := cleanText("before https://googleusercontent.com/image_generation_content/0 after <Image src=\"x\"/> end")
	if strings.Contains(got, "googleusercontent") || strings.Contains(got, "<Image") {
		t.Fatalf("markers remain: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") || !strings.Contains(got, "end") {
		t.Fatalf("text lost: %q", got)
	}
}

func TestChatMarkerOnlyBecomesCapabilityUnsupported(t *testing.T) {
	srv := tokenHandlerRPC(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, imageFrame(t, []any{[]any{fixtureImageURL}}, ""))
	})
	defer srv.Close()
	a := New(&http.Client{})
	a.SetBaseURL(srv.URL)
	_, err := a.Chat(context.Background(), provider.ChatRequest{Model: "gemini-3.8-flash", Messages: []provider.Message{{Role: "user", Content: "draw"}}}, nil)
	if !errors.Is(err, provider.ErrCapabilityUnsupported) {
		t.Fatalf("expected capability unsupported, got %v", err)
	}
}

func TestChatImageUsesMode10ForImage(t *testing.T) {
	var rpcCalls, cdnCalls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "lh3.googleusercontent.com":
			cdnCalls++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(fixturePNG)
		default:
			if r.Method == http.MethodGet && r.URL.Path == "/app" {
				_, _ = io.WriteString(w, `{"SNlM0e":"xsrf","cfb2h":"bl","FdrFJe":"fsid"}`)
				return
			}
			rpcCalls++
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if !strings.Contains(r.Form.Get("f.req"), "Generate an image of: a blue bird") {
				t.Error("missing image prompt in payload")
			}
			want, err := imageRPCHeader("gemini-3.8-flash")
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Header.Get("x-goog-ext-525001261-jspb"); got != want {
				t.Errorf("header=%q want=%q", got, want)
			}
			_, _ = io.WriteString(w, imageFrame(t, []any{[]any{fixtureImageURL}}, ""))
		}
	}))
	defer server.Close()
	client := server.Client()
	base := client.Transport
	client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "lh3.googleusercontent.com" {
			copyReq := req.Clone(req.Context())
			copyReq.URL = cloneURL(req.URL)
			copyReq.URL.Host = strings.TrimPrefix(server.URL, "https://")
			return base.RoundTrip(copyReq)
		}
		return base.RoundTrip(req)
	})
	a := New(client)
	a.SetBaseURL(server.URL)
	resp, err := a.Chat(context.Background(), provider.ChatRequest{
		Model:           "gemini-3.8-flash",
		ImageGeneration: true,
		Messages:        []provider.Message{{Role: "user", Content: "a blue bird"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resp.ImageData, fixturePNG) || resp.ImageMIME != "image/png" {
		t.Fatalf("image data=%d mime=%q", len(resp.ImageData), resp.ImageMIME)
	}
	if rpcCalls != 1 || cdnCalls != 1 {
		t.Fatalf("RPC=%d CDN=%d", rpcCalls, cdnCalls)
	}
}
