package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func modelFixture(t *testing.T, dir, id, config string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, id+".onnx"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, id+".onnx.json"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

const validConfig = `{"phoneme_type":"espeak","phoneme_id_map":{"a":[1]},"audio":{"sample_rate":22050}}`

func TestDiscoverModels(t *testing.T) {
	dir := t.TempDir()
	modelFixture(t, dir, "valid", validConfig)
	modelFixture(t, dir, "missing", "")
	modelFixture(t, dir, "invalid", `{"phoneme_type":"unknown"}`)
	modelFixture(t, dir, "khmer_tts_v3", `{"vocab":["x"],"sample_rate":22050}`)
	modelFixture(t, dir, "thai", `{"phoneme_type":"text","phoneme_id_map":{"ก":[1]},"audio":{"sample_rate":22050}}`)
	found, err := discoverModels(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found["valid"] == "" || found["thai"] == "" {
		t.Fatalf("unexpected voices: %v", found)
	}
	if _, ok := found["khmer_tts_v3"]; ok {
		t.Fatal("Khmer model without Piper metadata was advertised")
	}
	if _, err := discoverModels(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("missing model directory accepted")
	}
}

// Built once per test: a real subprocess that can emit valid, malformed, or
// oversized WAV and can block until CommandContext kills it.
func fakePiper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := `package main
import("encoding/binary";"os";"strings";"time")
func main(){ var out string; for i:=1;i<len(os.Args)-1;i++ { if os.Args[i]=="--output_file" {out=os.Args[i+1]} }; if out=="" {os.Exit(2)}; model:=""; for i:=1;i<len(os.Args)-1;i++ {if os.Args[i]=="--model" {model=os.Args[i+1]}}; if strings.Contains(model,"sleep") {time.Sleep(10*time.Second)}; if strings.Contains(model,"fail") {os.Exit(3)}; if strings.Contains(model,"oversize") {f,_:=os.Create(out);f.Truncate(20<<20);f.Close();return}; if strings.Contains(model,"bad") {os.WriteFile(out,[]byte("not-wav"),0600);return}; data:=make([]byte,44);copy(data,"RIFF");binary.LittleEndian.PutUint32(data[4:8],36);copy(data[8:],"WAVE");os.WriteFile(out,data,0600)}`
	src := filepath.Join(dir, "fake.go")
	if err := os.WriteFile(src, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-piper")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, src)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake Piper: %v %s", err, output)
	}
	return bin
}

func TestSynthesizeBoundsAndContext(t *testing.T) {
	bin := fakePiper(t)
	for _, tc := range []struct {
		name      string
		wantError bool
	}{{"ok", false}, {"bad", true}, {"oversize", true}, {"fail", true}} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := synthesize(context.Background(), bin, tc.name+".onnx", "hello")
			if (err != nil) != tc.wantError {
				t.Fatalf("data=%d err=%v", len(data), err)
			}
			if err == nil && (len(data) != 44 || string(data[:4]) != "RIFF") {
				t.Fatal("invalid WAV")
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := synthesize(ctx, bin, "sleep.onnx", "text")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation: %v, duration %v", err, time.Since(start))
	}
}

