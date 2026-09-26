package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/admin"
	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/workerhub"
)

type batchTestSpeechAdapter struct {
	mu        sync.Mutex
	callCount int
	failInput string
}

func (a *batchTestSpeechAdapter) Name() string { return "piper_local" }
func (a *batchTestSpeechAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilitySpeech}
}
func (a *batchTestSpeechAdapter) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, nil
}
func (a *batchTestSpeechAdapter) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	return nil, nil
}
func (a *batchTestSpeechAdapter) Speech(_ context.Context, req provider.SpeechRequest, _ []byte) ([]byte, string, error) {
	a.mu.Lock()
	a.callCount++
	a.mu.Unlock()
	if a.failInput != "" && req.Input == a.failInput {
		return nil, "", errors.New("synthetic speech failure for: " + req.Input)
	}
	wav := []byte("RIFF-WAV-" + req.Input)
	return wav, "audio/wav", nil
}
func (a *batchTestSpeechAdapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, nil
}
func (a *batchTestSpeechAdapter) Voices() []piper.Voice {
	return []piper.Voice{
		{ID: "voice-a", ModelPath: "/mock/voice-a.onnx"},
		{ID: "voice-b", ModelPath: "/mock/voice-b.onnx"},
	}
}

func setupBatchTestHandler(adapter *batchTestSpeechAdapter) *Handler {
	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{
		ID:           "piper:voice-a",
		Provider:     "piper_local",
		Capabilities: []provider.Capability{provider.CapabilitySpeech},
		TimeoutSec:   30,
	})
	_ = models.Register(scheduler.ModelSpec{
		ID:           "piper:voice-b",
		Provider:     "piper_local",
		Capabilities: []provider.Capability{provider.CapabilitySpeech},
		TimeoutSec:   30,
	})
	adapters := provider.NewRegistry()
	if adapter != nil {
		adapters.Register(adapter)
	}
	creds := scheduler.NewMemoryCredentials()
	creds.Add("piper_local", identity.Credential{
		ID:             "cred-piper",
		Domain:         "piper_local",
		Status:         identity.StatusHealthy,
		MaxConcurrency: 100,
	}, nil)
	leases := scheduler.NewLeaseManager()
	dispatcher := &scheduler.Dispatcher{
		Models:      models,
		Adapters:    adapters,
		Credentials: creds,
		Leases:      leases,
	}
	return NewHandler(dispatcher, models)
}

