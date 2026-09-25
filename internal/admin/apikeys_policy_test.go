package admin

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAPIKeyPolicyAndRateLimit(t *testing.T) {
	db := setupTestDB(t)
	s := NewAPIKeys(db)
	meta, plain, err := s.CreateWithOptions(CreateKeySpec{Name: "chat-only", Scopes: []string{"chat"}, ExpiresInDays: 7, RateLimitRPM: 3})
	if err != nil {
		t.Fatal(err)
	}
	if meta.ExpiresAt == nil || len(meta.Scopes) != 1 || meta.Scopes[0] != "chat" {
		t.Fatalf("metadata: %+v", meta)
	}
	if _, err := s.AuthenticateWithScope(plain, "images"); !errors.Is(err, ErrInsufficientScope) {
		t.Fatalf("expected scope denial: %v", err)
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.AuthenticateWithScope(plain, "chat"); outcomes <- err }()
	}
	wg.Wait()
	close(outcomes)
	allowed, limited := 0, 0
	for result := range outcomes {
		switch {
		case result == nil:
			allowed++
		case errors.Is(result, ErrRateLimit):
			limited++
		default:
			t.Fatalf("unexpected result: %v", result)
		}
	}
	if allowed != 3 || limited != 3 {
		t.Fatalf("allowed=%d limited=%d", allowed, limited)
	}
	if err := s.Revoke(meta.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateWithScope(plain, "chat"); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("expected window to persist: %v", err)
	}
	if _, err := db.Exec(`UPDATE api_keys SET expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC(), meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateWithScope(plain, "chat"); !errors.Is(err, ErrExpiredAPIKey) {
		t.Fatalf("expected expiry: %v", err)
	}
	if err := s.Activate(meta.ID); !errors.Is(err, ErrExpiredAPIKey) {
		t.Fatalf("expected expired key conflict: %v", err)
	}
}

func TestAPIKeyPolicyValidation(t *testing.T) {
	s := NewAPIKeys(setupTestDB(t))
	for _, spec := range []CreateKeySpec{{Name: " "}, {Name: "x", Scopes: []string{"bogus"}}, {Name: "x", Scopes: []string{"all", "chat"}}, {Name: "x", RateLimitRPM: -1}, {Name: "x", ExpiresInDays: -1}} {
		if _, _, err := s.CreateWithOptions(spec); !errors.Is(err, ErrInvalidKeyOptions) {
			t.Fatalf("expected validation for %+v: %v", spec, err)
		}
	}
}
