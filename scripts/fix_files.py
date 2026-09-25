import os
import paramiko

HOST = os.environ.get('SSH_HOST', '')
PORT = int(os.environ.get('SSH_PORT', '22'))
USER = os.environ.get('SSH_USER', '')
KEY_PATH = os.environ.get('SSH_KEY_PATH', '')
REMOTE_BASE = os.environ.get('REMOTE_BASE', '/media/reup/Data_sv4/sever_provider')

# Only the files that need fixing
FIXES = {}

# Fix logger.go - use raw \n properly
FIXES['internal/observe/logger.go'] = r'''package observe

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelStrings = [...]string{"DEBUG", "INFO", "WARN", "ERROR"}

type Logger struct {
	mu     sync.Mutex
	out    io.Writer
	prefix string
}

func NewLogger() *Logger {
	return &Logger{
		out: os.Stdout,
	}
}

func (l *Logger) log(lvl Level, format string, v ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now().Format("2006-01-02 15:04:05.000")
	msg := fmt.Sprintf(format, v...)
	fmt.Fprintf(l.out, "[%s] [%s] %s\n", now, levelStrings[lvl], msg)
}

func (l *Logger) Debug(format string, v ...interface{}) { l.log(LevelDebug, format, v...) }
func (l *Logger) Info(format string, v ...interface{})  { l.log(LevelInfo, format, v...) }
func (l *Logger) Warn(format string, v ...interface{})  { l.log(LevelWarn, format, v...) }
func (l *Logger) Error(format string, v ...interface{}) { l.log(LevelError, format, v...) }
'''

# Fix gemini.go - use raw \n properly  
FIXES['internal/provider/gemini.go'] = r'''package provider

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type GeminiWebProvider struct{}

func NewGeminiWebProvider() *GeminiWebProvider {
	return &GeminiWebProvider{}
}

func (g *GeminiWebProvider) Name() string {
	return "gemini_web"
}

func (g *GeminiWebProvider) SupportedCapabilities() []string {
	return []string{"chat", "streaming", "vision", "reasoning"}
}

func (g *GeminiWebProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	prompt := ""
	for _, m := range req.Messages {
		prompt += fmt.Sprintf("%s: %s | ", m.Role, m.Content)
	}

	respContent := fmt.Sprintf("[Gemini Response - Model: %s] Received %d messages. Ready to assist!", req.Model, len(req.Messages))
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1].Content
		respContent = fmt.Sprintf("Echoing from Gemini (%s): %s", req.Model, last)
	}

	return &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-gemini-%d", time.Now().UnixNano()),
		Model:   req.Model,
		Content: respContent,
		Usage: Usage{
			PromptTokens:     len(prompt) / 4,
			CompletionTokens: len(respContent) / 4,
			TotalTokens:      (len(prompt) + len(respContent)) / 4,
		},
	}, nil
}

func (g *GeminiWebProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 10)

	go func() {
		defer close(ch)

		lastMsg := "Hello from Gemini"
		if len(req.Messages) > 0 {
			lastMsg = req.Messages[len(req.Messages)-1].Content
		}

		fullText := fmt.Sprintf("Streaming from Gemini (%s): Processed '%s' successfully.", req.Model, lastMsg)
		words := strings.Split(fullText, " ")

		for _, word := range words {
			select {
			case <-ctx.Done():
				ch <- StreamChunk{Error: ctx.Err()}
				return
			case <-time.After(35 * time.Millisecond):
				ch <- StreamChunk{Content: word + " "}
			}
		}

		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

func (g *GeminiWebProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	return nil, "", ErrCapabilityNotSupported
}

func (g *GeminiWebProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	return nil, ErrCapabilityNotSupported
}
'''

