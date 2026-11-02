package colabsupervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// :memory: is per-connection: without a single conn, concurrent writers
	// each get their own empty database.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS identities (
			id TEXT PRIMARY KEY,
			provider_group TEXT NOT NULL,
			email TEXT UNIQUE NOT NULL,
			normalized_email TEXT,
			priority INTEGER DEFAULT 1,
			global_status TEXT NOT NULL DEFAULT 'active',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS account_cooldowns (
			identity_id TEXT PRIMARY KEY REFERENCES identities(id) ON DELETE CASCADE,
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
	_ = os.WriteFile(mockScript, []byte("import time\ntime.sleep(5)\n"), 0o755)
	sup := New(db, Config{TokenDir: tmpDir, Command: "python " + mockScript, Cooldown: time.Hour, StartGrace: 150 * time.Millisecond})

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
	sup.cfg.StartGrace = 300 * time.Millisecond

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- sup.Rotate(ctx) }()
	time.Sleep(100 * time.Millisecond)
	if err := sup.Rotate(ctx); err == nil || err.Error() != "colab rotation already in progress" {
		t.Fatalf("expected busy-guard error, got %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("first rotate should go ACTIVE after grace, got %v", err)
	}
	// Grace expiry marks the still-running worker ACTIVE.
	accs, _ := sup.DiscoverAccounts(ctx)
	found := false
	for _, a := range accs {
		if a.ID == "colab-busy1_at_gmail_com" && a.Status == "ACTIVE" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected busy1 ACTIVE after grace expiry")
	}
}

