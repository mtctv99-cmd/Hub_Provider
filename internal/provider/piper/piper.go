package piper

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"sever_provider/internal/provider"
)

type Voice struct {
	ID        string `json:"voice_id"`
	Name      string `json:"name"`
	Engine    string `json:"engine"`
	Language  string `json:"language,omitempty"`
	ModelPath string `json:"-"`
}

type Adapter struct {
	BinaryPath string
	ModelsDir  string
	mu         sync.RWMutex
	voices     map[string]Voice
}

func New(binaryPath, modelsDir string) *Adapter {
	if binaryPath == "" {
		binaryPath = os.Getenv("PIPER_BIN")
	}
	if binaryPath == "" {
		binaryPath = "piper"
	}
	if modelsDir == "" {
		modelsDir = os.Getenv("PIPER_MODELS_DIR")
	}
	if modelsDir == "" {
		modelsDir = "models"
	}
	a := &Adapter{BinaryPath: binaryPath, ModelsDir: modelsDir, voices: make(map[string]Voice)}
	_ = a.RefreshVoices()
	return a
}

func (a *Adapter) RefreshVoices() error {
	root, err := filepath.Abs(a.ModelsDir)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("piper models path is not a directory")
	}
	found := make(map[string]Voice)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".onnx" {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		id := strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1]))
		if len(parts) == 2 && strings.TrimSuffix(parts[0], filepath.Ext(parts[0])) == id {
			id = parts[0]
		}
		if id == "" || strings.ContainsAny(id, `/\\`) || id == "khmer_tts_v3" {
			return nil
		}
		found[id] = Voice{ID: id, Name: id, Engine: "piper", Language: languageForVoice(id), ModelPath: path}

		return nil
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.voices = found
	a.mu.Unlock()
	return nil
}

func languageForVoice(id string) string {
	lower := strings.ToLower(id)
	if strings.HasPrefix(lower, "th_") || strings.Contains(lower, "thai") {
		return "th"
	}
	if strings.HasPrefix(lower, "khmer") || strings.Contains(lower, "km_") {
		return "km"
	}
	return "vi"
}

func (a *Adapter) Voices() []Voice {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Voice, 0, len(a.voices))
	for _, voice := range a.voices {
		out = append(out, voice)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *Adapter) Ready() bool {
	if _, err := exec.LookPath(a.BinaryPath); err != nil && filepath.Base(a.BinaryPath) == a.BinaryPath {
		return false
	}
	return len(a.Voices()) > 0
}

func (a *Adapter) Name() string { return "piper_local" }
func (a *Adapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilitySpeech}
}
func (a *Adapter) Chat(context.Context, provider.ChatRequest, []byte) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, provider.ErrCapabilityUnsupported
}
func (a *Adapter) StreamChat(context.Context, provider.ChatRequest, []byte) (<-chan provider.StreamChunk, error) {
	return nil, provider.ErrCapabilityUnsupported
}
func (a *Adapter) Image(context.Context, provider.ImageRequest, []byte) (provider.ImageResponse, error) {
	return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
}

func (a *Adapter) Speech(ctx context.Context, req provider.SpeechRequest, _ []byte) ([]byte, string, error) {
	model := req.Voice
	if model == "" {
		model = "ngochuyennew"
	}
	modelPath := a.modelPath(model)
	if modelPath == "" {
		return nil, "", fmt.Errorf("invalid piper voice %q", model)
	}
	if _, err := os.Stat(modelPath); err != nil {
		return nil, "", fmt.Errorf("piper model %q not found: %w", model, err)
	}

	output, err := os.CreateTemp(filepath.Dir(modelPath), ".piper-*.wav")
	if err != nil {
		return nil, "", fmt.Errorf("create piper output file: %w", err)
	}
	outputPath := output.Name()
	if err := output.Close(); err != nil {
		_ = os.Remove(outputPath)
		return nil, "", fmt.Errorf("prepare piper output file: %w", err)
	}
	defer os.Remove(outputPath)

	cmd := exec.CommandContext(ctx, a.BinaryPath, "--model", modelPath, "--output_file", outputPath)
	cmd.Stdin = bytes.NewBufferString(req.Input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("piper failed: %w: %s", err, stderr.String())
	}
	audio, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, "", fmt.Errorf("read piper output: %w", err)
	}
	if len(audio) == 0 {
		return nil, "", fmt.Errorf("piper returned empty audio")
	}
	if len(audio) < 12 || !bytes.Equal(audio[:4], []byte("RIFF")) || !bytes.Equal(audio[8:12], []byte("WAVE")) {
		return nil, "", fmt.Errorf("piper returned invalid WAV audio")
	}
	return audio, "audio/wav", nil
}

func (a *Adapter) modelPath(voice string) string {
	if voice == "khmer_tts_v3" {
		return ""
	}
	a.mu.RLock()
	if v, ok := a.voices[voice]; ok && v.ModelPath != "" {
		a.mu.RUnlock()
		return v.ModelPath
	}
	a.mu.RUnlock()

	voice = filepath.Clean(voice)
	if strings.Contains(voice, "..") || filepath.IsAbs(voice) {
		return ""
	}
	candidates := []string{
		filepath.Join(a.ModelsDir, voice, voice+".onnx"),
		filepath.Join(a.ModelsDir, voice+".onnx"),
	}
	if voice == "ngochuyennew" {
		candidates = append([]string{filepath.Join(a.ModelsDir, "ngoc_huyen", "ngochuyennew.onnx")}, candidates...)
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(a.ModelsDir, voice, voice+".onnx")
}
