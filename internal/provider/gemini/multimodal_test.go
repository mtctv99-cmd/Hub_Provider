package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/provider"
)

func TestParseImageData(t *testing.T) {
	// 1. Valid data URL
	dummy := []byte("PNGDATA12345678")
	b64 := base64.StdEncoding.EncodeToString(dummy)
	dataURL := "data:image/png;base64," + b64

	data, filename, mimeType, err := ParseImageData(dataURL)
	if err != nil {
		t.Fatalf("ParseImageData failed: %v", err)
	}
	if string(data) != string(dummy) {
		t.Errorf("expected %s, got %s", dummy, data)
	}
	if mimeType != "image/png" {
		t.Errorf("expected image/png, got %s", mimeType)
	}
	if !strings.HasSuffix(filename, ".png") {
		t.Errorf("expected .png suffix, got %s", filename)
	}

	// 2. Invalid data URL (missing comma)
	_, _, _, err = ParseImageData("data:image/png;base64")
	if err == nil {
		t.Errorf("expected error for missing comma in data URL")
	}

	// 3. Oversized image rejection
	hugePayload := strings.Repeat("A", (maxImageBytes+100)*4/3)
	_, _, _, err = ParseImageData("data:image/png;base64," + hugePayload)
	if err == nil {
		t.Errorf("expected error for oversized image")
	}
}

func TestUploadImageMockScotty(t *testing.T) {
	var step int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		switch step {
		case 1:
			// Step 1: Scotty start
			if r.Header.Get("X-Goog-Upload-Command") != "start" {
				http.Error(w, "bad command", 400)
				return
			}
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload_bytes")
			w.WriteHeader(http.StatusOK)
		case 2:
			// Step 2: Upload bytes
			if r.Header.Get("X-Goog-Upload-Command") != "upload, finalize" {
				http.Error(w, "bad upload command", 400)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("/contrib_service/mock_file_id_999"))
		case 3:
			// Step 3: ProcessFile RPC
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[["wrb.fr",null,[]]]`))
		default:
			http.Error(w, "unexpected step", 500)
		}
	}))
	defer ts.Close()

	adapter := New(ts.Client())
	adapter.BaseURL = ts.URL

	tokens := PageTokens{
		PushID: "test-push",
		XSRF:   "test-xsrf",
		BL:     "test-bl",
		FSID:   "test-fsid",
	}

	adapter.UploadBaseURL = ts.URL

	uploaded, err := adapter.UploadImage(context.Background(), []byte("fake-png-content"), "image_1.png", "image/png", "", tokens)
	if err != nil {
		t.Fatalf("UploadImage failed: %v", err)
	}
	if uploaded.ID != "/contrib_service/mock_file_id_999" {
		t.Fatalf("unexpected uploaded file ID: %s", uploaded.ID)
	}
	if step != 3 {
		t.Fatalf("expected 3 upload stages, got %d", step)
	}

	payload := BuildPayload("describe this image", "gemini-3.8-flash", tokens.XSRF, "", ConversationMetadata{}, []UploadedFile{uploaded})
	if !strings.Contains(payload, "mock_file_id_999") {
		t.Errorf("expected payload to contain file ID: %s", payload)
	}
	if !strings.Contains(payload, "image_1.png") {
		t.Errorf("expected payload to contain file name: %s", payload)
	}
}

func TestChatWithImageUploadFlow(t *testing.T) {
	inner := []any{"ignored", []any{"c_vision", "r_vision"}, nil, nil, []any{[]any{"rc_vision", []any{"The image shows a blue circle."}}}}
	innerJSON, _ := json.Marshal(inner)
	fixture, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/app"):
			w.Write([]byte(`{"SNlM0e":"xsrf-token","cfb2h":"bl-token","FdrFJe":"fsid-token","qKIAYe":"feeds/test"}`))
		case r.Header.Get("X-Goog-Upload-Command") == "start":
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload_bytes")
			w.WriteHeader(http.StatusOK)
		case r.Header.Get("X-Goog-Upload-Command") == "upload, finalize":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("/contrib_service/mock_img_123"))
		case strings.Contains(r.URL.Path, "ProcessFile"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[["wrb.fr",null,[]]]`))
		case strings.Contains(r.URL.Path, "StreamGenerate"):
			w.WriteHeader(http.StatusOK)
			w.Write(fixture)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
		}
	}))
	defer server.Close()

	adapter := New(server.Client())
	adapter.SetBaseURL(server.URL)
	adapter.UploadBaseURL = server.URL

	dummyImageB64 := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("FAKEDATA"))
	req := provider.ChatRequest{
		Model: "gemini-3.8-flash",
		Messages: []provider.Message{
			{
				Role: "user",
				Parts: []provider.ContentPart{
					{Type: "text", Text: "What is this?"},
					{Type: "image_url", ImageURL: &provider.ImageURL{URL: dummyImageB64}},
				},
			},
		},
	}

	resp, err := adapter.Chat(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Chat with image failed: %v", err)
	}
	if resp.Content != "The image shows a blue circle." {
		t.Fatalf("unexpected content: %q", resp.Content)
	}
	if resp.Upstream.ConversationID != "c_vision" || resp.Upstream.ResponseID != "r_vision" || resp.Upstream.ChoiceID != "rc_vision" {
		t.Fatalf("unexpected upstream state: %+v", resp.Upstream)
	}
}