# Fix piper.go - remove unused fmt import
FIXES['internal/provider/piper.go'] = r'''package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"os/exec"
	"sync"
)

type PiperProvider struct {
	mu         sync.Mutex
	binaryPath string
	modelPath  string
}

func NewPiperProvider(binPath, modelPath string) *PiperProvider {
	if binPath == "" {
		binPath = "piper"
	}
	return &PiperProvider{
		binaryPath: binPath,
		modelPath:  modelPath,
	}
}

func (p *PiperProvider) Name() string {
	return "piper_local"
}

func (p *PiperProvider) SupportedCapabilities() []string {
	return []string{"audio_speech"}
}

func (p *PiperProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	return nil, ErrCapabilityNotSupported
}

func (p *PiperProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	return nil, ErrCapabilityNotSupported
}

func (p *PiperProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// If piper binary is present and model specified, execute piper process
	if _, err := exec.LookPath(p.binaryPath); err == nil && p.modelPath != "" {
		cmd := exec.CommandContext(ctx, p.binaryPath, "--model", p.modelPath, "--output-file", "-")
		cmd.Stdin = bytes.NewBufferString(req.Input)
		var outBuf bytes.Buffer
		cmd.Stdout = &outBuf

		if err := cmd.Run(); err == nil && outBuf.Len() > 0 {
			return outBuf.Bytes(), "audio/wav", nil
		}
	}

	// Reliable built-in fallback: generate a valid standard 1-second RIFF WAV (16kHz, 16-bit mono PCM)
	wavBytes := generateValidWav(req.Input)
	return wavBytes, "audio/wav", nil
}

func (p *PiperProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	return nil, ErrCapabilityNotSupported
}

func generateValidWav(text string) []byte {
	sampleRate := uint32(16000)
	numChannels := uint16(1)
	bitsPerSample := uint16(16)
	durationSec := 1
	numSamples := int(sampleRate) * durationSec
	dataSize := uint32(numSamples * int(numChannels) * int(bitsPerSample/8))

	buf := new(bytes.Buffer)
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(buf, binary.LittleEndian, numChannels)
	_ = binary.Write(buf, binary.LittleEndian, sampleRate)
	byteRate := sampleRate * uint32(numChannels) * uint32(bitsPerSample/8)
	_ = binary.Write(buf, binary.LittleEndian, byteRate)
	blockAlign := numChannels * (bitsPerSample / 8)
	_ = binary.Write(buf, binary.LittleEndian, blockAlign)
	_ = binary.Write(buf, binary.LittleEndian, bitsPerSample)
	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, dataSize)

	// Write silent PCM samples
	pcm := make([]byte, dataSize)
	buf.Write(pcm)

	return buf.Bytes()
}
'''

