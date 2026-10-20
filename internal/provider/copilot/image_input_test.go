package copilot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func testPNGDataURI() string {
	data := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

func TestUploadImagesUsesCopilotUploadFileContract(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret-test-token" {
			t.Errorf("unexpected upload request method/auth")
		}
		if r.Header.Get("X-Scenario") != "OfficeWebIncludedCopilot" || r.Header.Get("X-Variants") != "feature.EnableImageSupportInUploadFile" {
			t.Errorf("missing Copilot upload headers")
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse upload form: %v", err)
			return
		}
		if r.Form.Get("scenario") != "UploadImage" || r.Form.Get("conversationId") != "conversation-1" || r.Form.Get("FileBase64") != testPNGDataURI() {
			t.Errorf("unexpected UploadFile form fields")
		}
		options := r.Form["optionsSets"]
		if len(options) != 2 || options[0] != "cwcgptvsan" || options[1] != "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch" {
			t.Errorf("unexpected optionsSets: %v", options)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"docId":"doc-%d"}`, calls)
	}))
	defer server.Close()

	adapter := New(nil)
	adapter.UploadURL = server.URL
	adapter.HTTPClient = server.Client()
	images := []string{testPNGDataURI(), testPNGDataURI()}
	annotations, err := adapter.uploadImages(context.Background(), "wss://substrate.office.com/m365Copilot/Chathub/id@tenant?access_token=secret-test-token", "conversation-1", images)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(annotations) != 2 {
		t.Fatalf("calls=%d annotations=%d, want 2 each", calls, len(annotations))
	}
	for i, annotation := range annotations {
		if annotation.ID != fmt.Sprintf("doc-%d", i+1) || annotation.MessageAnnotationType != "ImageFile" || annotation.MessageAnnotationMetadata.Type != "File" || annotation.MessageAnnotationMetadata.Annotation != "File" || annotation.MessageAnnotationMetadata.FileType != "png" || !strings.HasSuffix(annotation.MessageAnnotationMetadata.FileName, ".png") {
			t.Fatalf("unexpected annotation %d: %+v", i, annotation)
		}
	}
}

func TestUploadImagesRejectsUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upload unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	adapter := New(nil)
	adapter.UploadURL = server.URL
	if annotations, err := adapter.uploadImages(context.Background(), "wss://substrate.office.com/path?access_token=token", "conversation-1", []string{testPNGDataURI()}); err == nil || annotations != nil {
		t.Fatalf("expected upload error, annotations=%v err=%v", annotations, err)
	}
}

func TestGenerateAttachesUploadedImageAnnotations(t *testing.T) {
	var gotMessage map[string]any
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{}`+recordSeparator))
		_, request, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var invocation map[string]any
		for _, raw := range strings.Split(string(request), recordSeparator) {
			if json.Unmarshal([]byte(raw), &invocation) == nil && invocation["type"] != nil {
				break
			}
		}
		args := invocation["arguments"].([]any)
		gotMessage = args[0].(map[string]any)["message"].(map[string]any)
		frame := `{"type":2,"item":{"result":{"value":"Success"},"messages":[{"author":"bot","text":"I can see the attached image."}]}}` + recordSeparator
		_ = conn.WriteMessage(websocket.TextMessage, []byte(frame))
	}))
	defer wsServer.Close()

	uploadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.Form.Get("scenario") != "UploadImage" {
			t.Errorf("scenario=%q", r.Form.Get("scenario"))
		}
		_, _ = w.Write([]byte(`{"docId":"uploaded-image-1"}`))
	}))
	defer uploadServer.Close()

	adapter := New(nil)
	adapter.UploadURL = uploadServer.URL
	adapter.HTTPClient = uploadServer.Client()
	sess := Session{}
	account := Account{TokenURL: "ws://" + strings.TrimPrefix(wsServer.URL, "http://") + "/chathub?access_token=test-token"}
	answer, err := adapter.generate(context.Background(), account, &sess, "Describe the image", "gpt-5.6", []string{testPNGDataURI()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "I can see the attached image." {
		t.Fatalf("unexpected answer %q", answer)
	}
	annotations, ok := gotMessage["messageAnnotations"].([]any)
	if !ok || len(annotations) != 1 {
		t.Fatalf("messageAnnotations=%#v", gotMessage["messageAnnotations"])
	}
	annotation := annotations[0].(map[string]any)
	if annotation["id"] != "uploaded-image-1" || annotation["messageAnnotationType"] != "ImageFile" {
		t.Fatalf("unexpected uploaded-image annotation: %#v", annotation)
	}
	if _, exists := gotMessage["imageUrl"]; exists {
		t.Fatal("chat message should reference uploaded file annotation, not inline imageUrl")
	}
}
