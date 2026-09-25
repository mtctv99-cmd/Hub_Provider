package colabsupervisor

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS account_cooldowns (
			identity_id TEXT PRIMARY KEY,
			state TEXT NOT NULL DEFAULT 'READY',
			cooldown_until DATETIME,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func TestColabSupervisorLifecycle(t *testing.T) {
	db := setupTestDB(t)
	tmpDir, err := os.MkdirTemp("", "colab-tokens-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create 2 mock token files
	f1 := filepath.Join(tmpDir, "token_account1@gmail.com.json")
	f2 := filepath.Join(tmpDir, "token_account2@gmail.com.json")
	_ = os.WriteFile(f1, []byte(`{"token":"dummy1"}`), 0o600)
	_ = os.WriteFile(f2, []byte(`{"token":"dummy2"}`), 0o600)

	cfg := Config{
		TokenDir: tmpDir,
		Cooldown: 2 * time.Hour,
	}
	sup := New(db, cfg)

	// 1. Discover accounts
	ctx := context.Background()
	accs, err := sup.DiscoverAccounts(ctx)
	if err != nil {
		t.Fatalf("discover failed: %v", err)
	}
	if len(accs) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accs))
	}
	for _, a := range accs {
		if a.Status != "READY" {
			t.Errorf("expected READY status, got %s", a.Status)
		}
	}

	// 2. Put account1 into cooldown
	err = sup.RecordCooldown(ctx, "colab-account1@gmail.com", 2*time.Hour)
	if err != nil {
		t.Fatalf("record cooldown failed: %v", err)
	}

	// 3. Re-discover accounts: account1 should be COOLDOWN, account2 should be READY
	accs, err = sup.DiscoverAccounts(ctx)
	if err != nil {
		t.Fatalf("rediscover failed: %v", err)
	}
	for _, a := range accs {
		if a.ID == "colab-account1@gmail.com" {
			if a.Status != "COOLDOWN" || a.CooldownUntil == nil {
				t.Errorf("expected account1 in COOLDOWN, got status=%s, until=%v", a.Status, a.CooldownUntil)
			}
		} else if a.ID == "colab-account2@gmail.com" {
			if a.Status != "READY" {
				t.Errorf("expected account2 READY, got status=%s", a.Status)
			}
		}
	}
}