func TestSessionAuthAndRouting(t *testing.T) {
	bin := fakePiper(t)
	dir := t.TempDir()
	modelFixture(t, dir, "voice", validConfig)
	modelFixture(t, dir, "khmer_tts_v3", `{"vocab":["x"]}`)
	var mu sync.Mutex
	var header, query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		header = r.Header.Get("X-Worker-Token")
		query = r.URL.RawQuery
		mu.Unlock()
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var reg struct {
			Token          string   `json:"token"`
			Models         []string `json:"models"`
			MaxConcurrency int      `json:"max_concurrency"`
		}
		if err := conn.ReadJSON(&reg); err != nil {
			t.Errorf("registration: %v", err)
			return
		}
		if reg.Token != "secret" || len(reg.Models) != 1 || reg.Models[0] != "piper:voice" || reg.MaxConcurrency != 1 {
			t.Errorf("registration: %+v", reg)
		}
		if err := conn.WriteJSON(map[string]string{"type": "registered"}); err != nil {
			t.Error(err)
			return
		}
		for _, req := range []request{
			{Type: "chat_request", RequestID: "bad", Model: "piper:khmer_tts_v3", Text: "x"},
			{Type: "tts_request", RequestID: "mismatch", Model: "piper:voice", VoiceID: "other", Text: "x"},
			{Type: "tts_request", RequestID: "ok", Model: "piper:voice", VoiceID: "voice", Text: "x"},
		} {
			if err := conn.WriteJSON(req); err != nil {
				t.Error(err)
				return
			}
		}
		received := map[string]map[string]any{}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for len(received) < 3 {
			var event map[string]any
			if err := conn.ReadJSON(&event); err != nil {
				t.Errorf("read event: %v", err)
				return
			}
			id, _ := event["request_id"].(string)
			received[id] = event
		}
		if received["bad"]["type"] != "error" || received["mismatch"]["type"] != "error" || received["ok"]["type"] != "audio_result" {
			t.Errorf("unexpected events: %v", received)
		}
		encoded, _ := received["ok"]["audio_base64"].(string)
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || string(data[:4]) != "RIFF" {
			t.Errorf("invalid audio: %v", err)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cfg := WorkerConfig{ServerURL: "ws" + strings.TrimPrefix(server.URL, "http"), WorkerID: "test", Token: "secret", BinaryPath: bin, ModelsDir: dir, MaxConcurrency: 1}
	_ = runSession(ctx, cfg)
	mu.Lock()
	defer mu.Unlock()
	if header != "secret" || query != "" {
		t.Errorf("auth leaked: header=%q query=%q", header, query)
	}
}

func TestRejectTokenQueryAndInvalidConfig(t *testing.T) {
	bin := fakePiper(t)
	dir := t.TempDir()
	modelFixture(t, dir, "voice", validConfig)
	base := WorkerConfig{ServerURL: "ws://127.0.0.1:1/worker?token=secret", WorkerID: "test", Token: "secret", BinaryPath: bin, ModelsDir: dir, MaxConcurrency: 1}
	if err := runSession(context.Background(), base); err == nil || !strings.Contains(err.Error(), "token query") {
		t.Fatalf("query token accepted: %v", err)
	}
	base.ServerURL = "ws://127.0.0.1:1/worker"
	base.Token = ""
	if err := runSession(context.Background(), base); err == nil {
		t.Fatal("missing token accepted")
	}
	base.Token = "secret"
	base.MaxConcurrency = 9
	if err := runSession(context.Background(), base); err == nil {
		t.Fatal("oversubscribed worker accepted")
	}
}

func TestBoundedVoiceAndCancellation(t *testing.T) {
	bin := fakePiper(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var ignored json.RawMessage
		if err := conn.ReadJSON(&ignored); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]string{"type": "registered"})
		for _, id := range []string{"first", "second"} {
			_ = conn.WriteJSON(request{Type: "tts_request", RequestID: id, Model: "piper:sleep", Text: "x"})
		}
		_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Errorf("busy response: %v", err)
			return
		}
		if event["request_id"] != "second" || event["type"] != "error" {
			t.Errorf("expected busy response: %v", event)
		}
		_ = conn.WriteJSON(request{Type: "cancel", RequestID: "first"})
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		if err := conn.ReadJSON(&event); err == nil {
			t.Errorf("cancelled synthesis emitted event: %v", event)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	modelFixture(t, dir, "sleep", validConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := WorkerConfig{ServerURL: "ws" + strings.TrimPrefix(server.URL, "http"), WorkerID: "test", Token: "secret", BinaryPath: bin, ModelsDir: dir, MaxConcurrency: 1}
	err := runSession(ctx, cfg)
	if err == nil {
		t.Fatal("expected session close")
	}
}