func TestBatchAudioSpeech_Format1_Inputs(t *testing.T) {
	adapter := &batchTestSpeechAdapter{}
	h := setupBatchTestHandler(adapter)

	body := `{"model":"piper:voice-a","inputs":["câu 1","câu 2","câu 3"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech/batch", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.BatchAudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

		var resp BatchSpeechResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response array: %v", err)
		}
		results := resp.Results

		if len(results) != 3 {
			t.Fatalf("expected 3 results, got %d", len(results))
		}

		for i, r := range results {
			expectedID := fmt.Sprintf("%d", i)
			expectedAudio := base64.StdEncoding.EncodeToString([]byte("RIFF-WAV-câu " + fmt.Sprintf("%d", i+1)))
			if r.ID != expectedID {
				t.Errorf("item %d: expected id %q, got %q", i, expectedID, r.ID)
			}
			if r.Index != i {
				t.Errorf("item %d: expected index %d, got %d", i, i, r.Index)
			}
			if r.Status != "ok" {
				t.Errorf("item %d: expected status ok, got %q", i, r.Status)
			}
		if r.AudioBase64 != expectedAudio {
			t.Errorf("item %d: unexpected audio_base64", i)
		}
		if r.B64JSON != expectedAudio {
			t.Errorf("item %d: unexpected b64_json", i)
		}
		if r.ContentType != "audio/wav" {
			t.Errorf("item %d: expected content_type audio/wav, got %q", i, r.ContentType)
		}
	}
}

func TestBatchAudioSpeech_Format2_Items(t *testing.T) {
	adapter := &batchTestSpeechAdapter{}
	h := setupBatchTestHandler(adapter)

	body := `{
		"items": [
			{"id": "custom-1", "model": "piper:voice-a", "input": "câu a"},
			{"id": "custom-2", "model": "piper:voice-b", "input": "câu b"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech/batch", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.BatchAudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

		var resp BatchSpeechResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response array: %v", err)
		}
		results := resp.Results

		if len(results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(results))
		}

		if results[0].ID != "custom-1" || results[0].Index != 0 || results[0].Status != "ok" {
			t.Errorf("item 0 mismatch: %+v", results[0])
		}
		if results[1].ID != "custom-2" || results[1].Index != 1 || results[1].Status != "ok" {
			t.Errorf("item 1 mismatch: %+v", results[1])
		}
}

func TestBatchAudioSpeech_Format2_ItemInheritance(t *testing.T) {
	adapter := &batchTestSpeechAdapter{}
	h := setupBatchTestHandler(adapter)

	body := `{
		"model": "piper:voice-a",
		"voice": "voice-a",
		"items": [
			{"input": "text 1"},
			{"id": "item-2", "input": "text 2"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech/batch", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.BatchAudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

		var resp BatchSpeechResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		results := resp.Results

		if len(results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(results))
		}
		if results[0].ID != "0" || results[0].Status != "ok" {
			t.Errorf("item 0 failed inheritance: %+v", results[0])
		}
		if results[1].ID != "item-2" || results[1].Status != "ok" {
			t.Errorf("item 1 failed: %+v", results[1])
		}
}

func TestBatchAudioSpeech_MaxBatchSizeCalculation(t *testing.T) {
	h := setupBatchTestHandler(&batchTestSpeechAdapter{})

	// 1. No workers connected -> fallback slots (5) * 2 = 10
	if size := h.MaxBatchSize(); size != 10 {
		t.Errorf("expected default max batch size 10, got %d", size)
	}

	// 2. Hub with worker slots = 4 -> 4 * 2 = 8
	hub := workerhub.New(workerhub.Config{AuthToken: "secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(workerhub.Registration{
		WorkerID:       "w1",
		Model:          "piper:voice-a",
		MaxConcurrency: 4,
		Token:          "secret",
	}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	_ = conn.ReadJSON(&ack)

	h.SetWorkers(hub)

	if size := h.MaxBatchSize(); size != 8 {
		t.Errorf("expected max batch size 8 with 4 slots, got %d", size)
	}

	// 3. Worker with MaxConcurrency = 64 (clamped to maxWorkerSlots 32) -> 32 * 2 = 64
	conn2, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	if err := conn2.WriteJSON(workerhub.Registration{
		WorkerID:       "w2",
		Model:          "piper:voice-b",
		MaxConcurrency: 32,
		Token:          "secret",
	}); err != nil {
		t.Fatal(err)
	}
	_ = conn2.ReadJSON(&ack)

	// total slots = 4 + 32 = 36 -> 36 * 2 = 72
	if size := h.MaxBatchSize(); size != 72 {
		t.Errorf("expected max batch size 72 with 36 slots, got %d", size)
	}

	// Add more workers to trigger ceiling 100
	conn3, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn3.Close()
	if err := conn3.WriteJSON(workerhub.Registration{
		WorkerID:       "w3",
		Model:          "piper:voice-a",
		MaxConcurrency: 32,
		Token:          "secret",
	}); err != nil {
		t.Fatal(err)
	}
	_ = conn3.ReadJSON(&ack)

	// total slots = 4 + 32 + 32 = 68 -> 68 * 2 = 136 -> capped at 100
	if size := h.MaxBatchSize(); size != 100 {
		t.Errorf("expected safe ceiling 100, got %d", size)
	}
}

func TestBatchAudioSpeech_RejectsExceedingBatchSize(t *testing.T) {
	h := setupBatchTestHandler(&batchTestSpeechAdapter{})
	// Default max batch size is 10. Sending 11 items must fail.
	inputs := make([]string, 11)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("sentence %d", i)
	}
	payload, _ := json.Marshal(map[string]any{
		"model":  "piper:voice-a",
		"inputs": inputs,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech/batch", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()

	h.BatchAudioSpeech(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "batch_size_exceeded") {
		t.Errorf("expected batch_size_exceeded code, got %s", rec.Body.String())
	}
}

func TestBatchAudioSpeech_ValidationErrors(t *testing.T) {
	h := setupBatchTestHandler(&batchTestSpeechAdapter{})

	cases := []struct {
		name       string
		method     string
		body       string
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "method not allowed",
			method:     http.MethodGet,
			body:       "",
			wantStatus: http.StatusMethodNotAllowed,
			wantSubstr: "method not allowed",
		},
		{
			name:       "invalid JSON",
			method:     http.MethodPost,
			body:       "{invalid-json",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "invalid_json",
		},
		{
			name:       "missing inputs and items",
			method:     http.MethodPost,
			body:       `{"model":"piper:voice-a"}`,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "missing_parameter",
		},
		{
			name:       "empty inputs",
			method:     http.MethodPost,
			body:       `{"model":"piper:voice-a","inputs":[]}`,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "empty_batch",
		},
		{
			name:       "empty items",
			method:     http.MethodPost,
			body:       `{"items":[]}`,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "empty_batch",
		},
		{
			name:       "missing model in item",
			method:     http.MethodPost,
			body:       `{"items":[{"input":"text without model"}]}`,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "missing_parameter",
		},
		{
			name:       "empty text input",
			method:     http.MethodPost,
			body:       `{"model":"piper:voice-a","inputs":["valid", ""]}`,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "missing_parameter",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/v1/audio/speech/batch", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.BatchAudioSpeech(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantSubstr) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), tc.wantSubstr)
			}
		})
	}
}

func TestBatchAudioSpeech_PartialFailurePreservesOrder(t *testing.T) {
	adapter := &batchTestSpeechAdapter{failInput: "fail-this-sentence"}
	h := setupBatchTestHandler(adapter)

	body := `{
		"model": "piper:voice-a",
		"inputs": ["first sentence", "fail-this-sentence", "third sentence"]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech/batch", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.BatchAudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

		var resp BatchSpeechResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		results := resp.Results

		if len(results) != 3 {
			t.Fatalf("expected 3 results, got %d", len(results))
		}

		// Index 0: success -> "ok"
		if results[0].Index != 0 || results[0].Status != "ok" || results[0].AudioBase64 == "" {
			t.Errorf("item 0 unexpected: %+v", results[0])
		}
		// Index 1: error
		if results[1].Index != 1 || results[1].Status != "error" || results[1].Error == "" {
			t.Errorf("item 1 unexpected: %+v", results[1])
		}
		// Index 2: success -> "ok"
		if results[2].Index != 2 || results[2].Status != "ok" || results[2].AudioBase64 == "" {
			t.Errorf("item 2 unexpected: %+v", results[2])
		}
}

func TestBatchAudioSpeech_WorkerHubFanOut(t *testing.T) {
	hub := workerhub.New(workerhub.Config{AuthToken: "secret", HeartbeatInterval: time.Hour})
	defer hub.Close()
	server := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(workerhub.Registration{
		WorkerID:       "remote-tts",
		Model:          "piper:voice-fanout",
		MaxConcurrency: 4,
		Token:          "secret",
	}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}

	models := scheduler.NewModelRegistry()
	_ = models.Register(scheduler.ModelSpec{
		ID:           "piper:voice-fanout",
		Provider:     "piper_local",
		Capabilities: []provider.Capability{provider.CapabilitySpeech},
		TimeoutSec:   30,
	})
	dispatcher := &scheduler.Dispatcher{Models: models}
	h := NewHandler(dispatcher, models)
	h.SetWorkers(hub)

	// Goroutine on worker side to answer incoming speech requests
	var workerWg sync.WaitGroup
	workerWg.Add(1)
	go func() {
		defer workerWg.Done()
		for i := 0; i < 3; i++ {
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
				reqID, _ := request["request_id"].(string)
				text, _ := request["text"].(string)
				wav := append([]byte("RIFF\x00\x00\x00\x00WAVE"), []byte(text)...)
				_ = conn.WriteJSON(map[string]any{
				"type":         "audio_result",
				"request_id":   reqID,
				"mime":         "audio/wav",
				"audio_base64": base64.StdEncoding.EncodeToString(wav),
				"done":         true,
			})
		}
	}()

	body := `{"model":"piper:voice-fanout","inputs":["line A","line B","line C"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech/batch", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.BatchAudioSpeech(rec, req)
	workerWg.Wait()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

		var resp BatchSpeechResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode: %v", err)
		}
		results := resp.Results

		if len(results) != 3 {
			t.Fatalf("expected 3 results, got %d", len(results))
		}
		for i, r := range results {
			if r.Index != i || r.Status != "ok" {
				t.Errorf("result %d error: %+v", i, r)
			}
		}
}

func TestBatchAudioSpeech_RouterAndAuth(t *testing.T) {
	adapter := &batchTestSpeechAdapter{}
	h := setupBatchTestHandler(adapter)

	dbConn := setupTestDBForGateway(t)
	apiKeys := admin.NewAPIKeys(dbConn)
	h.APIKeys = apiKeys

	// Create scoped key for audio_speech
	_, rawKey, err := apiKeys.CreateWithOptions(admin.CreateKeySpec{
		Name:   "speech-client",
		Scopes: []string{"audio_speech"},
	})
	if err != nil {
		t.Fatal(err)
	}

	router := NewRouter(h, "")

	// 1. Without API key -> 401 Unauthorized
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech/batch", strings.NewReader(`{"model":"piper:voice-a","inputs":["test"]}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without key, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. With scoped API key -> 200 OK
	req = httptest.NewRequest(http.MethodPost, "/v1/audio/speech/batch", strings.NewReader(`{"model":"piper:voice-a","inputs":["test"]}`))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with scoped key, got %d: %s", rec.Code, rec.Body.String())
	}
}
