package buildinfo

import "testing"

func TestDefaults(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must never be empty")
	}
	if Commit == "" {
		t.Fatal("Commit must never be empty")
	}
	if Version != "dev" {
		t.Fatalf("unexpected default version %q, want dev", Version)
	}
	if Commit != "unknown" {
		t.Fatalf("unexpected default commit %q, want unknown", Commit)
	}
}
