package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"sever_provider/internal/admin"
	"sever_provider/internal/filestore"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

func newImageTestHandler(t *testing.T, specs []scheduler.ModelSpec, adapter provider.Adapter, files *filestore.Store) *Handler {
	t.Helper()
	models := scheduler.NewModelRegistry()
	for _, spec := range specs {
		if err := models.Register(spec); err != nil {
			t.Fatal(err)
		}
	}
	adapters := provider.NewRegistry()
	adapters.Register(adapter)
	h := NewHandler(&scheduler.Dispatcher{Models: models, Credentials: sourceMock{}, Adapters: adapters, Leases: scheduler.NewLeaseManager()}, models)
	if files != nil {
		h.SetFiles(files)
	}
	return h
}

func newImageStore(t *testing.T) *filestore.Store {
	t.Helper()
	store, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func postImage(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRecorder()
	h.Images(r, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))
	return r
}

func TestImageGenerationDefaultReturnsBase64AndURL(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "mock", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 5}}, imageMock{}, newImageStore(t))
	r := postImage(t, h, `{"model":"mock","prompt":"a blue circle"}`)
	body := r.Body.String()
	if r.Code != http.StatusOK || !strings.Contains(body, `"b64_json":"cG5n"`) || !strings.Contains(body, `"url":"/v1/files/`) {
		t.Fatalf("status=%d body=%s", r.Code, body)
	}
}

func TestImageGenerationURLOnly(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "mock", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 5}}, imageMock{}, newImageStore(t))
	r := postImage(t, h, `{"model":"mock","prompt":"a blue circle","response_format":"url"}`)
	body := r.Body.String()
	if r.Code != http.StatusOK || !strings.Contains(body, `"url":"/v1/files/`) || strings.Contains(body, "b64_json") {
		t.Fatalf("status=%d body=%s", r.Code, body)
	}
}

type imageURLMock struct{ imageMock }

func (imageURLMock) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{URL: "https://cdn.example/x.png"}, nil
}

func TestImageGenerationProviderURLOnly(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "mock", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 5}}, imageURLMock{}, nil)
	r := postImage(t, h, `{"model":"mock","prompt":"a blue circle"}`)
	body := r.Body.String()
	if r.Code != http.StatusOK || !strings.Contains(body, `"url":"https://cdn.example/x.png"`) || strings.Contains(body, "b64_json") {
		t.Fatalf("status=%d body=%s", r.Code, body)
	}
}

func TestImageGenerationSaveFailureStillBase64(t *testing.T) {
	store := newImageStore(t)
	if err := os.RemoveAll(store.Root()); err != nil {
		t.Fatal(err)
	}
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "mock", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 5}}, imageMock{}, store)
	r := postImage(t, h, `{"model":"mock","prompt":"a blue circle"}`)
	body := r.Body.String()
	if r.Code != http.StatusOK || !strings.Contains(body, `"b64_json":"cG5n"`) || strings.Contains(body, "/v1/files/") {
		t.Fatalf("status=%d body=%s", r.Code, body)
	}
}

type oversizeImageMock struct{ imageMock }

func (oversizeImageMock) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{Data: make([]byte, maxGeneratedImageBytes+1)}, nil
}

func TestImageGenerationOversizeRejected(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "mock", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 5}}, oversizeImageMock{}, nil)
	r := postImage(t, h, `{"model":"mock","prompt":"a blue circle"}`)
	if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "image_too_large") {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestImageGenerationNonImageModelReturns400(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "chat-only", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 5}}, imageMock{}, nil)
	r := postImage(t, h, `{"model":"chat-only","prompt":"a blue circle"}`)
	if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "unsupported_capability") {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

type countingChatMock struct {
	imageMock
	calls int
	data  []byte
}

func (m *countingChatMock) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	m.calls++
	return provider.ChatResponse{ID: "id", Model: "mock", Content: "here it is", ImageData: m.data, ImageMIME: "image/png"}, nil
}

func postChat(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRecorder()
	h.ChatCompletions(r, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return r
}

func TestChatImageGenerationRejectsNonImageModel(t *testing.T) {
	mock := &countingChatMock{}
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "chat-only", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 5}}, mock, nil)
	r := postChat(t, h, `{"model":"chat-only","messages":[{"role":"user","content":"draw a cat"}],"image_generation":true}`)
	if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "unsupported_capability") {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	if mock.calls != 0 {
		t.Fatalf("adapter Chat called %d times for a rejected request", mock.calls)
	}
}

func TestChatImageGenerationStreams(t *testing.T) {
	store := newImageStore(t)
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "img", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5}}, &countingChatMock{data: []byte("png")}, store)
	r := postChat(t, h, `{"model":"img","messages":[{"role":"user","content":"draw a cat"}],"image_generation":true,"stream":true}`)
	body := r.Body.String()
	if r.Code != http.StatusOK || r.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d content-type=%q body=%s", r.Code, r.Header().Get("Content-Type"), body)
	}
	if !strings.Contains(body, "data:image/png;base64,cG5n") || !strings.Contains(body, "image_url") || !strings.Contains(body, "/v1/files/") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream body=%s", body)
	}
}

