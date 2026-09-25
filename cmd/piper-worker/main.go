package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxWAVBytes     = 16 << 20
	maxTextBytes    = 32 << 10
	maxMessageBytes = 64 << 10
	jobTimeout      = 30 * time.Second
	writeTimeout    = 10 * time.Second
)

type WorkerConfig struct {
	ServerURL      string
	WorkerID       string
	Token          string
	BinaryPath     string
	ModelsDir      string
	MaxConcurrency int
}

func main() {
	serverURL := flag.String("server", getEnv("PIPER_SERVER_URL", "ws://127.0.0.1:8080/api/llm/ws/worker"), "Server workerhub WebSocket endpoint")
	workerID := flag.String("id", getEnv("PIPER_WORKER_ID", "piper-worker-01"), "Unique Piper worker identifier")
	token := getEnv("PIPER_WORKER_TOKEN", "")
	binaryPath := flag.String("bin", getEnv("PIPER_BIN", "piper"), "Piper executable binary path")
	modelsDir := flag.String("models", getEnv("PIPER_MODELS_DIR", "models"), "Directory containing Piper .onnx models")
	concurrency := flag.Int("concurrency", 1, "Max parallel jobs across voices (one job per voice)")
	flag.Parse()

	cfg := WorkerConfig{ServerURL: *serverURL, WorkerID: *workerID, Token: token, BinaryPath: *binaryPath, ModelsDir: *modelsDir, MaxConcurrency: *concurrency}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Printf("[PIPER WORKER] Starting %s (models: %s)", cfg.WorkerID, cfg.ModelsDir)
	for ctx.Err() == nil {
		if err := runSession(ctx, cfg); err != nil && ctx.Err() == nil {
			// Do not log the URL, request payload, or authentication material.
			log.Printf("[PIPER WORKER] Session closed: %v. Retrying in 3s...", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}
}

func runSession(ctx context.Context, cfg WorkerConfig) error {
	if cfg.Token == "" {
		return errors.New("worker token is required")
	}
	if cfg.MaxConcurrency < 1 || cfg.MaxConcurrency > 8 {
		return errors.New("concurrency must be between 1 and 8")
	}
	if _, err := exec.LookPath(cfg.BinaryPath); err != nil {
		return fmt.Errorf("piper executable unavailable: %w", err)
	}
	models, err := discoverModels(cfg.ModelsDir)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no supported Piper models with valid metadata found in %s", cfg.ModelsDir)
	}
	u, err := url.Parse(cfg.ServerURL)
	if err != nil {
		return errors.New("invalid server URL")
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil || u.Query().Has("token") {
		return errors.New("server URL must be ws/wss with no credentials or token query")
	}
	header := http.Header{"X-Worker-Token": []string{cfg.Token}}
	conn, _, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).DialContext(ctx, u.String(), header)
	if err != nil {
		return errors.New("dial server failed (check endpoint and worker authentication)")
	}
	defer conn.Close()
	conn.SetReadLimit(maxMessageBytes)
	voices := make([]string, 0, len(models))
	for voice := range models {
		voices = append(voices, "piper:"+voice)
	}
	sort.Strings(voices)
	s := &session{conn: conn, cfg: cfg, models: models, ctx: ctx, slots: make(chan struct{}, cfg.MaxConcurrency), active: make(map[string]context.CancelFunc), busy: make(map[string]bool)}
	reg := map[string]any{"worker_id": cfg.WorkerID, "model": voices[0], "models": voices, "capabilities": []string{"audio_speech", "tts"}, "max_concurrency": cfg.MaxConcurrency, "token": cfg.Token}
	if err := s.send(reg); err != nil {
		return errors.New("send registration failed")
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var ack struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&ack); err != nil {
		return errors.New("registration acknowledgement failed")
	}
	_ = conn.SetReadDeadline(time.Time{})
	if ack.Type != "registered" {
		return errors.New("registration not acknowledged")
	}
	log.Printf("[PIPER WORKER] Registered as %s (voices: %d)", cfg.WorkerID, len(voices))
	// Interrupt a blocking read on shutdown and cancel all jobs on disconnect.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	defer s.shutdown()
	for ctx.Err() == nil {
		var msg request
		if err := conn.ReadJSON(&msg); err != nil {
			return errors.New("worker connection closed")
		}
		switch msg.Type {
		case "ping":
			if err := s.send(map[string]string{"type": "pong"}); err != nil {
				return err
			}
		case "cancel":
			s.cancel(msg.RequestID)
		case "chat_request", "tts_request":
			s.start(msg)
		}
	}
	return ctx.Err()
}

type request struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	VoiceID   string `json:"voice_id"`
	Model     string `json:"model"`
	Text      string `json:"text"`
}

type session struct {
	conn    *websocket.Conn
	cfg     WorkerConfig
	models  map[string]string
	ctx     context.Context
	writeMu sync.Mutex
	mu      sync.Mutex
	wg      sync.WaitGroup
	slots   chan struct{}
	active  map[string]context.CancelFunc
	busy    map[string]bool
}