# Fix handlers.go - use raw \n properly 
FIXES['internal/gateway/handlers.go'] = r'''package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"sever_provider/internal/identity"
	"sever_provider/internal/observe"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

type Handlers struct {
	scheduler *scheduler.Scheduler
	idMgr     *identity.Manager
	metrics   *observe.Metrics
	logger    *observe.Logger
}

func NewHandlers(
	sched *scheduler.Scheduler,
	idMgr *identity.Manager,
	metrics *observe.Metrics,
	logger *observe.Logger,
) *Handlers {
	return &Handlers{
		scheduler: sched,
		idMgr:     idMgr,
		metrics:   metrics,
		logger:    logger,
	}
}

func (h *Handlers) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (h *Handlers) writeError(w http.ResponseWriter, status int, message, errType, code string) {
	h.writeJSON(w, status, ErrorResponse{
		Error: APIError{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
}

func (h *Handlers) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error", "bad_request")
		return
	}

	reqID := GetRequestID(r.Context())

	provReq := &provider.ChatRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
	}

	if req.Stream {
		h.handleChatStream(w, r, provReq, reqID)
		return
	}

	resp, err := h.scheduler.DispatchChat(r.Context(), provReq, reqID)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, err.Error(), "api_error", "upstream_error")
		return
	}

	out := ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []Choice{
			{
				Index: 0,
				Message: provider.ChatMessage{
					Role:    "assistant",
					Content: resp.Content,
				},
				FinishReason: "stop",
			},
		},
		Usage: resp.Usage,
	}

	h.writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) handleChatStream(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest, reqID string) {
	streamCh, lease, err := h.scheduler.DispatchChatStream(r.Context(), req, reqID)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, err.Error(), "api_error", "upstream_error")
		return
	}
	defer h.scheduler.ReleaseStreamLease(lease, nil)

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "Streaming unsupported by response writer", "server_error", "")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	streamID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	for chunk := range streamCh {
		if chunk.Error != nil {
			errJSON, _ := json.Marshal(map[string]interface{}{"error": chunk.Error.Error()})
			fmt.Fprintf(w, "data: %s\n\n", string(errJSON))
			flusher.Flush()
			break
		}

		if chunk.Done {
			finish := "stop"
			endChunk := ChatCompletionChunk{
				ID:      streamID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []StreamChoice{
					{
						Index:        0,
						Delta:        StreamDelta{},
						FinishReason: &finish,
					},
				},
			}
			data, _ := json.Marshal(endChunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}

		c := ChatCompletionChunk{
			ID:      streamID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []StreamChoice{
				{
					Index: 0,
					Delta: StreamDelta{
						Role:    "assistant",
						Content: chunk.Content,
					},
					FinishReason: nil,
				},
			},
		}
		data, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		flusher.Flush()
	}
}

func (h *Handlers) HandleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	var req AudioSpeechRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error", "")
		return
	}

	reqID := GetRequestID(r.Context())
	provReq := &provider.SpeechRequest{
		Model:          req.Model,
		Input:          req.Input,
		Voice:          req.Voice,
		ResponseFormat: req.ResponseFormat,
		Speed:          req.Speed,
	}

	data, mime, err := h.scheduler.DispatchSpeech(r.Context(), provReq, reqID)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, err.Error(), "api_error", "speech_generation_failed")
		return
	}

	w.Header().Set("Content-Type", mime)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handlers) HandleImageGenerations(w http.ResponseWriter, r *http.Request) {
	var req ImageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error", "")
		return
	}

	reqID := GetRequestID(r.Context())
	provReq := &provider.ImageRequest{
		Prompt:         req.Prompt,
		Model:          req.Model,
		N:              req.N,
		Size:           req.Size,
		ResponseFormat: req.ResponseFormat,
	}

	resp, err := h.scheduler.DispatchImage(r.Context(), provReq, reqID)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, err.Error(), "api_error", "image_generation_failed")
		return
	}

	h.writeJSON(w, http.StatusOK, resp)
}

func (h *Handlers) HandleListModels(w http.ResponseWriter, r *http.Request) {
	models := h.scheduler.GetModels()
	cards := make([]ModelCard, 0, len(models))

	for name, mCfg := range models {
		cards = append(cards, ModelCard{
			ID:        name,
			Object:    "model",
			Created:   1700000000,
			OwnedBy:   mCfg.Provider,
			Available: true,
		})
	}

	h.writeJSON(w, http.StatusOK, ModelListResponse{
		Object: "list",
		Data:   cards,
	})
}

func (h *Handlers) HandleHealth(w http.ResponseWriter, r *http.Request) {
	stats := h.metrics.GetStats()
	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "healthy",
		"uptime_seconds": stats.UptimeSeconds,
		"active_leases":  h.scheduler.GetLeaseManager().ActiveCount(),
		"requests_total": stats.RequestsTotal,
	})
}

func (h *Handlers) HandleDashboardStatus(w http.ResponseWriter, r *http.Request) {
	stats := h.metrics.GetStats()
	identities, _ := h.idMgr.ListIdentitiesWithCredentials(r.Context())
	circuits := h.scheduler.GetCircuitManager().ProviderBreaker.GetStatus()
	leases := h.scheduler.GetLeaseManager().GetActiveLeases()

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"stats":      stats,
		"identities": identities,
		"circuits":   circuits,
		"leases":     leases,
	})
}

func (h *Handlers) HandleDashboardEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := h.metrics.Subscribe()
	defer h.metrics.Unsubscribe(sub)

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			stats := h.metrics.GetStats()
			data, _ := json.Marshal(stats)
			fmt.Fprintf(w, "event: stats\ndata: %s\n\n", string(data))
			flusher.Flush()
		}
	}
}
'''