func TestChatImageGenerationRequiresPrompt(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "img", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5}}, &countingChatMock{}, nil)
	r := postChat(t, h, `{"model":"img","messages":[{"role":"user","content":"  "}],"image_generation":true}`)
	if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "missing_parameter") {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestChatImageGenerationReturnsURL(t *testing.T) {
	store := newImageStore(t)
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "img", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5}}, &countingChatMock{data: []byte("png")}, store)
	r := postChat(t, h, `{"model":"img","messages":[{"role":"user","content":"draw a cat"}],"image_generation":true}`)
	body := r.Body.String()
	if r.Code != http.StatusOK || !strings.Contains(body, "/v1/files/") {
		t.Fatalf("status=%d body=%s", r.Code, body)
	}
	var payload ChatCompletionResponse
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Choices) == 0 || payload.Choices[0].Message == nil || len(payload.Choices[0].Message.Images) != 1 {
		t.Fatalf("chat response missing message.images: %s", body)
	}
	imageURL := payload.Choices[0].Message.Images[0].ImageURL
	if payload.Choices[0].Message.Images[0].Type != "image_url" || imageURL == nil || imageURL.URL != "data:image/png;base64,cG5n" {
		t.Fatalf("unexpected image shape: %+v", payload.Choices[0].Message.Images[0])
	}
	entries, err := os.ReadDir(store.Root())
	if err != nil || len(entries) != 1 {
		t.Fatalf("stored files=%d err=%v", len(entries), err)
	}
}

func TestChatImageGenerationModalitiesTrigger(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "img", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5}}, &countingChatMock{data: []byte("png")}, newImageStore(t))
	r := postChat(t, h, `{"model":"img","messages":[{"role":"user","content":"draw a cat"}],"modalities":["text","image"]}`)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "data:image/png;base64,cG5n") {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestChatImageGenerationToolTrigger(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "img", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5}}, &countingChatMock{data: []byte("png")}, newImageStore(t))
	r := postChat(t, h, `{"model":"img","messages":[{"role":"user","content":"draw a cat"}],"tools":[{"type":"image_generation"}]}`)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "data:image/png;base64,cG5n") {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestChatImageStoreFailure(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{{ID: "img", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityImage}, TimeoutSec: 5}}, &countingChatMock{data: []byte("png")}, nil)
	r := postChat(t, h, `{"model":"img","messages":[{"role":"user","content":"draw a cat"}],"image_generation":true}`)
	if r.Code != http.StatusServiceUnavailable || !strings.Contains(r.Body.String(), "image_storage_unavailable") {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestModelsListExposesCapabilities(t *testing.T) {
	h := newImageTestHandler(t, []scheduler.ModelSpec{
		{ID: "img", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 5},
		{ID: "chat-only", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 5},
	}, imageMock{}, nil)
	r := httptest.NewRecorder()
	h.ModelsList(r, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var payload struct {
		Data []struct {
			ID           string   `json:"id"`
			Capabilities []string `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	seen := map[string][]string{}
	for _, item := range payload.Data {
		seen[item.ID] = item.Capabilities
	}
	if !strings.Contains(strings.Join(seen["img"], ","), "image_generation") {
		t.Fatalf("img capabilities=%v", seen["img"])
	}
	if strings.Contains(strings.Join(seen["chat-only"], ","), "image_generation") {
		t.Fatalf("chat-only capabilities=%v", seen["chat-only"])
	}
}

func TestMaskProviderErrorImageBytesAndCapability(t *testing.T) {
	if status, _, code := maskProviderError(fmt.Errorf("wrapped: %w", provider.ErrImageBytesUnavailable)); status != http.StatusBadGateway || code != "image_bytes_unavailable" {
		t.Fatalf("image bytes: status=%d code=%s", status, code)
	}
	if status, _, code := maskProviderError(provider.ErrCapabilityUnsupported); status != http.StatusBadRequest || code != "unsupported_capability" {
		t.Fatalf("capability: status=%d code=%s", status, code)
	}
}

func TestFileScopeAllowsImagesAndVideo(t *testing.T) {
	if got := scopeForPath("/v1/files/abc.png"); got != "images|video" {
		t.Fatalf("scope=%q", got)
	}
	db := setupTestDBForGateway(t)
	keys := admin.NewAPIKeys(db)
	_, imageKey, err := keys.CreateWithOptions(admin.CreateKeySpec{Name: "images", Scopes: []string{"images"}})
	if err != nil {
		t.Fatal(err)
	}
	_, videoKey, err := keys.CreateWithOptions(admin.CreateKeySpec{Name: "video", Scopes: []string{"video"}})
	if err != nil {
		t.Fatal(err)
	}
	_, chatKey, err := keys.CreateWithOptions(admin.CreateKeySpec{Name: "chat", Scopes: []string{"chat"}})
	if err != nil {
		t.Fatal(err)
	}
	store := newImageStore(t)
	id, err := store.Save([]byte("png"), ".png")
	if err != nil {
		t.Fatal(err)
	}
	h := newImageTestHandler(t, nil, imageMock{}, store)
	h.SetAPIKeys(keys)
	server := httptest.NewServer(NewRouter(h, ""))
	defer server.Close()

	get := func(key string) int {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/files/"+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if status := get(imageKey); status != http.StatusOK {
		t.Fatalf("images key status=%d", status)
	}
	if status := get(videoKey); status != http.StatusOK {
		t.Fatalf("video key status=%d", status)
	}
	if status := get(chatKey); status != http.StatusForbidden {
		t.Fatalf("chat key status=%d", status)
	}
	if _, err := keys.AuthenticateWithScope(imageKey, ""); err != nil {
		t.Fatalf("empty scope must bypass: %v", err)
	}
}
