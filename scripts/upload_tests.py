import os
import paramiko

HOST = os.environ.get('SSH_HOST', '')
PORT = int(os.environ.get('SSH_PORT', '22'))
USER = os.environ.get('SSH_USER', '')
KEY_PATH = os.environ.get('SSH_KEY_PATH', '')
REMOTE_BASE = os.environ.get('REMOTE_BASE', '/media/reup/Data_sv4/sever_provider')

TEST_FILES = {}

TEST_FILES['internal/secret/store_test.go'] = r'''package secret

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestStoreEncryptDecrypt(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	hexKey := hex.EncodeToString(key)

	store, err := NewStore(hexKey)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	plaintext := []byte("confidential-gemini-cookie-and-token")
	payload, nonce, err := store.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	decrypted, err := store.Decrypt(payload, nonce)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Fatalf("expected %s, got %s", plaintext, decrypted)
	}
}

func TestStoreEphemeralKey(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore ephemeral failed: %v", err)
	}

	data := []byte("test-data")
	payload, nonce, err := store.Encrypt(data)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	decrypted, err := store.Decrypt(payload, nonce)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(data, decrypted) {
		t.Fatalf("expected %s, got %s", data, decrypted)
	}
}
'''

TEST_FILES['internal/scheduler/errors_test.go'] = r'''package scheduler

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		status   int
		body     string
		retryHdr string
		expected ErrorKind
	}{
		{429, "Too Many Requests", "10", ErrRateLimited},
		{429, "insufficient_quota exceeded", "", ErrQuotaExhausted},
		{401, "Unauthorized token", "", ErrAuthExpired},
		{403, "Forbidden resource", "", ErrAuthForbidden},
		{502, "Bad Gateway", "", ErrProviderDown},
		{504, "Gateway Timeout", "", ErrUpstreamTimeout},
		{400, "Bad Request syntax", "", ErrBadRequest},
	}

	for _, tc := range tests {
		res := Classify(tc.status, tc.body, tc.retryHdr)
		if res.Kind != tc.expected {
			t.Errorf("status %d body %q: expected kind %s, got %s", tc.status, tc.body, tc.expected, res.Kind)
		}
	}

	res := Classify(429, "rate limited", "45")
	if res.RetryAfter != 45*time.Second {
		t.Errorf("expected 45s retry-after, got %v", res.RetryAfter)
	}
}
'''

TEST_FILES['internal/scheduler/circuit_test.go'] = r'''package scheduler

import (
	"testing"
	"time"
)

func TestAccountCircuitBreaker(t *testing.T) {
	cb := NewAccountCircuitBreaker()
	credID := "cred-test-1"

	if !cb.IsAvailable(credID) {
		t.Fatal("expected available initially")
	}

	err := CanonicalError{Kind: ErrRateLimited, RetryAfter: 100 * time.Millisecond}
	cb.RecordFailure(credID, err)

	if cb.IsAvailable(credID) {
		t.Fatal("expected cooling down after failure")
	}

	time.Sleep(150 * time.Millisecond)
	if !cb.IsAvailable(credID) {
		t.Fatal("expected available after cooldown passed")
	}

	cb.RecordSuccess(credID)
	if !cb.IsAvailable(credID) {
		t.Fatal("expected available after success")
	}
}

func TestProviderCircuitBreaker(t *testing.T) {
	pb := NewProviderCircuitBreaker()
	prov := "gemini_web"

	if !pb.CanAttempt(prov) {
		t.Fatal("expected closed initially")
	}

	pb.RecordFailure(prov, CanonicalError{Kind: ErrProviderDown})
	if pb.CanAttempt(prov) {
		t.Fatal("expected open after provider down")
	}

	pb.mu.Lock()
	pb.breakers[prov].OpenUntil = time.Now().Add(-1 * time.Second)
	pb.mu.Unlock()

	if !pb.CanAttempt(prov) {
		t.Fatal("expected half-open after timeout")
	}

	pb.RecordSuccess(prov)
	if !pb.CanAttempt(prov) {
		t.Fatal("expected closed after success")
	}
}
'''

