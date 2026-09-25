package piper

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"sever_provider/internal/provider"
)

func TestSpeechFailsClearlyWhenModelMissing(t *testing.T) {
	a := New("does-not-exist", t.TempDir())
	_, _, err := a.Speech(context.Background(), provider.SpeechRequest{Input: "hello", Voice: "missing"}, nil)
	if err == nil {
		t.Fatal("expected missing model error")
	}
}

func TestRefreshVoicesDiscoversNestedAndFlatModels(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "flat.onnx"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "nested.onnx"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "khmer_tts_v3.onnx"), []byte("onnx"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New("piper", dir)
	voices := a.Voices()
	if len(voices) != 2 || voices[0].ID != "flat" || voices[1].ID != "nested" {
		t.Fatalf("voices=%+v; Khmer ONNX model must use its dedicated runtime", voices)
	}
}

func TestKhmerModelIsNotRunThroughPiperCLI(t *testing.T) {
	a := New("piper", t.TempDir())
	_, _, err := a.Speech(context.Background(), provider.SpeechRequest{Voice: "khmer_tts_v3", Input: "សួស្តី"}, nil)
	if err == nil || err.Error() != `invalid piper voice "khmer_tts_v3"` {
		t.Fatalf("err=%v", err)
	}
}

func TestModelPathResolution(t *testing.T) {
	dir := t.TempDir()
	voiceDir := filepath.Join(dir, "voice")
	if err := os.MkdirAll(voiceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(voiceDir, "voice.onnx")
	if err := os.WriteFile(model, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New("piper", dir)
	if got := a.modelPath("voice"); got != model {
		t.Fatalf("got %q, want %q", got, model)
	}

	vnDir := filepath.Join(dir, "ngoc_huyen")
	if err := os.MkdirAll(vnDir, 0o700); err != nil {
		t.Fatal(err)
	}
	vnModel := filepath.Join(vnDir, "ngochuyennew.onnx")
	if err := os.WriteFile(vnModel, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.modelPath("ngochuyennew"); got != vnModel {
		t.Fatalf("got %q, want %q", got, vnModel)
	}
}
