package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenMigratesSchemaAndEnablesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orchestrator.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("expected WAL mode, got %q", mode)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='credentials'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("credentials table missing")
	}
}

func TestDBUsesBoundedReadPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Stats().MaxOpenConnections != 4 {
		t.Fatalf("expected four max connections, got %d", db.Stats().MaxOpenConnections)
	}
	_ = sql.ErrNoRows
}