TEST_FILES['internal/identity/manager_test.go'] = r'''package identity

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"sever_provider/internal/db"
	"sever_provider/internal/secret"
)

func TestIdentityAndCredentialFlow(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	database, err := db.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	defer database.Close()
	defer os.Remove(dbPath)

	store, err := secret.NewStore("")
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	mgr := NewManager(database, store)
	ctx := context.Background()

	err = mgr.CreateIdentity(ctx, "id-01", "gemini_pool", "test@gmail.com", 5)
	if err != nil {
		t.Fatalf("CreateIdentity failed: %v", err)
	}

	secretData := []byte(`{"cookie":"session_token_123"}`)
	err = mgr.CreateCredential(ctx, "cred-01", "id-01", "gemini_web", 2, secretData)
	if err != nil {
		t.Fatalf("CreateCredential failed: %v", err)
	}

	decrypted, err := mgr.GetSecret(ctx, "cred-01")
	if err != nil {
		t.Fatalf("GetSecret failed: %v", err)
	}
	if string(decrypted) != string(secretData) {
		t.Fatalf("expected secret %s, got %s", secretData, decrypted)
	}

	creds, err := mgr.GetHealthyCredentials(ctx, "gemini_web")
	if err != nil {
		t.Fatalf("GetHealthyCredentials failed: %v", err)
	}
	if len(creds) != 1 || creds[0].ID != "cred-01" {
		t.Fatalf("unexpected healthy creds: %v", creds)
	}

	details, err := mgr.ListIdentitiesWithCredentials(ctx)
	if err != nil {
		t.Fatalf("ListIdentitiesWithCredentials failed: %v", err)
	}
	if len(details) != 1 || len(details[0].Credentials) != 1 {
		t.Fatalf("unexpected details: %v", details)
	}
}
'''

TEST_FILES['internal/gateway/handlers_test.go'] = r'''package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"sever_provider/internal/config"
	"sever_provider/internal/db"
	"sever_provider/internal/identity"
	"sever_provider/internal/jobs"
	"sever_provider/internal/observe"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/secret"
)

func setupTestRouter(t *testing.T) http.Handler {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "gw_test.db")

	database, err := db.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.Close(); os.Remove(dbPath) })

	store, _ := secret.NewStore("")
	idMgr := identity.NewManager(database, store)
	circuitMgr := scheduler.NewCircuitManager()
	leaseMgr := scheduler.NewLeaseManager(database, idMgr, circuitMgr)
	queue := jobs.NewJobQueue(50, 2)
	t.Cleanup(func() { queue.Close() })
	logger := observe.NewLogger()
	metrics := observe.NewMetrics(database)

	reg := provider.NewRegistry()
	reg.Register(provider.NewGeminiWebProvider())
	reg.Register(provider.NewPiperProvider("piper", ""))

	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8080},
		Models: map[string]config.ModelConfig{
			"gemini-3.8-flash": {
				Provider:     "gemini_web",
				Capabilities: []string{"chat", "streaming"},
				TimeoutSec:   5,
			},
			"piper:test": {
				Provider:     "piper_local",
				Capabilities: []string{"audio_speech"},
				OutputFormat: "audio/wav",
			},
		},
	}

	_ = idMgr.CreateIdentity(context.Background(), "id-test", "gemini_pool", "test@domain.com", 1)
	_ = idMgr.CreateCredential(context.Background(), "cred-test", "id-test", "gemini_web", 2, nil)

	sched := scheduler.NewScheduler(cfg, idMgr, leaseMgr, circuitMgr, reg, queue, logger, metrics)
	handlers := NewHandlers(sched, idMgr, metrics, logger)
	return NewRouter(handlers, logger, metrics, "")
}

func TestHealthEndpoint(t *testing.T) {
	router := setupTestRouter(t)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal health response: %v", err)
	}
	if resp["status"] != "healthy" {
		t.Fatalf("expected status healthy, got %v", resp["status"])
	}
}

func TestListModelsEndpoint(t *testing.T) {
	router := setupTestRouter(t)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp ModelListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatalf("expected models, got 0")
	}
}

func TestChatCompletionsNonStream(t *testing.T) {
	router := setupTestRouter(t)

	body := []byte(`{
		"model": "gemini-3.8-flash",
		"messages": [{"role": "user", "content": "Hello AI Gateway"}],
		"stream": false
	}`)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal chat response: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		t.Fatalf("empty content in response: %v", resp)
	}
}

func TestAudioSpeechEndpoint(t *testing.T) {
	router := setupTestRouter(t)

	body := []byte(`{
		"model": "piper:test",
		"input": "Xin chao the gioi",
		"voice": "default"
	}`)

	req := httptest.NewRequest("POST", "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "audio/wav" {
		t.Fatalf("expected audio/wav, got %s", contentType)
	}

	if w.Body.Len() < 44 {
		t.Fatalf("WAV output too short: %d bytes", w.Body.Len())
	}
}
'''

def main():
    if not HOST or not USER or not KEY_PATH:
        raise RuntimeError('set SSH_HOST, SSH_USER, and SSH_KEY_PATH environment variables')
    client = paramiko.SSHClient()
    client.load_system_host_keys()
    client.set_missing_host_key_policy(paramiko.RejectPolicy())
    client.connect(HOST, port=PORT, username=USER, key_filename=KEY_PATH, timeout=10)
    sftp = client.open_sftp()

    for rel_path, content in TEST_FILES.items():
        full_path = REMOTE_BASE + '/' + rel_path
        print(f"Uploading {rel_path}...")
        with sftp.file(full_path, 'wb') as f:
            f.write(content.encode('utf-8'))

    sftp.close()
    client.close()
    print("Test files uploaded.")

if __name__ == '__main__':
    main()
