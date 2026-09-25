package settings

import (
	"context"
	"database/sql"
	"strings"
	"sync"
)

type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

func New(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Get(ctx context.Context, key, fallback string) string {
	if s == nil || s.db == nil {
		return fallback
	}
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key=?`, key).Scan(&value); err != nil {
		return fallback
	}
	return value
}

func (s *Store) Set(ctx context.Context, key, value string) error {
	if s == nil || s.db == nil {
		return sql.ErrConnDone
	}
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if key == "" {
		return sql.ErrNoRows
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runtime_settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=CURRENT_TIMESTAMP`, key, value)
	return err
}

func (s *Store) GetBool(ctx context.Context, key string, fallback bool) bool {
	value := strings.ToLower(s.Get(ctx, key, ""))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func (s *Store) SetBool(ctx context.Context, key string, value bool) error {
	if value {
		return s.Set(ctx, key, "true")
	}
	return s.Set(ctx, key, "false")
}