func TestParseCooldownTimeMixedEncodings(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	unixFloat := float64(future.Unix()) + 0.5

	cases := []any{
		unixFloat,            // legacy Python REAL
		int64(future.Unix()), // integer epoch
		future.UTC().Format("2006-01-02 15:04:05"), // bare DATETIME (UTC, e.g. CURRENT_TIMESTAMP)
		future.Format(time.RFC3339),                // RFC3339 string with offset
		future.String(),                            // Go String() with monotonic suffix (production bug 2026-10-13)
	}
	for i, raw := range cases {
		got, ok := parseCooldownTime(raw)
		if !ok {
			t.Fatalf("case %d (%T %v): not parsed", i, raw, raw)
		}
		if got.Location() == time.UTC {
			// Production Go String() has +0700 offset; bare DATETIME parses as UTC.
			if raw == future.String() && got.IsZero() {
				t.Fatalf("case %d: zero time", i)
			}
		}
		// Compare instants: DATETIME strings without offset parse as UTC.
		if diff := got.Unix() - future.Unix(); diff > 3600*8+60 || diff < -60 {
			t.Fatalf("case %d: got unix %d, want ~%d", i, got.Unix(), future.Unix())
		}
	}
	// Bug guard: Go datetime strings must not parse as year-1970 epoch 2026.
	if got, ok := parseCooldownTime(future.String()); !ok || got.Year() < 2000 {
		t.Fatalf("monotonic string misparsed: %v %v", got, ok)
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
	_ = os.WriteFile(mockScript, []byte("import time\ntime.sleep(2)\n"), 0o755)
	sup.cfg.Command = "python " + mockScript
	sup.cfg.StartGrace = 200 * time.Millisecond
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

// writeMarkerScript writes a python mock that appends $COLAB_ACTION to marker.
func writeMarkerScript(t *testing.T, dir, marker string) string {
	t.Helper()
	script := filepath.Join(dir, "marker_runner.py")
	body := fmt.Sprintf("import os, time\nopen(%q, 'a').write(os.environ.get('COLAB_ACTION', '?') + chr(10))\ntime.sleep(2)\n", marker)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func cooldownOf(t *testing.T, sup *Supervisor, accountID string) *Account {
	t.Helper()
	accs, err := sup.DiscoverAccounts(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	for i := range accs {
		if accs[i].ID == accountID {
			return &accs[i]
		}
	}
	t.Fatalf("account %s not discovered", accountID)
	return nil
}

func assertCooldownAtLeast(t *testing.T, sup *Supervisor, accountID string, min time.Duration) {
	t.Helper()
	acc := cooldownOf(t, sup, accountID)
	if acc.Status != "COOLDOWN" || acc.CooldownUntil == nil {
		t.Fatalf("%s: expected COOLDOWN, got %s (%v)", accountID, acc.Status, acc.CooldownUntil)
	}
	if left := time.Until(*acc.CooldownUntil); left < min {
		t.Fatalf("%s: cooldown shortened to %v, want at least %v", accountID, left, min)
	}
}

func waitForStatus(t *testing.T, sup *Supervisor, accountID, want string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		acc := cooldownOf(t, sup, accountID)
		if acc.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: status %s, want %s within %v", accountID, acc.Status, want, limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCommandTimeoutByAction(t *testing.T) {
	db := setupTestDB(t)
	sup := New(db, Config{})

	if got, want := sup.commandTimeout("start"), 4*time.Hour+30*time.Minute; got != want {
		t.Fatalf("start timeout = %v, want %v", got, want)
	}
	if sup.commandTimeout("start") <= sup.cfg.MaxRuntime {
		t.Fatalf("start timeout %v must outlive MaxRuntime %v", sup.commandTimeout("start"), sup.cfg.MaxRuntime)
	}
	if got := sup.commandTimeout("stop"); got != 5*time.Minute {
		t.Fatalf("stop timeout = %v, want 5m", got)
	}

	capped := New(db, Config{CommandTimeout: 200 * time.Millisecond})
	if got := capped.commandTimeout("stop"); got != 200*time.Millisecond {
		t.Fatalf("configured stop timeout = %v, want 200ms", got)
	}
	if got := capped.commandTimeout("start"); got != 4*time.Hour+30*time.Minute {
		t.Fatalf("start timeout must ignore CommandTimeout, got %v", got)
	}
}

// A start wrapper that exits with status 0 must not leave the account ACTIVE:
// that stale row makes the UI and the supervisor believe a dead worker is alive
// until MaxRuntime (4h) expires.
func TestCleanStartExitDoesNotMarkActive(t *testing.T) {
	db := setupTestDB(t)
	tmpDir := t.TempDir()
	accountID := "colab-clean_at_gmail_com"
	_ = os.WriteFile(filepath.Join(tmpDir, "token_clean_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	script := filepath.Join(tmpDir, "clean_runner.py")
	if err := os.WriteFile(script, []byte("import sys\nsys.exit(0)\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sup := New(db, Config{TokenDir: tmpDir, Command: "python " + script, Cooldown: time.Hour, StartGrace: 5 * time.Second})

	if err := sup.Rotate(context.Background()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		acc := cooldownOf(t, sup, accountID)
		if acc.LastAttempt != nil {
			if acc.Status == "ACTIVE" {
				t.Fatal("clean exit must not leave the account ACTIVE")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no cooldown row written for the clean exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A restart kills the workers this supervisor spawned, so any ACTIVE row it
// finds at boot is stale and must not block the first bootstrap.
func TestBootClearsStaleActive(t *testing.T) {
	db := setupTestDB(t)
	tmpDir := t.TempDir()
	accountID := "colab-stale_at_gmail_com"
	_ = os.WriteFile(filepath.Join(tmpDir, "token_stale_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	sup := New(db, Config{TokenDir: tmpDir, Cooldown: time.Hour})
	ctx := context.Background()

	sup.ensureIdentity(ctx, accountID)
	if _, err := db.Exec(`INSERT INTO account_cooldowns(identity_id, state, updated_at) VALUES (?, 'ACTIVE', CURRENT_TIMESTAMP)`, accountID); err != nil {
		t.Fatal(err)
	}
	if acc := cooldownOf(t, sup, accountID); acc.Status != "ACTIVE" {
		t.Fatalf("setup: status %s, want ACTIVE", acc.Status)
	}
	if err := sup.clearStaleActive(ctx); err != nil {
		t.Fatal(err)
	}
	if acc := cooldownOf(t, sup, accountID); acc.Status != "READY" {
		t.Fatalf("stale ACTIVE must be cleared at boot, got %s", acc.Status)
	}
}

func TestStartSurvivesCommandTimeout(t *testing.T) {
	db := setupTestDB(t)
	tmpDir := t.TempDir()
	accountID := "colab-slow_at_gmail_com"
	_ = os.WriteFile(filepath.Join(tmpDir, "token_slow_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)

	script := filepath.Join(tmpDir, "slow_runner.py")
	if err := os.WriteFile(script, []byte("import time\ntime.sleep(5)\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sup := New(db, Config{
		TokenDir:       tmpDir,
		Command:        "python " + script,
		CommandTimeout: 200 * time.Millisecond,
		StartGrace:     2500 * time.Millisecond,
		Cooldown:       time.Hour,
	})
	ctx := context.Background()
	if err := sup.Rotate(ctx); err != nil {
		t.Fatalf("rotate should survive the 200ms CommandTimeout, got %v", err)
	}
	waitForStatus(t, sup, accountID, "ACTIVE", 3*time.Second)

	var cooldowns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM account_cooldowns WHERE state='COOLDOWN'`).Scan(&cooldowns); err != nil {
		t.Fatal(err)
	}
	if cooldowns != 0 {
		t.Fatalf("healthy worker must not be cooled down, got %d COOLDOWN rows", cooldowns)
	}

	t.Run("stop still honours CommandTimeout", func(t *testing.T) {
		start := time.Now()
		err := sup.runCommand(ctx, "stop", accountID)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", err)
		}
		if elapsed := time.Since(start); elapsed >= time.Second {
			t.Fatalf("stop ran %v, expected the 200ms cap", elapsed)
		}
	})
}

func TestCooldownNeverShortened(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	for _, name := range []string{"keep", "rev", "race", "ms", "mspast", "text", "textpast"} {
		_ = os.WriteFile(filepath.Join(tmpDir, "token_"+name+"_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	}
	sup := New(db, Config{TokenDir: tmpDir, Cooldown: 16 * time.Hour})

	// Long then short.
	id := "colab-keep_at_gmail_com"
	if err := sup.RecordCooldown(ctx, id, 16*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := sup.RecordCooldown(ctx, id, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	assertCooldownAtLeast(t, sup, id, 15*time.Hour+59*time.Minute)

	// Short then long still ends long.
	idRev := "colab-rev_at_gmail_com"
	_ = sup.RecordCooldown(ctx, idRev, 30*time.Minute)
	_ = sup.RecordCooldown(ctx, idRev, 16*time.Hour)
	assertCooldownAtLeast(t, sup, idRev, 15*time.Hour+59*time.Minute)

	// Concurrent interleaving must not lose the long write (TOCTOU guard).
	idRace := "colab-race_at_gmail_com"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := 16 * time.Hour
			if i%2 == 1 {
				d = 30 * time.Minute
			}
			_ = sup.RecordCooldown(ctx, idRace, d)
		}(i)
	}
	wg.Wait()
	assertCooldownAtLeast(t, sup, idRace, 15*time.Hour+59*time.Minute)

	// Millisecond epoch in the future must survive a shorter text write.
	idMs := "colab-ms_at_gmail_com"
	_ = sup.RecordCooldown(ctx, idMs, 0)
	ms := time.Now().Add(16 * time.Hour).UnixMilli()
	if _, err := db.Exec(`UPDATE account_cooldowns SET cooldown_until=? WHERE identity_id=?`, ms, idMs); err != nil {
		t.Fatal(err)
	}
	_ = sup.RecordCooldown(ctx, idMs, 30*time.Minute)
	assertCooldownAtLeast(t, sup, idMs, 15*time.Hour+59*time.Minute)
	var storedMs int64
	if err := db.QueryRow(`SELECT CAST(cooldown_until AS INTEGER) FROM account_cooldowns WHERE identity_id=?`, idMs).Scan(&storedMs); err != nil {
		t.Fatal(err)
	}
	if storedMs != ms {
		t.Fatalf("ms row was rewritten: %d, want %d", storedMs, ms)
	}

	// Past millisecond epoch is stale: a fresh 30m write wins.
	idMsPast := "colab-mspast_at_gmail_com"
	_ = sup.RecordCooldown(ctx, idMsPast, 0)
	pastMs := time.Now().Add(-time.Hour).UnixMilli()
	if _, err := db.Exec(`UPDATE account_cooldowns SET cooldown_until=? WHERE identity_id=?`, pastMs, idMsPast); err != nil {
		t.Fatal(err)
	}
	_ = sup.RecordCooldown(ctx, idMsPast, 30*time.Minute)
	acc := cooldownOf(t, sup, idMsPast)
	if acc.Status != "COOLDOWN" || acc.CooldownUntil == nil || time.Until(*acc.CooldownUntil) > 31*time.Minute {
		t.Fatalf("stale ms cooldown not replaced: %v %v", acc.Status, acc.CooldownUntil)
	}

	// Text-numeric epoch (Go reader accepts it) must win comparisons too.
	idText := "colab-text_at_gmail_com"
	_ = sup.RecordCooldown(ctx, idText, 0)
	textFuture := strconv.FormatInt(time.Now().Add(16*time.Hour).Unix(), 10)
	if _, err := db.Exec(`UPDATE account_cooldowns SET cooldown_until=? WHERE identity_id=?`, textFuture, idText); err != nil {
		t.Fatal(err)
	}
	_ = sup.RecordCooldown(ctx, idText, 30*time.Minute)
	assertCooldownAtLeast(t, sup, idText, 15*time.Hour+59*time.Minute)

	idTextPast := "colab-textpast_at_gmail_com"
	_ = sup.RecordCooldown(ctx, idTextPast, 0)
	if _, err := db.Exec(`UPDATE account_cooldowns SET cooldown_until='1700000000' WHERE identity_id=?`, idTextPast); err != nil {
		t.Fatal(err)
	}
	_ = sup.RecordCooldown(ctx, idTextPast, 30*time.Minute)
	acc = cooldownOf(t, sup, idTextPast)
	if acc.Status != "COOLDOWN" || acc.CooldownUntil == nil || time.Until(*acc.CooldownUntil) > 31*time.Minute {
		t.Fatalf("stale text epoch not replaced: %v %v", acc.Status, acc.CooldownUntil)
	}

	// RecordActive must not clear a future cooldown.
	if err := sup.RecordActive(ctx, id); err != nil {
		t.Fatal(err)
	}
	assertCooldownAtLeast(t, sup, id, 15*time.Hour+59*time.Minute)

	// StartAccount must refuse the cooling-down account and keep the cooldown.
	marker := filepath.Join(tmpDir, "marker.txt")
	script := writeMarkerScript(t, tmpDir, marker)
	starter := New(db, Config{TokenDir: tmpDir, Command: "python " + script, Cooldown: 16 * time.Hour})
	err := starter.StartAccount(ctx, id)
	if err == nil || !strings.Contains(err.Error(), "not READY") {
		t.Fatalf("expected rejection of COOLDOWN account, got %v", err)
	}
	assertCooldownAtLeast(t, sup, id, 15*time.Hour+59*time.Minute)
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("supervisor command must not run for a COOLDOWN account")
	}
}

// Only the accountID key proves a live worker; legacy name/email-key ACTIVE
// rows must be ignored even when they are newer.
func TestActiveOnlyTrustsAccountKey(t *testing.T) {
	db := setupTestDB(t)
	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "token_acct_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	_ = os.WriteFile(filepath.Join(tmpDir, "token_other_at_gmail_com.json"), []byte(`{"token":"y"}`), 0o600)
	marker := filepath.Join(tmpDir, "marker.txt")
	script := writeMarkerScript(t, tmpDir, marker)
	sup := New(db, Config{TokenDir: tmpDir, Command: "python " + script, Cooldown: time.Hour})
	ctx := context.Background()

	accountID := "colab-acct_at_gmail_com"
	sup.ensureIdentity(ctx, accountID)
	sup.ensureIdentity(ctx, "acct_at_gmail_com")
	sup.ensureIdentity(ctx, "acct@gmail.com")
	trusted := time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05")
	legacyFresh := time.Now().Add(-10 * time.Second).UTC().Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at) VALUES (?, 'ACTIVE', NULL, ?)`, accountID, trusted); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at) VALUES ('acct_at_gmail_com', 'ACTIVE', NULL, ?)`, legacyFresh); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at) VALUES ('acct@gmail.com', 'ACTIVE', NULL, ?)`, legacyFresh); err != nil {
		t.Fatal(err)
	}

	acc := cooldownOf(t, sup, accountID)
	if acc.Status != "ACTIVE" || acc.ActiveSince == nil {
		t.Fatalf("expected ACTIVE with ActiveSince, got %s %v", acc.Status, acc.ActiveSince)
	}
	if age := time.Since(*acc.ActiveSince); age > 2*time.Minute || age < 30*time.Second {
		t.Fatalf("ActiveSince age = %v, want ~1m from the accountID row", age)
	}

	sup.check(ctx) // must not rotate: the trusted row is inside MaxRuntime
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("check rotated despite an ACTIVE worker inside MaxRuntime")
	}
}

// Legacy rotator rows: expired cooldown plus a stale email-key ACTIVE row must
// read as READY so bootstrap can start the only account again.
func TestLegacyEmailActiveDoesNotResurrect(t *testing.T) {
	db := setupTestDB(t)
	tmpDir := t.TempDir()
	accountID := "colab-acct_at_gmail_com"
	_ = os.WriteFile(filepath.Join(tmpDir, "token_acct_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	marker := filepath.Join(tmpDir, "marker.txt")
	script := writeMarkerScript(t, tmpDir, marker)
	sup := New(db, Config{TokenDir: tmpDir, Command: "python " + script, Cooldown: 16 * time.Hour, StartGrace: 100 * time.Millisecond})
	ctx := context.Background()

	sup.ensureIdentity(ctx, accountID)
	sup.ensureIdentity(ctx, "acct@gmail.com")
	stale := time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05")
	past := time.Now().Add(-time.Second).UTC().Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at) VALUES (?, 'COOLDOWN', ?, ?)`, accountID, past, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at) VALUES ('acct@gmail.com', 'ACTIVE', NULL, ?)`, stale); err != nil {
		t.Fatal(err)
	}

	if acc := cooldownOf(t, sup, accountID); acc.Status != "READY" {
		t.Fatalf("expired cooldown + stale email ACTIVE must be READY, got %s", acc.Status)
	}

	sup.check(ctx) // single account must bootstrap instead of ErrNoAccountAvailable
	waitForStatus(t, sup, accountID, "ACTIVE", 2*time.Second)
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Fatalf("check never ran the start command")
	}
}

func TestServiceUnavailableShortPenalty(t *testing.T) {
	db := setupTestDB(t)
	tmpDir := t.TempDir()
	accountID := "colab-cap_at_gmail_com"
	_ = os.WriteFile(filepath.Join(tmpDir, "token_cap_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	script := filepath.Join(tmpDir, "cap_runner.py")
	if err := os.WriteFile(script, []byte("import sys\nprint('Service Unavailable', file=sys.stderr)\nsys.exit(1)\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sup := New(db, Config{
		TokenDir:   tmpDir,
		Command:    "python " + script,
		Cooldown:   16 * time.Hour,
		StartGrace: 3 * time.Second,
	})
	ctx := context.Background()
	err := sup.Rotate(ctx)
	if err == nil || !strings.Contains(err.Error(), "Service Unavailable") {
		t.Fatalf("expected capacity failure, got %v", err)
	}
	// The row is written by the background goroutine after Rotate reports.
	var acc *Account
	deadline := time.Now().Add(3 * time.Second)
	for {
		acc = cooldownOf(t, sup, accountID)
		if acc.LastAttempt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no cooldown row written for the capacity failure")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if acc.Status == "COOLDOWN" && acc.CooldownUntil != nil {
		if left := time.Until(*acc.CooldownUntil); left > 2*time.Minute {
			t.Fatalf("capacity error must not bench the account for long, got %v", left)
		}
	}
	if got := sup.failStreak.Load(); got != 1 {
		t.Fatalf("failStreak = %d, want 1", got)
	}
	backoff := time.Until(time.Unix(0, sup.retryAt.Load()))
	if backoff <= 0 || backoff > 2*time.Minute+10*time.Second {
		t.Fatalf("retryAt backoff = %v, want ~2m", backoff)
	}

	if got := sup.penaltyFor(errors.New("503 Service Unavailable")); got != 0 {
		t.Fatalf("capacity penalty = %v, want 0 (global retry paces it)", got)
	}
	if got := sup.penaltyFor(fmt.Errorf("worker cancelled: %w", context.Canceled)); got != 0 {
		t.Fatalf("cancel penalty = %v, want 0", got)
	}
	if got := sup.penaltyFor(errors.New("bad token")); got != 30*time.Minute {
		t.Fatalf("generic penalty = %v, want 30m", got)
	}
	if got := sup.penaltyFor(context.DeadlineExceeded); got != 30*time.Minute {
		t.Fatalf("deadline penalty = %v, want 30m", got)
	}
}

func TestStartAccountUnknownID(t *testing.T) {
	db := setupTestDB(t)
	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "token_real_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	marker := filepath.Join(tmpDir, "marker.txt")
	script := writeMarkerScript(t, tmpDir, marker)
	sup := New(db, Config{TokenDir: tmpDir, Command: "python " + script, Cooldown: time.Hour})

	err := sup.StartAccount(context.Background(), "colab-khong-ton-tai")
	if err == nil || !strings.Contains(err.Error(), "unknown colab account") {
		t.Fatalf("expected unknown account error, got %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("supervisor command must not run for an unknown account")
	}
}

// A capacity error benches one account; the next rotation must ask a different
// account instead of retrying the same one until its penalty expires.
func TestRotateSpreadsAttempts(t *testing.T) {
	db := setupTestDB(t)
	tmpDir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		_ = os.WriteFile(filepath.Join(tmpDir, "token_"+name+"_at_gmail_com.json"), []byte(`{"token":"x"}`), 0o600)
	}
	marker := filepath.Join(tmpDir, "started.txt")
	script := filepath.Join(tmpDir, "who_runner.py")
	body := fmt.Sprintf("import os, time\nopen(%q, 'a').write(os.environ.get('COLAB_ACCOUNT_ID', '?') + chr(10))\ntime.sleep(2)\n", marker)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	sup := New(db, Config{TokenDir: tmpDir, Command: "python " + script, Cooldown: time.Hour, StartGrace: 150 * time.Millisecond})

	ctx := context.Background()
	// Account A was just attempted and benched; its cooldown has expired again.
	if err := sup.RecordCooldown(ctx, "colab-a_at_gmail_com", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // updated_at has one-second resolution

	if err := sup.Rotate(ctx); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if started := strings.TrimSpace(string(raw)); started != "colab-b_at_gmail_com" {
		t.Fatalf("rotation must prefer the least recently attempted account, started %q", started)
	}
}
