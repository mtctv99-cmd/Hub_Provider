package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrationAddsAPIKeyColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE api_keys (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		key_hash TEXT NOT NULL UNIQUE,
		active INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_used_at DATETIME
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	migrated, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()

	var count int
	if err := migrated.QueryRow(`SELECT count(*) FROM pragma_table_info('api_keys') WHERE name IN ('scopes','expires_at','rate_limit_rpm')`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("expected 3 migrated columns, got %d (err: %v)", count, err)
	}
}
