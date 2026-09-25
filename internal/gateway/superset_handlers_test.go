package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sever_provider/internal/filestore"
	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

type imageMock struct{}

func (imageMock) Name() string { return "mock" }
func (imageMock) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityImage, provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilitySpeech}
}
func (imageMock) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	return provider.ChatResponse{ID: "id", Model: "mock", Content: "ok"}, nil
}
func (imageMock) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}
func (imageMock) Speech(context.Context, provider.SpeechRequest, []byte) ([]byte, string, error) {
	return []byte("audio"), "audio/wav", nil
}
func (imageMock) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{Data: []byte("png")}, nil
}

type sourceMock struct{}

func (sourceMock) List(context.Context, string) ([]identity.Credential, error) {
	return []identity.Credential{{ID: "c", Domain: "mock", Status: identity.StatusHealthy, MaxConcurrency: 1}}, nil
}
func (sourceMock) ReadSecret(context.Context, string) ([]byte, error) { return nil, nil }

func TestImageGenerationStoresMIMEAwareFile(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "dall-e-3", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 5})
	adapters := provider.NewRegistry()
	adapters.Register(imageMIME{imageMock{}})
	leases := scheduler.NewLeaseManager()
	dispatcher := &scheduler.Dispatcher{Models: models, Credentials: sourceMock{}, Adapters: adapters, Leases: leases}
	h := NewHandler(dispatcher, models)
	store, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.SetFiles(store)
	r := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"dall-e-3","prompt":"a blue circle"}`))
	h.Images(r, req)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d", r.Code)
	}
	if !strings.Contains(r.Body.String(), "/v1/files/") {
		t.Fatalf("body=%s", r.Body.String())
	}
}

type imageMIME struct{ imageMock }

func (imageMIME) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{Data: []byte("fake-jpeg"), MIME: "image/jpeg"}, nil
}

func TestImageGenerationRejectsUnsupportedOptions(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "mock", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 5})
	adapters := provider.NewRegistry()
	adapters.Register(imageMock{})
	h := NewHandler(&scheduler.Dispatcher{Models: models, Credentials: sourceMock{}, Adapters: adapters, Leases: scheduler.NewLeaseManager()}, models)
	for _, body := range []string{
		`{"model":"mock","prompt":"test","size":"1024x1024"}`,
		`{"model":"mock","prompt":"test","n":2}`,
		`{"model":"mock","prompt":"test","aspect_ratio":"16:9"}`,
		`{"model":"mock","prompt":"test","style":"vivid"}`,
		`{"model":"mock","prompt":"test","response_format":"binary"}`,
	} {
		r := httptest.NewRecorder()
		h.Images(r, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))
		if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "unsupported_option") {
			t.Fatalf("request %s: status=%d body=%s", body, r.Code, r.Body.String())
		}
	}
}

func TestImageGenerationBase64Response(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "mock", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 5})
	adapters := provider.NewRegistry()
	adapters.Register(imageMock{})
	h := NewHandler(&scheduler.Dispatcher{Models: models, Credentials: sourceMock{}, Adapters: adapters, Leases: scheduler.NewLeaseManager()}, models)
	r := httptest.NewRecorder()
	h.Images(r, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"mock","prompt":"test","response_format":"b64_json"}`)))
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"b64_json":"cG5n"`) {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestSupersetRoutesExposeAsyncContracts(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "mock", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilitySpeech, provider.CapabilityImage}, TimeoutSec: 5})
	adapters := provider.NewRegistry()
	adapters.Register(imageMock{})
	leases := scheduler.NewLeaseManager()
	dispatcher := &scheduler.Dispatcher{Models: models, Credentials: sourceMock{}, Adapters: adapters, Leases: leases}
	h := NewHandler(dispatcher, models)
	r := httptest.NewServer(NewRouter(h, ""))
	defer r.Close()
	for _, endpoint := range []string{"/api/tts/voices", "/v1/models"} {
		res, err := http.Get(r.URL + endpoint)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", endpoint, res.StatusCode)
		}
	}
}

func TestTTSJobPathUsesJobID(t *testing.T) {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{ID: "mock", Provider: "mock", Capabilities: []provider.Capability{provider.CapabilitySpeech}, TimeoutSec: 5})
	h := NewHandler(nil, models)
	r := httptest.NewRequest(http.MethodPost, "/api/tts/generate", strings.NewReader(`{"text":"hi","voice_id":"ngochuyennew"}`))
	w := httptest.NewRecorder()
	h.TTSGenerate(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422 for unconfigured TTS model", w.Code)
	}
}
