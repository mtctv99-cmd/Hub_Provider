package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// DB wraps the SQLite handle. The core process owns all mutations.
type DB struct {
	*sql.DB
}

func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" {
		path = "data/orchestrator.db"
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	h, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One writer keeps transaction ownership explicit; SQLite still allows readers.
	h.SetMaxOpenConns(1)
	h.SetMaxIdleConns(1)

	db := &DB{DB: h}
	if err := db.PingContext(ctx); err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := db.Migrate(ctx); err != nil {
		_ = h.Close()
		return nil, err
	}
	return db, nil
}

func (d *DB) Migrate(ctx context.Context) error {
	data, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	if _, err := d.ExecContext(ctx, string(data)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS does not add columns to databases created by
	// older versions. Apply additive migrations explicitly and ignore only the
	// harmless duplicate-column error.
	for _, stmt := range []string{
		`ALTER TABLE identities ADD COLUMN normalized_email TEXT`,
		`ALTER TABLE credentials ADD COLUMN source_ref TEXT`,
		`ALTER TABLE credentials ADD COLUMN fingerprint TEXT`,
		`ALTER TABLE credentials ADD COLUMN pool_role TEXT NOT NULL DEFAULT 'AUTO'`,
		`ALTER TABLE credentials ADD COLUMN last_error_kind TEXT`,
		`ALTER TABLE api_keys ADD COLUMN scopes TEXT NOT NULL DEFAULT 'all'`,
		`ALTER TABLE api_keys ADD COLUMN expires_at DATETIME`,
		`ALTER TABLE api_keys ADD COLUMN rate_limit_rpm INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := d.ExecContext(ctx, stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("apply additive migration %q: %w", stmt, err)
		}
	}
	if _, err := d.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS admin_credentials (
			id TEXT PRIMARY KEY,
			password_hash TEXT NOT NULL,
			salt TEXT NOT NULL,
			must_change_password INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			password_changed_at DATETIME
		)`); err != nil {
		return fmt.Errorf("create admin_credentials table: %w", err)
	}
	if _, err := d.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_identities_normalized_email ON identities(normalized_email) WHERE normalized_email IS NOT NULL`); err != nil {
		return fmt.Errorf("index normalized identity email: %w", err)
	}
	if _, err := d.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_credentials_fingerprint ON credentials(fingerprint) WHERE fingerprint IS NOT NULL AND fingerprint <> ''`); err != nil {
		return fmt.Errorf("index credential fingerprint: %w", err)
	}
	return nil
}

func (d *DB) Close() error { return d.DB.Close() }

func (d *DB) Health(ctx context.Context) error {
	var result int
	return d.QueryRowContext(ctx, "SELECT 1").Scan(&result)
}

func NormalizePath(path string) string {
	return strings.TrimSpace(path)
}
