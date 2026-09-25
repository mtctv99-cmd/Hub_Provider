package colab

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	a := New(Config{URL: "ws://localhost", WorkerID: "test"})
	if a.cfg.Model != "gemma-4-colab" {
		t.Fatalf("model=%s", a.cfg.Model)
	}
	if a.cfg.Heartbeat != 15*time.Second {
		t.Fatalf("heartbeat=%s", a.cfg.Heartbeat)
	}
}