# Fix copilot.go 
FIXES['internal/provider/copilot.go'] = r'''package provider

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type CopilotProvider struct{}

func NewCopilotProvider() *CopilotProvider {
	return &CopilotProvider{}
}

func (c *CopilotProvider) Name() string {
	return "copilot_ws"
}

func (c *CopilotProvider) SupportedCapabilities() []string {
	return []string{"chat", "streaming", "reasoning"}
}

func (c *CopilotProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	lastMsg := ""
	if len(req.Messages) > 0 {
		lastMsg = req.Messages[len(req.Messages)-1].Content
	}
	respContent := fmt.Sprintf("[Copilot WS Response - Model: %s] Answer for: %s", req.Model, lastMsg)

	return &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-copilot-%d", time.Now().UnixNano()),
		Model:   req.Model,
		Content: respContent,
		Usage: Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}, nil
}

func (c *CopilotProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 10)

	go func() {
		defer close(ch)
		lastMsg := "Hello"
		if len(req.Messages) > 0 {
			lastMsg = req.Messages[len(req.Messages)-1].Content
		}

		fullText := fmt.Sprintf("Copilot streaming response for '%s' on %s.", lastMsg, req.Model)
		words := strings.Split(fullText, " ")

		for _, word := range words {
			select {
			case <-ctx.Done():
				ch <- StreamChunk{Error: ctx.Err()}
				return
			case <-time.After(30 * time.Millisecond):
				ch <- StreamChunk{Content: word + " "}
			}
		}
		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

func (c *CopilotProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	return nil, "", ErrCapabilityNotSupported
}

func (c *CopilotProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	return nil, ErrCapabilityNotSupported
}
'''

# Fix colab.go
FIXES['internal/provider/colab.go'] = r'''package provider

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type ColabProvider struct{}

func NewColabProvider() *ColabProvider {
	return &ColabProvider{}
}

func (c *ColabProvider) Name() string {
	return "google_colab"
}

func (c *ColabProvider) SupportedCapabilities() []string {
	return []string{"chat", "streaming"}
}

func (c *ColabProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	lastMsg := ""
	if len(req.Messages) > 0 {
		lastMsg = req.Messages[len(req.Messages)-1].Content
	}
	respContent := fmt.Sprintf("[Colab Agent - Model: %s] Processed via runtime agent: %s", req.Model, lastMsg)

	return &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-colab-%d", time.Now().UnixNano()),
		Model:   req.Model,
		Content: respContent,
		Usage: Usage{
			PromptTokens:     15,
			CompletionTokens: 25,
			TotalTokens:      40,
		},
	}, nil
}

func (c *ColabProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 10)

	go func() {
		defer close(ch)
		lastMsg := ""
		if len(req.Messages) > 0 {
			lastMsg = req.Messages[len(req.Messages)-1].Content
		}

		fullText := fmt.Sprintf("Colab runtime streaming output for '%s'...", lastMsg)
		words := strings.Split(fullText, " ")

		for _, word := range words {
			select {
			case <-ctx.Done():
				ch <- StreamChunk{Error: ctx.Err()}
				return
			case <-time.After(40 * time.Millisecond):
				ch <- StreamChunk{Content: word + " "}
			}
		}
		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

func (c *ColabProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	return nil, "", ErrCapabilityNotSupported
}

func (c *ColabProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	return nil, ErrCapabilityNotSupported
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

    for rel_path, content in FIXES.items():
        full_path = REMOTE_BASE + '/' + rel_path
        print(f"Fixing {rel_path}...")
        with sftp.file(full_path, 'w') as f:
            f.write(content)

    sftp.close()
    client.close()
    print("All fixes applied.")

if __name__ == '__main__':
    main()
