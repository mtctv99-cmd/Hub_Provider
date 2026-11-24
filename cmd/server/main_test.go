package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestCountAPIKeys(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE api_keys (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	count, err := countAPIKeys(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("empty table count = %d, want 0", count)
	}
	if _, err := db.Exec(`INSERT INTO api_keys(id) VALUES ('key-1')`); err != nil {
		t.Fatal(err)
	}
	count, err = countAPIKeys(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

// A missing table must be an error so startup fails closed rather than treating
// an unreadable key store as "keys exist".
func TestCountAPIKeysMissingTable(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := countAPIKeys(context.Background(), db); err == nil {
		t.Fatal("expected an error for a missing api_keys table")
	}
}
