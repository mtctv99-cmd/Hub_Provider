package filestore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return store, root
}

func TestRejectsPathTraversal(t *testing.T) {
	store, root := newTestStore(t)
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, id := range []string{"../outside.txt", `..\\outside.txt`, filepath.Join("nested", "file.txt"), "", "/etc/passwd", "bad\x00name"} {
		if _, err := store.Read(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Read(%q) error = %v, want ErrInvalidID", id, err)
		}
		if err := store.Delete(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Delete(%q) error = %v, want ErrInvalidID", id, err)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "keep" {
		t.Fatalf("outside file changed: data=%q err=%v", got, err)
	}
}

func TestAtomicWriteAndRead(t *testing.T) {
	store, root := newTestStore(t)
	want := []byte("media payload")
	id, err := store.Save(want, ".txt")
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if filepath.Ext(id) != ".txt" {
		t.Fatalf("generated ID %q does not have expected extension", id)
	}
	if len(filepath.Base(id)) != len(id) {
		t.Fatalf("generated ID %q is not a basename", id)
	}

	got, err := store.Read(id)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Read = %q, want %q", got, want)
	}
	f, err := store.Open(id)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || stat.Size() != int64(len(want)) {
		t.Fatalf("Open stat = size %d err %v", stat.Size(), err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != id {
		t.Fatalf("root contains %v, want only committed file %q", entries, id)
	}
	if err := store.Delete(id); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := store.Read(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read after Delete error = %v, want os.ErrNotExist", err)
	}
}

func TestContentTypeByExtension(t *testing.T) {
	store, _ := newTestStore(t)
	tests := map[string]string{
		"image.jpg":   "image/jpeg",
		"image.jpeg":  "image/jpeg",
		"image.png":   "image/png",
		"audio.mp3":   "audio/mpeg",
		"audio.wav":   "audio/wav",
		"video.mp4":   "video/mp4",
		"unknown.bin": "application/octet-stream",
	}
	for name, want := range tests {
		if got := store.ContentType(name); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", name, got, want)
		}
		if got := ContentType(name); got != want {
			t.Errorf("package ContentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestListAndCleanup(t *testing.T) {
	store, _ := newTestStore(t)
	oldID, err := store.Save([]byte("old"), "txt")
	if err != nil {
		t.Fatal(err)
	}
	newID, err := store.Save([]byte("new"), "txt")
	if err != nil {
		t.Fatal(err)
	}
	oldPath, err := store.pathForID(oldID)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	files, err := store.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("List returned %d files, want 2", len(files))
	}
	seen := map[string]bool{files[0].ID: true, files[1].ID: true}
	if !seen[oldID] || !seen[newID] {
		t.Fatalf("List IDs = %v, want %q and %q", seen, oldID, newID)
	}
	if files[0].ContentType != "text/plain; charset=utf-8" && files[1].ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("List did not report text MIME type: %+v", files)
	}

	removed, err := store.Cleanup(time.Hour)
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if len(removed) != 1 || removed[0] != oldID {
		t.Fatalf("Cleanup removed %v, want [%q]", removed, oldID)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(oldPath), oldID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old file still exists, stat error = %v", err)
	}
	if _, err := store.Read(newID); err != nil {
		t.Fatalf("new file was removed: %v", err)
	}
}