func (s *session) send(message any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return s.conn.WriteJSON(message)
}

func (s *session) sendError(id, detail string) {
	if id != "" {
		_ = s.send(map[string]any{"type": "error", "request_id": id, "error": detail})
	}
}

func (s *session) start(req request) {
	if req.RequestID == "" {
		return
	}
	voice := req.VoiceID
	if voice == "" && strings.HasPrefix(req.Model, "piper:") {
		voice = strings.TrimPrefix(req.Model, "piper:")
	}
	path, ok := s.models[voice]
	if !ok || (req.Model != "" && req.Model != "piper:"+voice) {
		s.sendError(req.RequestID, "unsupported voice")
		return
	}
	if strings.TrimSpace(req.Text) == "" || len(req.Text) > maxTextBytes {
		s.sendError(req.RequestID, "text is empty or too long")
		return
	}
	s.mu.Lock()
	if s.active[req.RequestID] != nil {
		s.mu.Unlock()
		return // Never terminate a live request with a duplicate ID.
	}
	if s.busy[voice] {
		s.mu.Unlock()
		s.sendError(req.RequestID, "worker voice busy")
		return
	}
	select {
	case s.slots <- struct{}{}:
	default:
		s.mu.Unlock()
		s.sendError(req.RequestID, "worker at capacity")
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, jobTimeout)
	s.active[req.RequestID] = cancel
	s.busy[voice] = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.active, req.RequestID)
			delete(s.busy, voice)
			<-s.slots
			s.mu.Unlock()
			cancel()
		}()
		data, err := synthesize(ctx, s.cfg.BinaryPath, path, req.Text)
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && s.ctx.Err() == nil {
				s.sendError(req.RequestID, "piper synthesis timed out")
			}
			return // A cancelled request must not produce a late success.
		}

		if err != nil {
			s.sendError(req.RequestID, err.Error())
			return
		}
		_ = s.send(map[string]any{"type": "audio_result", "request_id": req.RequestID, "mime": "audio/wav", "audio_base64": base64.StdEncoding.EncodeToString(data), "done": true})
	}()
}

func (s *session) cancel(id string) {
	s.mu.Lock()
	cancel := s.active[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *session) shutdown() {
	s.mu.Lock()
	for _, cancel := range s.active {
		cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func synthesize(ctx context.Context, binaryPath, model, text string) ([]byte, error) {
	// A private output file works with both Piper's CLI and its Windows Python
	// distribution (which cannot reopen a NamedTemporaryFile for stdout WAV).
	out, err := os.CreateTemp("", "piper-worker-*.wav")
	if err != nil {
		return nil, errors.New("cannot create audio output")
	}
	path := out.Name()
	_ = out.Close()
	defer os.Remove(path)
	cmd := exec.CommandContext(ctx, binaryPath, "--model", model, "--output_file", path)
	cmd.Stdin = strings.NewReader(text)
	// Suppress stderr: Piper may echo text or paths containing private data.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("synthesis cancelled or timed out: %w", ctx.Err())
	}
	if err != nil {
		return nil, errors.New("piper synthesis failed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() < 44 || info.Size() > maxWAVBytes {
		return nil, errors.New("piper audio is empty or exceeds limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read piper audio")
	}
	if string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" || int64(binary.LittleEndian.Uint32(data[4:8]))+8 != int64(len(data)) {
		return nil, errors.New("piper did not return a complete WAV file")
	}
	return data, nil
}

// discoverModels only advertises Piper-compatible model/metadata pairs. The
// bundled Khmer ONNX model uses a separate vocabulary/preprocessing pipeline;
// Piper's CLI cannot synthesize it, so it is deliberately unsupported here.
func discoverModels(dir string) (map[string]string, error) {
	found := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".onnx") {
			return nil
		}
		id := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if id == "" {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil
		}
		meta, err := os.Open(path + ".json")
		if err != nil {
			return nil
		}
		var config struct {
			PhonemeType  string           `json:"phoneme_type"`
			PhonemeIDMap map[string][]int `json:"phoneme_id_map"`
			Audio        struct {
				SampleRate int `json:"sample_rate"`
			} `json:"audio"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(meta, 1<<20)).Decode(&config)
		_ = meta.Close()
		if decodeErr != nil || (config.PhonemeType != "espeak" && config.PhonemeType != "text") || len(config.PhonemeIDMap) == 0 || config.Audio.SampleRate <= 0 {
			return nil
		}
		if _, exists := found[id]; exists {
			return fmt.Errorf("duplicate Piper voice %q", id)
		}
		found[id] = path
		return nil
	})
	return found, err
}

func getEnv(key, def string) string {
	if val := os.Getenv(key); strings.TrimSpace(val) != "" {
		return strings.TrimSpace(val)
	}
	return def
}
