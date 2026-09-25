package admin

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	schema := `
	CREATE TABLE IF NOT EXISTS api_keys (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		key_hash TEXT NOT NULL UNIQUE,
		active INTEGER NOT NULL DEFAULT 1,
		scopes TEXT NOT NULL DEFAULT 'all',
		expires_at DATETIME,
		rate_limit_rpm INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_used_at DATETIME
	);
	CREATE TABLE IF NOT EXISTS api_key_rate_windows (
		key_id TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
		window_start INTEGER NOT NULL,
		request_count INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(key_id, window_start)
	);
	CREATE TABLE IF NOT EXISTS observability_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT,
		event_type TEXT NOT NULL,
		provider TEXT,
		model TEXT,
		account_id TEXT,
		latency_ms REAL,
		error_kind TEXT,
		detail TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to create schema: %v", err)
	}
	return db
}

func TestAPIKeysLifecycle(t *testing.T) {
	db := setupTestDB(t)
	store := NewAPIKeys(db)

	// Create key
	meta, plain, err := store.Create("test-service")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if meta.ID == "" || meta.Name != "test-service" || !meta.Active {
		t.Errorf("unexpected meta: %+v", meta)
	}
	if plain == "" || len(plain) < 10 {
		t.Errorf("unexpected plain key: %q", plain)
	}

	// Verify plaintext is NOT stored in the database
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM api_keys WHERE key_hash = ?", plain).Scan(&count)
	if err != nil {
		t.Fatalf("db query failed: %v", err)
	}
	if count != 0 {
		t.Errorf("plaintext key was stored in the database!")
	}

	// Authenticate valid key
	if !store.Authenticate(plain) {
		t.Errorf("Authenticate failed for valid key")
	}

	// Authenticate invalid key
	if store.Authenticate("invalid-key-xyz") {
		t.Errorf("Authenticate succeeded for invalid key")
	}

	// List keys
	keys, err := store.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != meta.ID {
		t.Errorf("unexpected keys list: %+v", keys)
	}
	if keys[0].LastUsedAt == nil {
		t.Errorf("LastUsedAt was not updated on authentication")
	}

	// Revoke key
	if err := store.Revoke(meta.ID); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}
	if store.Authenticate(plain) {
		t.Errorf("Authenticate succeeded for revoked key")
	}

	// Delete key
	if err := store.Delete(meta.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	keys, err = store.List()
	if err != nil {
		t.Fatalf("List after delete failed: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected 0 keys after delete, got %d", len(keys))
	}
}

func TestAuditLogger(t *testing.T) {
	db := setupTestDB(t)
	logger := NewAuditLogger(db)
	ctx := context.Background()

	err := logger.Record(ctx, AuditEvent{
		RequestID: "req-1",
		EventType: "chat_completion",
		Provider:  "gemini_web",
		Model:     "gemini-2.5-flash",
		AccountID: "acc-1",
		ErrorKind: "",
		Detail:    "completed successfully",
	})
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}

	lines, err := logger.Recent(ctx, 10)
	if err != nil {
		t.Fatalf("Recent failed: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0].EventType != "chat_completion" || lines[0].Provider != "gemini_web" {
		t.Errorf("unexpected audit line: %+v", lines[0])
	}

	// Clear logs
	if err := logger.Clear(ctx); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}
	lines, err = logger.Recent(ctx, 10)
	if err != nil {
		t.Fatalf("Recent after clear failed: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected 0 lines after clear, got %d", len(lines))
	}
}
