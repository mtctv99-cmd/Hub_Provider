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

func TestColabSupervisorBootstrap(t *testing.T) {
	db := setupTestDB(t)
	tmpDir, err := os.MkdirTemp("", "colab-bootstrap-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	_ = os.WriteFile(filepath.Join(tmpDir, "token_boot1_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	_ = os.WriteFile(filepath.Join(tmpDir, "token_boot2_at_gmail_com.json"), []byte(`{"token":"y"}`), 0o600)

	mockScript := filepath.Join(tmpDir, "mock_runner.py")
	_ = os.WriteFile(mockScript, []byte("import sys\nsys.exit(0)\n"), 0o755)
	sup := New(db, Config{TokenDir: tmpDir, Command: "python " + mockScript, Cooldown: time.Hour})

	// No ACTIVE accounts: check() must bootstrap the first READY one.
	ctx := context.Background()
	sup.check(ctx)
	accs, _ := sup.DiscoverAccounts(ctx)
	active := 0
	for _, a := range accs {
		if a.Status == "ACTIVE" {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("expected bootstrap to activate exactly 1 account, got %d", active)
	}

	// All accounts in cooldown and none ACTIVE: bootstrap must not fail loudly.
	for _, a := range accs {
		_ = sup.RecordCooldown(ctx, a.ID, time.Hour)
	}
	sup.check(ctx) // must not panic or activate anything
	accs, _ = sup.DiscoverAccounts(ctx)
	for _, a := range accs {
		if a.Status == "ACTIVE" {
			t.Fatalf("no account should activate while all in cooldown, got %s", a.ID)
		}
	}
}

func TestColabSupervisorBusyGuard(t *testing.T) {
	db := setupTestDB(t)
	tmpDir, err := os.MkdirTemp("", "colab-busy-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	_ = os.WriteFile(filepath.Join(tmpDir, "token_busy1_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	blockScript := filepath.Join(tmpDir, "block_runner.py")
	_ = os.WriteFile(blockScript, []byte("import time\ntime.sleep(5)\n"), 0o755)
	sup := New(db, Config{TokenDir: tmpDir, Command: "python " + blockScript, Cooldown: time.Hour})

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- sup.Rotate(ctx) }()
	time.Sleep(300 * time.Millisecond)
	if err := sup.Rotate(ctx); err == nil || err.Error() != "colab rotation already in progress" {
		t.Fatalf("expected busy-guard error, got %v", err)
	}
	<-done
}

func TestParseCooldownTimeMixedEncodings(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	unixFloat := float64(future.Unix()) + 0.5

	cases := []any{
		unixFloat,                            // legacy Python REAL
		int64(future.Unix()),                 // integer epoch
		future.UTC().Format("2006-01-02 15:04:05"), // bare DATETIME (UTC, e.g. CURRENT_TIMESTAMP)
		future.Format(time.RFC3339),          // RFC3339 string with offset
	}
	for i, raw := range cases {
		got, ok := parseCooldownTime(raw)
		if !ok {
			t.Fatalf("case %d (%T): not parsed", i, raw)
		}
		// Compare instants: DATETIME strings without offset parse as UTC.
		if diff := got.Unix() - future.Unix(); diff > 60 || diff < -60 {
			t.Fatalf("case %d: got unix %d, want ~%d", i, got.Unix(), future.Unix())
		}
	}
	for _, raw := range []any{nil, "", "not-a-time", float64(-1)} {
		if _, ok := parseCooldownTime(raw); ok {
			t.Fatalf("expected parse failure for %v", raw)
		}
	}
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

	// 4. Test _at_ format filename
	f3 := filepath.Join(tmpDir, "token_user3_at_gmail_com.json")
	_ = os.WriteFile(f3, []byte(`{"token":"dummy3"}`), 0o600)
	accs, _ = sup.DiscoverAccounts(ctx)
	foundUser3 := false
	for _, a := range accs {
		if a.ID == "colab-user3_at_gmail_com" {
			foundUser3 = true
			if a.Email != "user3@gmail.com" {
				t.Errorf("expected email user3@gmail.com, got %s", a.Email)
			}
		}
	}
	if !foundUser3 {
		t.Fatal("user3 not discovered")
	}

	// 5. Test RecordActive
	err = sup.RecordActive(ctx, "colab-user3_at_gmail_com")
	if err != nil {
		t.Fatalf("record active failed: %v", err)
	}
	accs, _ = sup.DiscoverAccounts(ctx)
	for _, a := range accs {
		if a.ID == "colab-user3_at_gmail_com" {
			if a.Status != "ACTIVE" || a.ActiveSince == nil {
				t.Errorf("expected user3 ACTIVE with ActiveSince, got status=%s, since=%v", a.Status, a.ActiveSince)
			}
		}
	}

	// 6. Test Rotate: user3 is ACTIVE, account2 is READY.
	// Rotate should retire user3 into COOLDOWN and activate account2.
	mockScript := filepath.Join(tmpDir, "mock_runner.py")
	_ = os.WriteFile(mockScript, []byte("import sys\nsys.exit(0)\n"), 0o755)
	sup.cfg.Command = "python " + mockScript
	err = sup.Rotate(ctx)
	if err != nil {
		t.Fatalf("rotate failed: %v", err)
	}
	accs, _ = sup.DiscoverAccounts(ctx)
	for _, a := range accs {
		if a.ID == "colab-user3_at_gmail_com" {
			if a.Status != "COOLDOWN" {
				t.Errorf("expected user3 to be COOLDOWN after rotation, got %s", a.Status)
			}
		}
		if a.ID == "colab-account2@gmail.com" {
			if a.Status != "ACTIVE" {
				t.Errorf("expected account2 to be ACTIVE after rotation, got %s", a.Status)
			}
		}
	}
}
