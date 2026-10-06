package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestProfileLockOwnerProtection(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux Chrome singleton ownership")
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	lock := filepath.Join(profile, "SingletonLock")
	if err := os.Symlink(hostname+"-"+strconv.Itoa(os.Getpid()), lock); err != nil {
		t.Fatal(err)
	}
	if _, err := existingProfileCDP(profile); !errors.Is(err, ErrAutoLoginBusy) {
		t.Fatalf("live unrelated owner not protected: %v", err)
	}
	if _, err := os.Lstat(lock); err != nil {
		t.Fatal("live owner lock removed")
	}
	_ = os.Remove(lock)
	if _, err := os.Stat("/proc/2147483647"); !os.IsNotExist(err) {
		t.Skip("test PID unavailable")
	}
	if err := os.Symlink(hostname+"-2147483647", lock); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SingletonSocket", "SingletonCookie"} {
		if err := os.Symlink("missing", filepath.Join(profile, name)); err != nil {
			t.Fatal(err)
		}
	}
	if port, err := existingProfileCDP(profile); err != nil || port != 0 {
		t.Fatalf("stale owner not reclaimed: port=%d err=%v", port, err)
	}
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if _, err := os.Lstat(filepath.Join(profile, name)); !os.IsNotExist(err) {
			t.Fatalf("stale %s remains", name)
		}
	}
}

func TestRestoredCookieParamsHostCookieScope(t *testing.T) {
	cookies, err := restoredCookieParams("SAPISID=test; __Host-GAPS=test; __Secure-1PSID=test")
	if err != nil {
		t.Fatal(err)
	}
	if cookies[0]["domain"] != ".google.com" {
		t.Fatal("Google domain not scoped")
	}
	if _, exists := cookies[1]["domain"]; exists {
		t.Fatal("host-only cookie must not have Domain")
	}
	if cookies[1]["url"] != "https://accounts.google.com/" || cookies[1]["path"] != "/" || cookies[1]["secure"] != true {
		t.Fatal("invalid host-only cookie scope")
	}
	if _, err := restoredCookieParams("SAPISID=x\r\nInjected: y"); err == nil {
		t.Fatal("header injection accepted")
	}
}

func TestProfileRefreshHonorsCancellationAndBusyLogin(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.RefreshAccountFromProfileContext(ctx, "Account-5", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	m.running.Store(true)
	if _, err := m.RefreshAccountFromProfileContext(context.Background(), "Account-5", ""); !errors.Is(err, ErrAutoLoginBusy) {
		t.Fatalf("expected active login preserved, got %v", err)
	}
	if !m.running.Load() {
		t.Fatal("refresh cleared active login")
	}
}

func TestAccountIDForEmail(t *testing.T) {
	id1 := AccountIDForEmail("test@example.com")
	id2 := AccountIDForEmail("TEST@example.com")
	id3 := AccountIDForEmail("  test@example.com  ")
	if id1 == "" {
		t.Fatal("expected non-empty account ID")
	}
	if id1 != id2 || id1 != id3 {
		t.Fatalf("expected deterministic ID regardless of casing/spacing: %q vs %q vs %q", id1, id2, id3)
	}
	if err := validateProfileID(id1); err != nil {
		t.Fatalf("derived ID %q fails profile ID validation: %v", id1, err)
	}
	diffID := AccountIDForEmail("other@example.com")
	if id1 == diffID {
		t.Fatalf("different emails should produce different IDs: %q == %q", id1, diffID)
	}
	if AccountIDForEmail("") != "" {
		t.Fatal("empty email should return empty ID")
	}
}

func TestExistingProfileCDPWindowsLockCleanup(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific lock cleanup test")
	}
	profile := t.TempDir()
	lock := filepath.Join(profile, "SingletonLock")
	if err := os.WriteFile(lock, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SingletonSocket", "SingletonCookie"} {
		_ = os.WriteFile(filepath.Join(profile, name), []byte("stale"), 0644)
	}
	port, err := existingProfileCDP(profile)
	if err != nil || port != 0 {
		t.Fatalf("expected stale lock cleaned on Windows, got port=%d err=%v", port, err)
	}
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if _, err := os.Stat(filepath.Join(profile, name)); !os.IsNotExist(err) {
			t.Fatalf("stale file %s was not removed", name)
		}
	}
}
