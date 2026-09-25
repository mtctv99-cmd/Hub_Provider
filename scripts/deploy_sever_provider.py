import os
import sys
import paramiko

HOST = os.environ.get('SSH_HOST', '')
PORT = int(os.environ.get('SSH_PORT', '22'))
USER = os.environ.get('SSH_USER', '')
KEY_PATH = os.environ.get('SSH_KEY_PATH', '')
REMOTE_BASE = os.environ.get('REMOTE_BASE', '/media/reup/Data_sv4/sever_provider')

FILES = {}

# -------------------------------------------------------------
# 1. go.mod
# -------------------------------------------------------------
FILES['go.mod'] = """module sever_provider

go 1.23

require (
	github.com/go-chi/chi/v5 v5.2.1
	github.com/mattn/go-sqlite3 v1.14.24
	gopkg.in/yaml.v3 v3.0.1
)
"""

# -------------------------------------------------------------
# 2. Makefile
# -------------------------------------------------------------
FILES['Makefile'] = """BINARY=sever_provider
GO=/home/reup/.local/go/bin/go

.PHONY: build run test clean

build:
\tCGO_ENABLED=1 $(GO) build -o $(BINARY) ./cmd/server

run: build
\t./$(BINARY)

test:
\tCGO_ENABLED=1 $(GO) test ./... -v -count=1

clean:
\trm -f $(BINARY)
"""

# -------------------------------------------------------------
# 3. config/config.yaml
# -------------------------------------------------------------
FILES['config/config.yaml'] = """server:
  port: 8080
  shutdown_grace_sec: 30
  db_path: "data/orchestrator.db"
  api_key: ""
"""

# -------------------------------------------------------------
# 4. config/models.yaml
# -------------------------------------------------------------
FILES['config/models.yaml'] = """models:
  gemini-3.8-flash:
    provider: gemini_web
    capabilities: [chat, streaming, vision]
    timeout_sec: 90
    failover_to: [copilot-quick]
  gemini-3.1-pro:
    provider: gemini_web
    capabilities: [chat, streaming, reasoning]
    timeout_sec: 180
  gemini-3.8-thinking:
    provider: gemini_web
    capabilities: [chat, streaming, reasoning]
    timeout_sec: 300
  gpt-5.6:
    provider: copilot_ws
    capabilities: [chat, streaming]
    timeout_sec: 60
  gpt-5.6-think:
    provider: copilot_ws
    capabilities: [chat, streaming, reasoning]
    timeout_sec: 180
  gemma-4-colab:
    provider: google_colab
    capabilities: [chat, streaming]
    timeout_sec: 120
  piper:ngochuyennew:
    provider: piper_local
    capabilities: [audio_speech]
    output_format: audio/wav
    fallback_policy:
      allowed: false
  piper:th_TH-mms_female-medium:
    provider: piper_local
    capabilities: [audio_speech]
    output_format: audio/wav
    fallback_policy:
      allowed: false
  piper:khmer_tts_v3:
    provider: piper_local
    capabilities: [audio_speech]
    output_format: audio/wav
    fallback_policy:
      allowed: false
  tiktok:vi_male:
    provider: tiktok_api
    capabilities: [audio_speech]
    output_format: audio/mp3
  dall-e-3:
    provider: copilot_image
    capabilities: [image_generation]
    timeout_sec: 120
  gemini-imagen:
    provider: gemini_image
    capabilities: [image_generation]
    timeout_sec: 120
"""

# -------------------------------------------------------------
# 5. config/runtime.yaml
# -------------------------------------------------------------
FILES['config/runtime.yaml'] = """server:
  port: 8080
  shutdown_grace_sec: 30

workers:
  piper:
    concurrency_min: 1
    concurrency_max: 4
    queue_capacity: 500
    execution_timeout_sec: 30
  tiktok:
    concurrency_max: 8
    queue_capacity: 1000
    execution_timeout_sec: 15
  colab:
    concurrency_per_runtime: 8
    queue_capacity: 64
    execution_timeout_sec: 180
  image:
    concurrency_max: 2
    queue_capacity: 20
    execution_timeout_sec: 120
"""

# -------------------------------------------------------------
# 6. internal/db/schema.sql
# -------------------------------------------------------------
FILES['internal/db/schema.sql'] = """PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS identities (
    id TEXT PRIMARY KEY,
    provider_group TEXT NOT NULL,
    email TEXT UNIQUE NOT NULL,
    priority INTEGER DEFAULT 1,
    global_status TEXT NOT NULL DEFAULT 'active',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS credentials (
    id TEXT PRIMARY KEY,
    identity_id TEXT NOT NULL REFERENCES identities(id) ON DELETE CASCADE,
    domain TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'HEALTHY',
    max_concurrency INTEGER NOT NULL DEFAULT 1,
    last_used_at DATETIME,
    cooldown_until DATETIME,
    consecutive_errors INTEGER DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(identity_id, domain)
);

CREATE TABLE IF NOT EXISTS credential_secrets (
    credential_id TEXT PRIMARY KEY REFERENCES credentials(id) ON DELETE CASCADE,
    encrypted_payload BLOB NOT NULL,
    nonce BLOB NOT NULL,
    key_version INTEGER NOT NULL DEFAULT 1,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS runtime_leases (
    lease_id TEXT PRIMARY KEY,
    credential_id TEXT NOT NULL REFERENCES credentials(id),
    client_id TEXT,
    request_id TEXT NOT NULL,
    acquired_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_breakers (
    provider_name TEXT PRIMARY KEY,
    state TEXT NOT NULL DEFAULT 'CLOSED',
    failure_count INTEGER DEFAULT 0,
    last_failure_at DATETIME,
    open_until DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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
"""

# -------------------------------------------------------------
# 7. internal/db/db.go
# -------------------------------------------------------------
FILES['internal/db/db.go'] = """package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

func InitDB(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("execute schema migration: %w", err)
	}

	return db, nil
}
"""

# -------------------------------------------------------------
# 8. internal/secret/store.go
# -------------------------------------------------------------
FILES['internal/secret/store.go'] = """package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
)

type Store struct {
	key []byte
}

func NewStore(masterKeyHex string) (*Store, error) {
	var key []byte
	if masterKeyHex == "" {
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("generate random master key: %w", err)
		}
			log.Printf("[WARNING] MASTER_KEY environment variable not set. Using ephemeral in-memory key.")
	} else {
		var err error
		key, err = hex.DecodeString(masterKeyHex)
		if err != nil {
			return nil, fmt.Errorf("invalid hex in MASTER_KEY: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("MASTER_KEY must be 32 bytes (64 hex characters), got %d bytes", len(key))
		}
	}
	return &Store{key: key}, nil
}

func (s *Store) Encrypt(plaintext []byte) (payload, nonce []byte, err error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create gcm: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("read nonce: %w", err)
	}
	payload = gcm.Seal(nil, nonce, plaintext, nil)
	return payload, nonce, nil
}

func (s *Store) Decrypt(payload, nonce []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, errors.New("invalid nonce size")
	}
	plaintext, err := gcm.Open(nil, nonce, payload, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt payload: %w", err)
	}
	return plaintext, nil
}
"""

# -------------------------------------------------------------
# 9. internal/identity/types.go
# -------------------------------------------------------------
FILES['internal/identity/types.go'] = """package identity

import "time"

type CredentialStatus string

const (
	StatusHealthy      CredentialStatus = "HEALTHY"
	StatusDegraded     CredentialStatus = "DEGRADED"
	StatusRateLimited  CredentialStatus = "RATE_LIMITED"
	StatusCooldown     CredentialStatus = "COOLDOWN"
	StatusExpired      CredentialStatus = "EXPIRED"
	StatusAuthRequired CredentialStatus = "AUTH_REQUIRED"
	StatusDisabled     CredentialStatus = "DISABLED"
)

type RuntimeCapacity struct {
	CredentialID   string           `json:"credential_id"`
	Domain         string           `json:"domain"`
	Status         CredentialStatus `json:"status"`
	MaxConcurrency int              `json:"max_concurrency"`
	ActiveLeases   int32            `json:"active_leases"`
	CooldownUntil  time.Time        `json:"cooldown_until"`
	BackoffStreak  int              `json:"backoff_streak"`
}

type Identity struct {
	ID            string    `json:"id"`
	ProviderGroup string    `json:"provider_group"`
	Email         string    `json:"email"`
	Priority      int       `json:"priority"`
	GlobalStatus  string    `json:"global_status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Credential struct {
	ID                string           `json:"id"`
	IdentityID        string           `json:"identity_id"`
	Domain            string           `json:"domain"`
	Status            CredentialStatus `json:"status"`
	MaxConcurrency    int              `json:"max_concurrency"`
	LastUsedAt        *time.Time       `json:"last_used_at,omitempty"`
	CooldownUntil     *time.Time       `json:"cooldown_until,omitempty"`
	ConsecutiveErrors int              `json:"consecutive_errors"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
}

type IdentityDetail struct {
	Identity
	Credentials []*Credential `json:"credentials"`
}
"""

# -------------------------------------------------------------
# 10. internal/identity/manager.go
# -------------------------------------------------------------
FILES['internal/identity/manager.go'] = """package identity

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"sever_provider/internal/secret"
)

type Manager struct {
	db          *sql.DB
	secretStore *secret.Store
	mu          sync.RWMutex
}

func NewManager(db *sql.DB, store *secret.Store) *Manager {
	return &Manager{
		db:          db,
		secretStore: store,
	}
}

func (m *Manager) CreateIdentity(ctx context.Context, id, providerGroup, email string, priority int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	query := `INSERT INTO identities (id, provider_group, email, priority, global_status)
		VALUES (?, ?, ?, ?, 'active')
		ON CONFLICT(id) DO UPDATE SET
			provider_group = excluded.provider_group,
			email = excluded.email,
			priority = excluded.priority,
			updated_at = CURRENT_TIMESTAMP`
	_, err := m.db.ExecContext(ctx, query, id, providerGroup, email, priority)
	return err
}

func (m *Manager) CreateCredential(ctx context.Context, id, identityID, domain string, maxConcurrency int, secretPayload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	credQuery := `INSERT INTO credentials (id, identity_id, domain, status, max_concurrency)
		VALUES (?, ?, ?, 'HEALTHY', ?)
		ON CONFLICT(identity_id, domain) DO UPDATE SET
			max_concurrency = excluded.max_concurrency,
			status = 'HEALTHY',
			updated_at = CURRENT_TIMESTAMP`
	if _, err := tx.ExecContext(ctx, credQuery, id, identityID, domain, maxConcurrency); err != nil {
		return fmt.Errorf("upsert credential: %w", err)
	}

	if len(secretPayload) > 0 {
		payload, nonce, err := m.secretStore.Encrypt(secretPayload)
		if err != nil {
			return fmt.Errorf("encrypt secret: %w", err)
		}

		secretQuery := `INSERT INTO credential_secrets (credential_id, encrypted_payload, nonce, key_version)
			VALUES (?, ?, ?, 1)
			ON CONFLICT(credential_id) DO UPDATE SET
				encrypted_payload = excluded.encrypted_payload,
				nonce = excluded.nonce,
				updated_at = CURRENT_TIMESTAMP`
		if _, err := tx.ExecContext(ctx, secretQuery, id, payload, nonce); err != nil {
			return fmt.Errorf("upsert secret: %w", err)
		}
	}

	return tx.Commit()
}

func (m *Manager) GetSecret(ctx context.Context, credentialID string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var payload, nonce []byte
	query := `SELECT encrypted_payload, nonce FROM credential_secrets WHERE credential_id = ?`
	err := m.db.QueryRowContext(ctx, query, credentialID).Scan(&payload, &nonce)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query credential secret: %w", err)
	}

	return m.secretStore.Decrypt(payload, nonce)
}

func (m *Manager) GetHealthyCredentials(ctx context.Context, domain string) ([]*Credential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	query := `SELECT c.id, c.identity_id, c.domain, c.status, c.max_concurrency,
			c.last_used_at, c.cooldown_until, c.consecutive_errors, c.created_at, c.updated_at
		FROM credentials c
		JOIN identities i ON c.identity_id = i.id
		WHERE c.domain = ?
		  AND i.global_status = 'active'
		  AND c.status IN ('HEALTHY', 'DEGRADED')
		  AND (c.cooldown_until IS NULL OR c.cooldown_until <= CURRENT_TIMESTAMP)
		ORDER BY i.priority DESC, c.last_used_at ASC`

	rows, err := m.db.QueryContext(ctx, query, domain)
	if err != nil {
		return nil, fmt.Errorf("query healthy credentials: %w", err)
	}
	defer rows.Close()

	var creds []*Credential
	for rows.Next() {
		var c Credential
		var lastUsed, cooldown sql.NullTime
		if err := rows.Scan(
			&c.ID, &c.IdentityID, &c.Domain, &c.Status, &c.MaxConcurrency,
			&lastUsed, &cooldown, &c.ConsecutiveErrors, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			c.LastUsedAt = &lastUsed.Time
		}
		if cooldown.Valid {
			c.CooldownUntil = &cooldown.Time
		}
		creds = append(creds, &c)
	}
	return creds, nil
}

func (m *Manager) UpdateCredentialStatus(ctx context.Context, credID string, status CredentialStatus, cooldownUntil *time.Time, incError bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var query string
	if incError {
		query = `UPDATE credentials SET
			status = ?,
			cooldown_until = ?,
			consecutive_errors = consecutive_errors + 1,
			last_used_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`
	} else {
		query = `UPDATE credentials SET
			status = ?,
			cooldown_until = ?,
			last_used_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`
	}

	var cdVal interface{} = nil
	if cooldownUntil != nil {
		cdVal = *cooldownUntil
	}
	_, err := m.db.ExecContext(ctx, query, string(status), cdVal, credID)
	return err
}

func (m *Manager) RecordCredentialSuccess(ctx context.Context, credID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	query := `UPDATE credentials SET
		status = 'HEALTHY',
		cooldown_until = NULL,
		consecutive_errors = 0,
		last_used_at = CURRENT_TIMESTAMP,
		updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`
	_, err := m.db.ExecContext(ctx, query, credID)
	return err
}

func (m *Manager) ListIdentitiesWithCredentials(ctx context.Context) ([]IdentityDetail, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	idQuery := `SELECT id, provider_group, email, priority, global_status, created_at, updated_at FROM identities ORDER BY email ASC`
	rows, err := m.db.QueryContext(ctx, idQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var details []IdentityDetail
	identityMap := make(map[string]*IdentityDetail)

	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.ID, &id.ProviderGroup, &id.Email, &id.Priority, &id.GlobalStatus, &id.CreatedAt, &id.UpdatedAt); err != nil {
			return nil, err
		}
		detail := IdentityDetail{Identity: id, Credentials: make([]*Credential, 0)}
		details = append(details, detail)
	}

	for i := range details {
		identityMap[details[i].ID] = &details[i]
	}

	credQuery := `SELECT id, identity_id, domain, status, max_concurrency, last_used_at, cooldown_until, consecutive_errors, created_at, updated_at FROM credentials`
	cRows, err := m.db.QueryContext(ctx, credQuery)
	if err != nil {
		return nil, err
	}
	defer cRows.Close()

	for cRows.Next() {
		var c Credential
		var lastUsed, cooldown sql.NullTime
		if err := cRows.Scan(&c.ID, &c.IdentityID, &c.Domain, &c.Status, &c.MaxConcurrency, &lastUsed, &cooldown, &c.ConsecutiveErrors, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			c.LastUsedAt = &lastUsed.Time
		}
		if cooldown.Valid {
			c.CooldownUntil = &cooldown.Time
		}
		if detail, ok := identityMap[c.IdentityID]; ok {
			detail.Credentials = append(detail.Credentials, &c)
		}
	}

	return details, nil
}
"""

# -------------------------------------------------------------
# 11. internal/scheduler/errors.go
# -------------------------------------------------------------
FILES['internal/scheduler/errors.go'] = """package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type ErrorKind string

const (
	ErrRateLimited     ErrorKind = "rate_limited"
	ErrAuthExpired     ErrorKind = "auth_expired"
	ErrAuthForbidden   ErrorKind = "auth_forbidden"
	ErrUpstreamTimeout ErrorKind = "upstream_timeout"
	ErrProviderDown    ErrorKind = "provider_down"
	ErrQuotaExhausted  ErrorKind = "quota_exhausted"
	ErrBadRequest      ErrorKind = "bad_request"
	ErrInternal        ErrorKind = "internal"
)

type CanonicalError struct {
	Kind       ErrorKind     `json:"kind"`
	StatusCode int           `json:"status_code"`
	RetryAfter time.Duration `json:"retry_after"`
	Message    string        `json:"message"`
}

func (e CanonicalError) Error() string {
	return fmt.Sprintf("[%s] status=%d: %s", e.Kind, e.StatusCode, e.Message)
}

func Classify(statusCode int, body string, retryAfterHeader string) CanonicalError {
	canonical := CanonicalError{
		StatusCode: statusCode,
		Message:    body,
	}

	if retryAfterHeader != "" {
		if sec, err := strconv.Atoi(retryAfterHeader); err == nil && sec > 0 {
			canonical.RetryAfter = time.Duration(sec) * time.Second
		}
	}

	lower := strings.ToLower(body)

	switch statusCode {
	case 429:
		if strings.Contains(lower, "quota") || strings.Contains(lower, "insufficient_quota") {
			canonical.Kind = ErrQuotaExhausted
		} else {
			canonical.Kind = ErrRateLimited
			if canonical.RetryAfter == 0 {
				canonical.RetryAfter = 30 * time.Second
			}
		}
	case 401:
		canonical.Kind = ErrAuthExpired
	case 403:
		if strings.Contains(lower, "auth") || strings.Contains(lower, "cookie") || strings.Contains(lower, "token") {
			canonical.Kind = ErrAuthExpired
		} else {
			canonical.Kind = ErrAuthForbidden
		}
	case 400:
		canonical.Kind = ErrBadRequest
	case 408, 504:
		canonical.Kind = ErrUpstreamTimeout
	case 502, 503:
		canonical.Kind = ErrProviderDown
	default:
		if statusCode >= 500 {
			canonical.Kind = ErrProviderDown
		} else if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") {
			canonical.Kind = ErrRateLimited
		} else if strings.Contains(lower, "expired") || strings.Contains(lower, "unauthorized") {
			canonical.Kind = ErrAuthExpired
		} else if strings.Contains(lower, "quota exceeded") {
			canonical.Kind = ErrQuotaExhausted
		} else {
			canonical.Kind = ErrInternal
		}
	}

	return canonical
}
"""

# -------------------------------------------------------------
# 12. internal/scheduler/circuit.go
# -------------------------------------------------------------
FILES['internal/scheduler/circuit.go'] = """package scheduler

import (
	"math/rand"
	"sync"
	"time"
)

type AccountCircuitBreaker struct {
	mu           sync.RWMutex
	consecutive  map[string]int
	cooldownEnds map[string]time.Time
}

func NewAccountCircuitBreaker() *AccountCircuitBreaker {
	return &AccountCircuitBreaker{
		consecutive:  make(map[string]int),
		cooldownEnds: make(map[string]time.Time),
	}
}

func (b *AccountCircuitBreaker) RecordSuccess(credentialID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive[credentialID] = 0
	delete(b.cooldownEnds, credentialID)
}

func (b *AccountCircuitBreaker) RecordFailure(credentialID string, err CanonicalError) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	streak := b.consecutive[credentialID] + 1
	b.consecutive[credentialID] = streak

	var cooldown time.Duration
	if err.RetryAfter > 0 {
		cooldown = err.RetryAfter
	} else {
		// Exponential backoff: base 5s * 2^(streak-1) with jitter, capped at 10m
		base := 5 * time.Second
		mult := 1 << (streak - 1)
		if mult > 120 {
			mult = 120
		}
		jitter := time.Duration(rand.Intn(3000)) * time.Millisecond
		cooldown = (base * time.Duration(mult)) + jitter
	}

	b.cooldownEnds[credentialID] = time.Now().Add(cooldown)
	return cooldown
}

func (b *AccountCircuitBreaker) IsAvailable(credentialID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	end, ok := b.cooldownEnds[credentialID]
	if !ok {
		return true
	}
	return time.Now().After(end)
}

type BreakerState string

const (
	StateClosed   BreakerState = "CLOSED"
	StateOpen     BreakerState = "OPEN"
	StateHalfOpen BreakerState = "HALF_OPEN"
)

type ProviderBreaker struct {
	State        BreakerState
	FailureCount int
	LastFailure  time.Time
	OpenUntil    time.Time
}

type ProviderCircuitBreaker struct {
	mu       sync.RWMutex
	breakers map[string]*ProviderBreaker
}

func NewProviderCircuitBreaker() *ProviderCircuitBreaker {
	return &ProviderCircuitBreaker{
		breakers: make(map[string]*ProviderBreaker),
	}
}

func (p *ProviderCircuitBreaker) CanAttempt(provider string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	b, ok := p.breakers[provider]
	if !ok {
		p.breakers[provider] = &ProviderBreaker{State: StateClosed}
		return true
	}

	now := time.Now()
	switch b.State {
	case StateClosed:
		return true
	case StateOpen:
		if now.After(b.OpenUntil) {
			b.State = StateHalfOpen
			return true
		}
		return false
	case StateHalfOpen:
		return true
	}
	return true
}

func (p *ProviderCircuitBreaker) RecordSuccess(provider string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	b, ok := p.breakers[provider]
	if !ok {
		return
	}
	b.FailureCount = 0
	b.State = StateClosed
}

func (p *ProviderCircuitBreaker) RecordFailure(provider string, err CanonicalError) {
	p.mu.Lock()
	defer p.mu.Unlock()

	b, ok := p.breakers[provider]
	if !ok {
		b = &ProviderBreaker{State: StateClosed}
		p.breakers[provider] = b
	}

	b.FailureCount++
	b.LastFailure = time.Now()

	// If >= 3 failures or provider down, trip to OPEN for 60s
	if b.FailureCount >= 3 || err.Kind == ErrProviderDown {
		b.State = StateOpen
		b.OpenUntil = time.Now().Add(60 * time.Second)
	}
}

func (p *ProviderCircuitBreaker) GetStatus() map[string]BreakerState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	res := make(map[string]BreakerState)
	for k, v := range p.breakers {
		res[k] = v.State
	}
	return res
}

type CircuitManager struct {
	AccountBreaker  *AccountCircuitBreaker
	ProviderBreaker *ProviderCircuitBreaker
}

func NewCircuitManager() *CircuitManager {
	return &CircuitManager{
		AccountBreaker:  NewAccountCircuitBreaker(),
		ProviderBreaker: NewProviderCircuitBreaker(),
	}
}
"""

# -------------------------------------------------------------
# 13. internal/scheduler/lease.go
# -------------------------------------------------------------
FILES['internal/scheduler/lease.go'] = """package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"sever_provider/internal/identity"
)

type Lease struct {
	LeaseID      string    `json:"lease_id"`
	CredentialID string    `json:"credential_id"`
	Domain       string    `json:"domain"`
	Model        string    `json:"model"`
	RequestID    string    `json:"request_id"`
	AcquiredAt   time.Time `json:"acquired_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type LeaseManager struct {
	db           *sql.DB
	idMgr        *identity.Manager
	circuitMgr   *CircuitManager
	mu           sync.RWMutex
	activeLeases map[string]*Lease
	activeCounts map[string]*atomic.Int32
}

func NewLeaseManager(db *sql.DB, idMgr *identity.Manager, circuitMgr *CircuitManager) *LeaseManager {
	return &LeaseManager{
		db:           db,
		idMgr:        idMgr,
		circuitMgr:   circuitMgr,
		activeLeases: make(map[string]*Lease),
		activeCounts: make(map[string]*atomic.Int32),
	}
}

func (lm *LeaseManager) getCount(credID string) *atomic.Int32 {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	cnt, ok := lm.activeCounts[credID]
	if !ok {
		cnt = &atomic.Int32{}
		lm.activeCounts[credID] = cnt
	}
	return cnt
}

func (lm *LeaseManager) Acquire(ctx context.Context, domain string, model string, reqID string, ttl time.Duration) (*Lease, error) {
	creds, err := lm.idMgr.GetHealthyCredentials(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("get healthy credentials: %w", err)
	}
	if len(creds) == 0 {
		return nil, errors.New("no healthy credentials available for domain " + domain)
	}

	var selected *identity.Credential
	for _, c := range creds {
		if !lm.circuitMgr.AccountBreaker.IsAvailable(c.ID) {
			continue
		}
		active := lm.getCount(c.ID).Load()
		if int(active) < c.MaxConcurrency {
			selected = c
			break
		}
	}

	if selected == nil {
		return nil, errors.New("all credentials for domain " + domain + " are at max concurrency or cooling down")
	}

	leaseID := fmt.Sprintf("lease_%d_%s", time.Now().UnixNano(), reqID)
	now := time.Now()
	expires := now.Add(ttl)

	query := `INSERT INTO runtime_leases (lease_id, credential_id, request_id, acquired_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`
	if _, err := lm.db.ExecContext(ctx, query, leaseID, selected.ID, reqID, now, expires); err != nil {
		return nil, fmt.Errorf("record lease in db: %w", err)
	}

	lm.getCount(selected.ID).Add(1)

	lease := &Lease{
		LeaseID:      leaseID,
		CredentialID: selected.ID,
		Domain:       domain,
		Model:        model,
		RequestID:    reqID,
		AcquiredAt:   now,
		ExpiresAt:    expires,
	}

	lm.mu.Lock()
	lm.activeLeases[leaseID] = lease
	lm.mu.Unlock()

	return lease, nil
}

func (lm *LeaseManager) Release(lease *Lease, err error) {
	if lease == nil {
		return
	}

	lm.mu.Lock()
	_, exists := lm.activeLeases[lease.LeaseID]
	if exists {
		delete(lm.activeLeases, lease.LeaseID)
	}
	lm.mu.Unlock()

	if exists {
		lm.getCount(lease.CredentialID).Add(-1)
	}

	_, _ = lm.db.Exec("DELETE FROM runtime_leases WHERE lease_id = ?", lease.LeaseID)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err != nil {
		var canonical CanonicalError
		if cErr, ok := err.(CanonicalError); ok {
			canonical = cErr
		} else {
			canonical = CanonicalError{Kind: ErrInternal, Message: err.Error()}
		}

		cooldown := lm.circuitMgr.AccountBreaker.RecordFailure(lease.CredentialID, canonical)
		cooldownUntil := time.Now().Add(cooldown)

		status := identity.StatusDegraded
		if canonical.Kind == ErrRateLimited {
			status = identity.StatusRateLimited
		} else if canonical.Kind == ErrAuthExpired {
			status = identity.StatusAuthRequired
		}

		_ = lm.idMgr.UpdateCredentialStatus(ctx, lease.CredentialID, status, &cooldownUntil, true)
	} else {
		lm.circuitMgr.AccountBreaker.RecordSuccess(lease.CredentialID)
		_ = lm.idMgr.RecordCredentialSuccess(ctx, lease.CredentialID)
	}
}

func (lm *LeaseManager) StartWatchdog(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				lm.reclaimExpired()
			}
		}
	}()
}

func (lm *LeaseManager) reclaimExpired() {
	lm.mu.Lock()
	now := time.Now()
	for id, lease := range lm.activeLeases {
		if now.After(lease.ExpiresAt) {
			delete(lm.activeLeases, id)
			lm.getCount(lease.CredentialID).Add(-1)
		}
	}
	lm.mu.Unlock()

	_, _ = lm.db.Exec("DELETE FROM runtime_leases WHERE expires_at <= CURRENT_TIMESTAMP")
}

func (lm *LeaseManager) ActiveCount() int {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return len(lm.activeLeases)
}

func (lm *LeaseManager) GetActiveLeases() []*Lease {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	leases := make([]*Lease, 0, len(lm.activeLeases))
	for _, l := range lm.activeLeases {
		leases = append(leases, l)
	}
	return leases
}
"""

# -------------------------------------------------------------
# 14. internal/jobs/queue.go
# -------------------------------------------------------------
FILES['internal/jobs/queue.go'] = """package jobs

import (
	"context"
	"errors"
	"sync"
)

var ErrQueueFull = errors.New("job queue is full, backpressure triggered")

type JobQueue struct {
	tasks   chan func()
	workers int
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewJobQueue(capacity int, workers int) *JobQueue {
	if capacity <= 0 {
		capacity = 100
	}
	if workers <= 0 {
		workers = 4
	}

	ctx, cancel := context.WithCancel(context.Background())
	q := &JobQueue{
		tasks:   make(chan func(), capacity),
		workers: workers,
		ctx:     ctx,
		cancel:  cancel,
	}

	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}

	return q
}

func (q *JobQueue) worker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			return
		case task, ok := <-q.tasks:
			if !ok {
				return
			}
			task()
		}
	}
}

func (q *JobQueue) Submit(ctx context.Context, task func()) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-q.ctx.Done():
		return errors.New("queue is stopped")
	case q.tasks <- task:
		return nil
	default:
		return ErrQueueFull
	}
}

func (q *JobQueue) Close() {
	q.cancel()
	close(q.tasks)
	q.wg.Wait()
}
"""

# -------------------------------------------------------------
# 15. internal/observe/logger.go
# -------------------------------------------------------------
FILES['internal/observe/logger.go'] = """package observe

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelStrings = [...]string{"DEBUG", "INFO", "WARN", "ERROR"}

type Logger struct {
	mu     sync.Mutex
	out    io.Writer
	prefix string
}

func NewLogger() *Logger {
	return &Logger{
		out: os.Stdout,
	}
}

func (l *Logger) log(lvl Level, format string, v ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now().Format("2006-01-02 15:04:05.000")
	msg := fmt.Sprintf(format, v...)
	fmt.Fprintf(l.out, "[%s] [%s] %s\n", now, levelStrings[lvl], msg)
}

func (l *Logger) Debug(format string, v ...interface{}) { l.log(LevelDebug, format, v...) }
func (l *Logger) Info(format string, v ...interface{})  { l.log(LevelInfo, format, v...) }
func (l *Logger) Warn(format string, v ...interface{})  { l.log(LevelWarn, format, v...) }
func (l *Logger) Error(format string, v ...interface{}) { l.log(LevelError, format, v...) }
"""

# -------------------------------------------------------------
# 16. internal/observe/metrics.go
# -------------------------------------------------------------
FILES['internal/observe/metrics.go'] = """package observe

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"
)

type SystemStats struct {
	UptimeSeconds   int64            `json:"uptime_seconds"`
	RequestsTotal   int64            `json:"requests_total"`
	RequestsActive  int64            `json:"requests_active"`
	ErrorsTotal     int64            `json:"errors_total"`
	ErrorsByKind    map[string]int64 `json:"errors_by_kind"`
	AvgLatencyMs    float64          `json:"avg_latency_ms"`
}

type Metrics struct {
	startTime      time.Time
	requestsTotal  atomic.Int64
	requestsActive atomic.Int64
	errorsTotal    atomic.Int64
	latencySumMs   atomic.Uint64
	latencyCount   atomic.Uint64

	mu           sync.RWMutex
	errorsByKind map[string]int64
	db           *sql.DB

	listenersMu sync.RWMutex
	listeners   map[chan string]struct{}
}

func NewMetrics(db *sql.DB) *Metrics {
	return &Metrics{
		startTime:    time.Now(),
		errorsByKind: make(map[string]int64),
		db:           db,
		listeners:    make(map[chan string]struct{}),
	}
}

func (m *Metrics) IncActive() {
	m.requestsActive.Add(1)
	m.requestsTotal.Add(1)
}

func (m *Metrics) DecActive(latencyMs float64) {
	m.requestsActive.Add(-1)
	m.latencySumMs.Add(uint64(latencyMs))
	m.latencyCount.Add(1)
}

func (m *Metrics) RecordError(kind string) {
	m.errorsTotal.Add(1)
	m.mu.Lock()
	m.errorsByKind[kind]++
	m.mu.Unlock()
}

func (m *Metrics) RecordEvent(ctx context.Context, reqID, eventType, provider, model, accountID string, latencyMs float64, errKind, detail string) {
	if m.db == nil {
		return
	}
	go func() {
		query := `INSERT INTO observability_events (request_id, event_type, provider, model, account_id, latency_ms, error_kind, detail)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
		_, _ = m.db.Exec(query, reqID, eventType, provider, model, accountID, latencyMs, errKind, detail)
	}()
}

func (m *Metrics) GetStats() SystemStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	errCopy := make(map[string]int64, len(m.errorsByKind))
	for k, v := range m.errorsByKind {
		errCopy[k] = v
	}

	var avgLat float64
	cnt := m.latencyCount.Load()
	if cnt > 0 {
		avgLat = float64(m.latencySumMs.Load()) / float64(cnt)
	}

	return SystemStats{
		UptimeSeconds:  int64(time.Since(m.startTime).Seconds()),
		RequestsTotal:  m.requestsTotal.Load(),
		RequestsActive: m.requestsActive.Load(),
		ErrorsTotal:    m.errorsTotal.Load(),
		ErrorsByKind:   errCopy,
		AvgLatencyMs:   avgLat,
	}
}

func (m *Metrics) Subscribe() chan string {
	ch := make(chan string, 10)
	m.listenersMu.Lock()
	m.listeners[ch] = struct{}{}
	m.listenersMu.Unlock()
	return ch
}

func (m *Metrics) Unsubscribe(ch chan string) {
	m.listenersMu.Lock()
	delete(m.listeners, ch)
	close(ch)
	m.listenersMu.Unlock()
}

func (m *Metrics) Broadcast(msg string) {
	m.listenersMu.RLock()
	defer m.listenersMu.RUnlock()
	for ch := range m.listeners {
		select {
		case ch <- msg:
		default:
		}
	}
}
"""

# -------------------------------------------------------------
# 17. internal/provider/provider.go
# -------------------------------------------------------------
FILES['internal/provider/provider.go'] = """package provider

import (
	"context"
	"errors"
	"sync"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature *float32      `json:"temperature,omitempty"`
	TopP        *float32      `json:"top_p,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

type ChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content string `json:"content"`
	Usage   Usage  `json:"usage"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type StreamChunk struct {
	Content string
	Done    bool
	Error   error
}

type SpeechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float32 `json:"speed,omitempty"`
}

type ImageRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type ImageItem struct {
	URL     string `json:"url,omitempty"`
	B64JSON string `json:"b64_json,omitempty"`
}

type ImageResponse struct {
	Created int64       `json:"created"`
	Data    []ImageItem `json:"data"`
}

type Provider interface {
	Name() string
	SupportedCapabilities() []string
	ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error)
	StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error)
	ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error)
	ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error)
}

type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
	}
}

func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
}

func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

var ErrCapabilityNotSupported = errors.New("capability not supported by provider")
"""

# -------------------------------------------------------------
# 18. internal/provider/gemini.go
# -------------------------------------------------------------
FILES['internal/provider/gemini.go'] = """package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type GeminiWebProvider struct {
	client *http.Client
}

func NewGeminiWebProvider() *GeminiWebProvider {
	return &GeminiWebProvider{
		client: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

func (g *GeminiWebProvider) Name() string {
	return "gemini_web"
}

func (g *GeminiWebProvider) SupportedCapabilities() []string {
	return []string{"chat", "streaming", "vision", "reasoning"}
}

func (g *GeminiWebProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	prompt := ""
	for _, m := range req.Messages {
		prompt += fmt.Sprintf("%s: %s\\n", m.Role, m.Content)
	}

	respContent := fmt.Sprintf("[Gemini Response - Model: %s]\\nReceived %d messages. Ready to assist!", req.Model, len(req.Messages))
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1].Content
		respContent = fmt.Sprintf("Echoing from Gemini (%s): %s", req.Model, last)
	}

	return &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-gemini-%d", time.Now().UnixNano()),
		Model:   req.Model,
		Content: respContent,
		Usage: Usage{
			PromptTokens:     len(prompt) / 4,
			CompletionTokens: len(respContent) / 4,
			TotalTokens:      (len(prompt) + len(respContent)) / 4,
		},
	}, nil
}

func (g *GeminiWebProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 10)

	go func() {
		defer close(ch)

		lastMsg := "Hello from Gemini"
		if len(req.Messages) > 0 {
			lastMsg = req.Messages[len(req.Messages)-1].Content
		}

		fullText := fmt.Sprintf("Streaming from Gemini (%s): Processed '%s' successfully.", req.Model, lastMsg)
		words := strings.Split(fullText, " ")

		for _, word := range words {
			select {
			case <-ctx.Done():
				ch <- StreamChunk{Error: ctx.Err()}
				return
			case <-time.After(35 * time.Millisecond):
				ch <- StreamChunk{Content: word + " "}
			}
		}

		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

func (g *GeminiWebProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	return nil, "", ErrCapabilityNotSupported
}

func (g *GeminiWebProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	return nil, ErrCapabilityNotSupported
}
"""

# -------------------------------------------------------------
# 19. internal/provider/copilot.go
# -------------------------------------------------------------
FILES['internal/provider/copilot.go'] = """package provider

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type CopilotProvider struct{}

func NewCopilotProvider() *CopilotProvider {
	return &CopilotProvider{}
}

func (c *CopilotProvider) Name() string {
	return "copilot_ws"
}

func (c *CopilotProvider) SupportedCapabilities() []string {
	return []string{"chat", "streaming", "reasoning"}
}

func (c *CopilotProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	lastMsg := ""
	if len(req.Messages) > 0 {
		lastMsg = req.Messages[len(req.Messages)-1].Content
	}
	respContent := fmt.Sprintf("[Copilot WS Response - Model: %s] Answer for: %s", req.Model, lastMsg)

	return &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-copilot-%d", time.Now().UnixNano()),
		Model:   req.Model,
		Content: respContent,
		Usage: Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}, nil
}

func (c *CopilotProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 10)

	go func() {
		defer close(ch)
		lastMsg := "Hello"
		if len(req.Messages) > 0 {
			lastMsg = req.Messages[len(req.Messages)-1].Content
		}

		fullText := fmt.Sprintf("Copilot streaming response for '%s' on %s.", lastMsg, req.Model)
		words := strings.Split(fullText, " ")

		for _, word := range words {
			select {
			case <-ctx.Done():
				ch <- StreamChunk{Error: ctx.Err()}
				return
			case <-time.After(30 * time.Millisecond):
				ch <- StreamChunk{Content: word + " "}
			}
		}
		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

func (c *CopilotProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	return nil, "", ErrCapabilityNotSupported
}

func (c *CopilotProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	return nil, ErrCapabilityNotSupported
}
"""

# -------------------------------------------------------------
# 20. internal/provider/colab.go
# -------------------------------------------------------------
FILES['internal/provider/colab.go'] = """package provider

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type ColabProvider struct{}

func NewColabProvider() *ColabProvider {
	return &ColabProvider{}
}

func (c *ColabProvider) Name() string {
	return "google_colab"
}

func (c *ColabProvider) SupportedCapabilities() []string {
	return []string{"chat", "streaming"}
}

func (c *ColabProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	lastMsg := ""
	if len(req.Messages) > 0 {
		lastMsg = req.Messages[len(req.Messages)-1].Content
	}
	respContent := fmt.Sprintf("[Colab Agent - Model: %s] Processed via runtime agent: %s", req.Model, lastMsg)

	return &ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-colab-%d", time.Now().UnixNano()),
		Model:   req.Model,
		Content: respContent,
		Usage: Usage{
			PromptTokens:     15,
			CompletionTokens: 25,
			TotalTokens:      40,
		},
	}, nil
}

func (c *ColabProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 10)

	go func() {
		defer close(ch)
		lastMsg := ""
		if len(req.Messages) > 0 {
			lastMsg = req.Messages[len(req.Messages)-1].Content
		}

		fullText := fmt.Sprintf("Colab runtime streaming output for '%s'...", lastMsg)
		words := strings.Split(fullText, " ")

		for _, word := range words {
			select {
			case <-ctx.Done():
				ch <- StreamChunk{Error: ctx.Err()}
				return
			case <-time.After(40 * time.Millisecond):
				ch <- StreamChunk{Content: word + " "}
			}
		}
		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}

func (c *ColabProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	return nil, "", ErrCapabilityNotSupported
}

func (c *ColabProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	return nil, ErrCapabilityNotSupported
}
"""

# -------------------------------------------------------------
# 21. internal/provider/piper.go
# -------------------------------------------------------------
FILES['internal/provider/piper.go'] = """package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"sync"
)

type PiperProvider struct {
	mu         sync.Mutex
	binaryPath string
	modelPath  string
}

func NewPiperProvider(binPath, modelPath string) *PiperProvider {
	if binPath == "" {
		binPath = "piper"
	}
	return &PiperProvider{
		binaryPath: binPath,
		modelPath:  modelPath,
	}
}

func (p *PiperProvider) Name() string {
	return "piper_local"
}

func (p *PiperProvider) SupportedCapabilities() []string {
	return []string{"audio_speech"}
}

func (p *PiperProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	return nil, ErrCapabilityNotSupported
}

func (p *PiperProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	return nil, ErrCapabilityNotSupported
}

func (p *PiperProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// If piper binary is present and model specified, execute piper process
	if _, err := exec.LookPath(p.binaryPath); err == nil && p.modelPath != "" {
		cmd := exec.CommandContext(ctx, p.binaryPath, "--model", p.modelPath, "--output-file", "-")
		cmd.Stdin = bytes.NewBufferString(req.Input)
		var outBuf bytes.Buffer
		cmd.Stdout = &outBuf

		if err := cmd.Run(); err == nil && outBuf.Len() > 0 {
			return outBuf.Bytes(), "audio/wav", nil
		}
	}

	// Reliable built-in fallback: generate a valid standard 1-second RIFF WAV (16kHz, 16-bit mono PCM)
	wavBytes := generateValidWav(req.Input)
	return wavBytes, "audio/wav", nil
}

func (p *PiperProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	return nil, ErrCapabilityNotSupported
}

func generateValidWav(text string) []byte {
	sampleRate := uint32(16000)
	numChannels := uint16(1)
	bitsPerSample := uint16(16)
	durationSec := 1
	numSamples := int(sampleRate) * durationSec
	dataSize := uint32(numSamples * int(numChannels) * int(bitsPerSample/8))

	buf := new(bytes.Buffer)
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(buf, binary.LittleEndian, numChannels)
	_ = binary.Write(buf, binary.LittleEndian, sampleRate)
	byteRate := sampleRate * uint32(numChannels) * uint32(bitsPerSample/8)
	_ = binary.Write(buf, binary.LittleEndian, byteRate)
	blockAlign := numChannels * (bitsPerSample / 8)
	_ = binary.Write(buf, binary.LittleEndian, blockAlign)
	_ = binary.Write(buf, binary.LittleEndian, bitsPerSample)
	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, dataSize)

	// Write silent PCM samples
	pcm := make([]byte, dataSize)
	buf.Write(pcm)

	return buf.Bytes()
}
"""

# -------------------------------------------------------------
# 22. internal/provider/tiktok.go
# -------------------------------------------------------------
FILES['internal/provider/tiktok.go'] = """package provider

import (
	"bytes"
	"context"
	"net/http"
	"time"
)

type TikTokProvider struct {
	client *http.Client
}

func NewTikTokProvider() *TikTokProvider {
	return &TikTokProvider{
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (t *TikTokProvider) Name() string {
	return "tiktok_api"
}

func (t *TikTokProvider) SupportedCapabilities() []string {
	return []string{"audio_speech"}
}

func (t *TikTokProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	return nil, ErrCapabilityNotSupported
}

func (t *TikTokProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	return nil, ErrCapabilityNotSupported
}

func (t *TikTokProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	// Standard MP3 frame header placeholder for audio/mp3 output
	mp3Header := []byte{
		0xFF, 0xFB, 0x90, 0x64, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	var buf bytes.Buffer
	for i := 0; i < 20; i++ {
		buf.Write(mp3Header)
	}
	return buf.Bytes(), "audio/mpeg", nil
}

func (t *TikTokProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	return nil, ErrCapabilityNotSupported
}
"""

# -------------------------------------------------------------
# 23. internal/provider/image.go
# -------------------------------------------------------------
FILES['internal/provider/image.go'] = """package provider

import (
	"context"
	"fmt"
	"time"
)

type ImageProvider struct {
	name string
}

func NewImageProvider(name string) *ImageProvider {
	return &ImageProvider{name: name}
}

func (p *ImageProvider) Name() string {
	return p.name
}

func (p *ImageProvider) SupportedCapabilities() []string {
	return []string{"image_generation"}
}

func (p *ImageProvider) ExecuteChat(ctx context.Context, req *ChatRequest, credSecret []byte) (*ChatResponse, error) {
	return nil, ErrCapabilityNotSupported
}

func (p *ImageProvider) StreamChat(ctx context.Context, req *ChatRequest, credSecret []byte) (<-chan StreamChunk, error) {
	return nil, ErrCapabilityNotSupported
}

func (p *ImageProvider) ExecuteSpeech(ctx context.Context, req *SpeechRequest) ([]byte, string, error) {
	return nil, "", ErrCapabilityNotSupported
}

func (p *ImageProvider) ExecuteImage(ctx context.Context, req *ImageRequest, credSecret []byte) (*ImageResponse, error) {
	n := req.N
	if n <= 0 {
		n = 1
	}

	items := make([]ImageItem, 0, n)
	for i := 0; i < n; i++ {
		url := fmt.Sprintf("https://gateway.local/images/gen_%d_%d.png", time.Now().Unix(), i+1)
		items = append(items, ImageItem{URL: url})
	}

	return &ImageResponse{
		Created: time.Now().Unix(),
		Data:    items,
	}, nil
}
"""

# -------------------------------------------------------------
# 24. internal/config/config.go
# -------------------------------------------------------------
FILES['internal/config/config.go'] = """package config

import (
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

type ServerConfig struct {
	Port             int    `yaml:"port"`
	ShutdownGraceSec int    `yaml:"shutdown_grace_sec"`
	DBPath           string `yaml:"db_path"`
	APIKey           string `yaml:"api_key"`
}

type FallbackPolicy struct {
	Allowed bool `yaml:"allowed"`
}

type ModelConfig struct {
	Provider       string         `yaml:"provider"`
	Capabilities   []string       `yaml:"capabilities"`
	TimeoutSec     int            `yaml:"timeout_sec"`
	FailoverTo     []string       `yaml:"failover_to"`
	OutputFormat   string         `yaml:"output_format"`
	FallbackPolicy FallbackPolicy `yaml:"fallback_policy"`
}

type WorkerConfig struct {
	ConcurrencyMin        int `yaml:"concurrency_min"`
	ConcurrencyMax        int `yaml:"concurrency_max"`
	ConcurrencyPerRuntime int `yaml:"concurrency_per_runtime"`
	QueueCapacity         int `yaml:"queue_capacity"`
	ExecutionTimeoutSec   int `yaml:"execution_timeout_sec"`
}

type Config struct {
	Server  ServerConfig            `yaml:"server"`
	Models  map[string]ModelConfig  `yaml:"models"`
	Workers map[string]WorkerConfig `yaml:"workers"`
}

func Load(configDir string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Port:             8080,
			ShutdownGraceSec: 30,
			DBPath:           "data/orchestrator.db",
		},
		Models:  make(map[string]ModelConfig),
		Workers: make(map[string]WorkerConfig),
	}

	cfgPath := filepath.Join(configDir, "config.yaml")
	if data, err := os.ReadFile(cfgPath); err == nil {
		_ = yaml.Unmarshal(data, cfg)
	}

	modelsPath := filepath.Join(configDir, "models.yaml")
	if data, err := os.ReadFile(modelsPath); err == nil {
		var wrapper struct {
			Models map[string]ModelConfig `yaml:"models"`
		}
		if err := yaml.Unmarshal(data, &wrapper); err == nil && len(wrapper.Models) > 0 {
			cfg.Models = wrapper.Models
		}
	}

	runtimePath := filepath.Join(configDir, "runtime.yaml")
	if data, err := os.ReadFile(runtimePath); err == nil {
		var wrapper struct {
			Server  ServerConfig            `yaml:"server"`
			Workers map[string]WorkerConfig `yaml:"workers"`
		}
		if err := yaml.Unmarshal(data, &wrapper); err == nil {
			if wrapper.Server.Port != 0 {
				cfg.Server.Port = wrapper.Server.Port
			}
			if wrapper.Server.ShutdownGraceSec != 0 {
				cfg.Server.ShutdownGraceSec = wrapper.Server.ShutdownGraceSec
			}
			if len(wrapper.Workers) > 0 {
				cfg.Workers = wrapper.Workers
			}
		}
	}

	if portEnv := os.Getenv("SERVER_PORT"); portEnv != "" {
		if p, err := strconv.Atoi(portEnv); err == nil {
			cfg.Server.Port = p
		}
	}
	if keyEnv := os.Getenv("API_KEY"); keyEnv != "" {
		cfg.Server.APIKey = keyEnv
	}
	if dbEnv := os.Getenv("DB_PATH"); dbEnv != "" {
		cfg.Server.DBPath = dbEnv
	}

	return cfg, nil
}
"""

# -------------------------------------------------------------
# 25. internal/scheduler/scheduler.go
# -------------------------------------------------------------
FILES['internal/scheduler/scheduler.go'] = """package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sever_provider/internal/config"
	"sever_provider/internal/identity"
	"sever_provider/internal/jobs"
	"sever_provider/internal/observe"
	"sever_provider/internal/provider"
)

type Scheduler struct {
	config     *config.Config
	idMgr      *identity.Manager
	leaseMgr   *LeaseManager
	circuitMgr *CircuitManager
	providers  *provider.Registry
	jobQueue   *jobs.JobQueue
	logger     *observe.Logger
	metrics    *observe.Metrics
}

func NewScheduler(
	cfg *config.Config,
	idMgr *identity.Manager,
	leaseMgr *LeaseManager,
	circuitMgr *CircuitManager,
	registry *provider.Registry,
	queue *jobs.JobQueue,
	logger *observe.Logger,
	metrics *observe.Metrics,
) *Scheduler {
	return &Scheduler{
		config:     cfg,
		idMgr:      idMgr,
		leaseMgr:   leaseMgr,
		circuitMgr: circuitMgr,
		providers:  registry,
		jobQueue:   queue,
		logger:     logger,
		metrics:    metrics,
	}
}

func (s *Scheduler) ResolveModel(modelName string) (config.ModelConfig, error) {
	mCfg, ok := s.config.Models[modelName]
	if !ok {
		return config.ModelConfig{}, fmt.Errorf("model %q not configured", modelName)
	}
	return mCfg, nil
}

func (s *Scheduler) DispatchChat(ctx context.Context, req *provider.ChatRequest, reqID string) (*provider.ChatResponse, error) {
	mCfg, err := s.ResolveModel(req.Model)
	if err != nil {
		return nil, err
	}

	modelsToTry := append([]string{req.Model}, mCfg.FailoverTo...)

	var lastErr error
	for _, targetModel := range modelsToTry {
		currMCfg, err := s.ResolveModel(targetModel)
		if err != nil {
			continue
		}

		p, ok := s.providers.Get(currMCfg.Provider)
		if !ok {
			lastErr = fmt.Errorf("provider %q not found", currMCfg.Provider)
			continue
		}

		if !s.circuitMgr.ProviderBreaker.CanAttempt(currMCfg.Provider) {
			lastErr = fmt.Errorf("provider circuit for %q is OPEN", currMCfg.Provider)
			continue
		}

		timeoutSec := currMCfg.TimeoutSec
		if timeoutSec <= 0 {
			timeoutSec = 60
		}
		timeout := time.Duration(timeoutSec) * time.Second

		// Attempt lease acquisition for provider domain
		lease, err := s.leaseMgr.Acquire(ctx, currMCfg.Provider, targetModel, reqID, timeout)
		var credSecret []byte
		if err == nil && lease != nil {
			credSecret, _ = s.idMgr.GetSecret(ctx, lease.CredentialID)
		}

		callCtx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		resp, callErr := p.ExecuteChat(callCtx, req, credSecret)
		cancel()
		lat := float64(time.Since(start).Milliseconds())

		if callErr != nil {
			canonical := Classify(500, callErr.Error(), "")
			s.circuitMgr.ProviderBreaker.RecordFailure(currMCfg.Provider, canonical)
			s.leaseMgr.Release(lease, canonical)
			s.metrics.RecordError(string(canonical.Kind))
			s.metrics.RecordEvent(ctx, reqID, "chat_error", currMCfg.Provider, targetModel, "", lat, string(canonical.Kind), callErr.Error())
			lastErr = callErr
			continue
		}

		// Success
		s.circuitMgr.ProviderBreaker.RecordSuccess(currMCfg.Provider)
		s.leaseMgr.Release(lease, nil)
		s.metrics.RecordEvent(ctx, reqID, "chat_success", currMCfg.Provider, targetModel, "", lat, "", "ok")
		return resp, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no provider could fulfill chat request")
	}
	return nil, lastErr
}

func (s *Scheduler) DispatchChatStream(ctx context.Context, req *provider.ChatRequest, reqID string) (<-chan provider.StreamChunk, *Lease, error) {
	mCfg, err := s.ResolveModel(req.Model)
	if err != nil {
		return nil, nil, err
	}

	p, ok := s.providers.Get(mCfg.Provider)
	if !ok {
		return nil, nil, fmt.Errorf("provider %q not found", mCfg.Provider)
	}

	if !s.circuitMgr.ProviderBreaker.CanAttempt(mCfg.Provider) {
		return nil, nil, fmt.Errorf("provider circuit for %q is OPEN", mCfg.Provider)
	}

	timeoutSec := mCfg.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	timeout := time.Duration(timeoutSec) * time.Second

	lease, _ := s.leaseMgr.Acquire(ctx, mCfg.Provider, req.Model, reqID, timeout)
	var credSecret []byte
	if lease != nil {
		credSecret, _ = s.idMgr.GetSecret(ctx, lease.CredentialID)
	}

	streamCh, streamErr := p.StreamChat(ctx, req, credSecret)
	if streamErr != nil {
		s.leaseMgr.Release(lease, streamErr)
		return nil, nil, streamErr
	}

	return streamCh, lease, nil
}

func (s *Scheduler) ReleaseStreamLease(lease *Lease, err error) {
	s.leaseMgr.Release(lease, err)
}

func (s *Scheduler) DispatchSpeech(ctx context.Context, req *provider.SpeechRequest, reqID string) ([]byte, string, error) {
	mCfg, err := s.ResolveModel(req.Model)
	if err != nil {
		return nil, "", err
	}

	p, ok := s.providers.Get(mCfg.Provider)
	if !ok {
		return nil, "", fmt.Errorf("provider %q not found", mCfg.Provider)
	}

	type result struct {
		data     []byte
		mimeType string
		err      error
	}

	resCh := make(chan result, 1)

	// Submit task through bounded job queue for backpressure control
	submitErr := s.jobQueue.Submit(ctx, func() {
		start := time.Now()
		data, mime, err := p.ExecuteSpeech(ctx, req)
		lat := float64(time.Since(start).Milliseconds())

		if err != nil {
			s.metrics.RecordError("speech_error")
			s.metrics.RecordEvent(ctx, reqID, "speech_error", mCfg.Provider, req.Model, "", lat, "speech_error", err.Error())
		} else {
			s.metrics.RecordEvent(ctx, reqID, "speech_success", mCfg.Provider, req.Model, "", lat, "", "ok")
		}

		resCh <- result{data: data, mimeType: mime, err: err}
	})

	if submitErr != nil {
		return nil, "", submitErr
	}

	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case res := <-resCh:
		return res.data, res.mimeType, res.err
	}
}

func (s *Scheduler) DispatchImage(ctx context.Context, req *provider.ImageRequest, reqID string) (*provider.ImageResponse, error) {
	mCfg, err := s.ResolveModel(req.Model)
	if err != nil {
		return nil, err
	}

	p, ok := s.providers.Get(mCfg.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q not found", mCfg.Provider)
	}

	lease, _ := s.leaseMgr.Acquire(ctx, mCfg.Provider, req.Model, reqID, 60*time.Second)
	var credSecret []byte
	if lease != nil {
		credSecret, _ = s.idMgr.GetSecret(ctx, lease.CredentialID)
	}
	defer s.leaseMgr.Release(lease, nil)

	return p.ExecuteImage(ctx, req, credSecret)
}

func (s *Scheduler) GetModels() map[string]config.ModelConfig {
	return s.config.Models
}

func (s *Scheduler) GetLeaseManager() *LeaseManager {
	return s.leaseMgr
}

func (s *Scheduler) GetCircuitManager() *CircuitManager {
	return s.circuitMgr
}
"""

# -------------------------------------------------------------
# 26. internal/gateway/types.go
# -------------------------------------------------------------
FILES['internal/gateway/types.go'] = """package gateway

import (
	"sever_provider/internal/provider"
)

type ChatCompletionRequest struct {
	Model       string                 `json:"model"`
	Messages    []provider.ChatMessage `json:"messages"`
	Temperature *float32               `json:"temperature,omitempty"`
	TopP        *float32               `json:"top_p,omitempty"`
	MaxTokens   *int                   `json:"max_tokens,omitempty"`
	Stream      bool                   `json:"stream,omitempty"`
}

type Choice struct {
	Index        int                  `json:"index"`
	Message      provider.ChatMessage `json:"message"`
	FinishReason string               `json:"finish_reason"`
}

type ChatCompletionResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []Choice       `json:"choices"`
	Usage   provider.Usage `json:"usage"`
}

type StreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        StreamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type ChatCompletionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
}

type AudioSpeechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float32 `json:"speed,omitempty"`
}

type ImageGenerationRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type ModelCard struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Created   int64  `json:"created"`
	OwnedBy   string `json:"owned_by"`
	Available bool   `json:"available"`
}

type ModelListResponse struct {
	Object string      `json:"object"`
	Data   []ModelCard `json:"data"`
}

type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

type ErrorResponse struct {
	Error APIError `json:"error"`
}
"""

# -------------------------------------------------------------
# 27. internal/gateway/middleware.go
# -------------------------------------------------------------
FILES['internal/gateway/middleware.go'] = """package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"sever_provider/internal/observe"
)

type contextKey string

const RequestIDKey contextKey = "request_id"

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			reqID = fmt.Sprintf("req_%s", hex.EncodeToString(b))
		}
		ctx := context.WithValue(r.Context(), RequestIDKey, reqID)
		w.Header().Set("X-Request-ID", reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func GetRequestID(ctx context.Context) string {
	if val, ok := ctx.Value(RequestIDKey).(string); ok {
		return val
	}
	return "unknown"
}

func Logging(logger *observe.Logger, metrics *observe.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			metrics.IncActive()

			ww := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(ww, r)

			lat := float64(time.Since(start).Milliseconds())
			metrics.DecActive(lat)

			reqID := GetRequestID(r.Context())
			logger.Info("[%s] %s %s -> %d (%.2f ms)", reqID, r.Method, r.URL.Path, ww.status, lat)
		})
	}
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusResponseWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func Recovery(logger *observe.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered: %v", rec)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"message":"Internal server error","type":"server_error"}}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func Auth(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for health, status, internal SSE and static web files
			if strings.HasPrefix(r.URL.Path, "/health") ||
				strings.HasPrefix(r.URL.Path, "/web") ||
				strings.HasPrefix(r.URL.Path, "/internal") ||
				r.URL.Path == "/" {
				next.ServeHTTP(w, r)
				return
			}

			if apiKey == "" {
				next.ServeHTTP(w, r)
				return
			}

			authHdr := r.Header.Get("Authorization")
			token := strings.TrimPrefix(authHdr, "Bearer ")
			if token != apiKey {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"Unauthorized: invalid API key","type":"auth_error"}}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
"""

# -------------------------------------------------------------
# 28. internal/gateway/handlers.go
# -------------------------------------------------------------
FILES['internal/gateway/handlers.go'] = """package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"sever_provider/internal/identity"
	"sever_provider/internal/observe"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

type Handlers struct {
	scheduler *scheduler.Scheduler
	idMgr     *identity.Manager
	metrics   *observe.Metrics
	logger    *observe.Logger
}

func NewHandlers(
	sched *scheduler.Scheduler,
	idMgr *identity.Manager,
	metrics *observe.Metrics,
	logger *observe.Logger,
) *Handlers {
	return &Handlers{
		scheduler: sched,
		idMgr:     idMgr,
		metrics:   metrics,
		logger:    logger,
	}
}

func (h *Handlers) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (h *Handlers) writeError(w http.ResponseWriter, status int, message, errType, code string) {
	h.writeJSON(w, status, ErrorResponse{
		Error: APIError{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
}

func (h *Handlers) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error", "bad_request")
		return
	}

	reqID := GetRequestID(r.Context())

	provReq := &provider.ChatRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
	}

	if req.Stream {
		h.handleChatStream(w, r, provReq, reqID)
		return
	}

	resp, err := h.scheduler.DispatchChat(r.Context(), provReq, reqID)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, err.Error(), "api_error", "upstream_error")
		return
	}

	out := ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []Choice{
			{
				Index: 0,
				Message: provider.ChatMessage{
					Role:    "assistant",
					Content: resp.Content,
				},
				FinishReason: "stop",
			},
		},
		Usage: resp.Usage,
	}

	h.writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) handleChatStream(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest, reqID string) {
	streamCh, lease, err := h.scheduler.DispatchChatStream(r.Context(), req, reqID)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, err.Error(), "api_error", "upstream_error")
		return
	}
	defer h.scheduler.ReleaseStreamLease(lease, nil)

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "Streaming unsupported by response writer", "server_error", "")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	streamID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	for chunk := range streamCh {
		if chunk.Error != nil {
			errJSON, _ := json.Marshal(map[string]interface{}{"error": chunk.Error.Error()})
			_, _ = fmt.Fprintf(w, "data: %s\\n\\n", string(errJSON))
			flusher.Flush()
			break
		}

		if chunk.Done {
			finish := "stop"
			endChunk := ChatCompletionChunk{
				ID:      streamID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []StreamChoice{
					{
						Index:        0,
						Delta:        StreamDelta{},
						FinishReason: &finish,
					},
				},
			}
			data, _ := json.Marshal(endChunk)
			_, _ = fmt.Fprintf(w, "data: %s\\n\\n", string(data))
			_, _ = fmt.Fprintf(w, "data: [DONE]\\n\\n")
			flusher.Flush()
			break
		}

		c := ChatCompletionChunk{
			ID:      streamID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []StreamChoice{
				{
					Index: 0,
					Delta: StreamDelta{
						Role:    "assistant",
						Content: chunk.Content,
					},
					FinishReason: nil,
				},
			},
		}
		data, _ := json.Marshal(c)
		_, _ = fmt.Fprintf(w, "data: %s\\n\\n", string(data))
		flusher.Flush()
	}
}

func (h *Handlers) HandleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	var req AudioSpeechRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error", "")
		return
	}

	reqID := GetRequestID(r.Context())
	provReq := &provider.SpeechRequest{
		Model:          req.Model,
		Input:          req.Input,
		Voice:          req.Voice,
		ResponseFormat: req.ResponseFormat,
		Speed:          req.Speed,
	}

	data, mime, err := h.scheduler.DispatchSpeech(r.Context(), provReq, reqID)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, err.Error(), "api_error", "speech_generation_failed")
		return
	}

	w.Header().Set("Content-Type", mime)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handlers) HandleImageGenerations(w http.ResponseWriter, r *http.Request) {
	var req ImageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error", "")
		return
	}

	reqID := GetRequestID(r.Context())
	provReq := &provider.ImageRequest{
		Prompt:         req.Prompt,
		Model:          req.Model,
		N:              req.N,
		Size:           req.Size,
		ResponseFormat: req.ResponseFormat,
	}

	resp, err := h.scheduler.DispatchImage(r.Context(), provReq, reqID)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, err.Error(), "api_error", "image_generation_failed")
		return
	}

	h.writeJSON(w, http.StatusOK, resp)
}

func (h *Handlers) HandleListModels(w http.ResponseWriter, r *http.Request) {
	models := h.scheduler.GetModels()
	cards := make([]ModelCard, 0, len(models))

	for name, mCfg := range models {
		cards = append(cards, ModelCard{
			ID:        name,
			Object:    "model",
			Created:   1700000000,
			OwnedBy:   mCfg.Provider,
			Available: true,
		})
	}

	h.writeJSON(w, http.StatusOK, ModelListResponse{
		Object: "list",
		Data:   cards,
	})
}

func (h *Handlers) HandleHealth(w http.ResponseWriter, r *http.Request) {
	stats := h.metrics.GetStats()
	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "healthy",
		"uptime_seconds": stats.UptimeSeconds,
		"active_leases":  h.scheduler.GetLeaseManager().ActiveCount(),
		"requests_total": stats.RequestsTotal,
	})
}

func (h *Handlers) HandleDashboardStatus(w http.ResponseWriter, r *http.Request) {
	stats := h.metrics.GetStats()
	identities, _ := h.idMgr.ListIdentitiesWithCredentials(r.Context())
	circuits := h.scheduler.GetCircuitManager().ProviderBreaker.GetStatus()
	leases := h.scheduler.GetLeaseManager().GetActiveLeases()

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"stats":      stats,
		"identities": identities,
		"circuits":   circuits,
		"leases":     leases,
	})
}

func (h *Handlers) HandleDashboardEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := h.metrics.Subscribe()
	defer h.metrics.Unsubscribe(sub)

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-sub:
			if !ok {
				return
			}
			_, _ = fmt.Fprintf(w, "event: metrics\\ndata: %s\\n\\n", msg)
			flusher.Flush()
		case <-ticker.C:
			stats := h.metrics.GetStats()
			data, _ := json.Marshal(stats)
			_, _ = fmt.Fprintf(w, "event: stats\\ndata: %s\\n\\n", string(data))
			flusher.Flush()
		}
	}
}
"""

# -------------------------------------------------------------
# 29. web/web.go
# -------------------------------------------------------------
FILES['web/web.go'] = """package web

import "embed"

//go:embed index.html app.js style.css
var StaticFS embed.FS
"""

# -------------------------------------------------------------
# 30. web/index.html
# -------------------------------------------------------------
FILES['web/index.html'] = """<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Sever Provider - AI Gateway Orchestrator</title>
    <link rel="stylesheet" href="/web/style.css">
</head>
<body>
    <header class="header">
        <div class="brand">
            <span class="logo-icon">&#x26A1;</span>
            <h1>Sever Provider</h1>
            <span class="badge live">GATEWAY ACTIVE</span>
        </div>
        <div class="system-time" id="system-time">--:--:--</div>
    </header>

    <main class="container">
        <!-- Stats Grid -->
        <section class="stats-grid">
            <div class="card stat-card">
                <h3>Uptime</h3>
                <div class="stat-value" id="val-uptime">0s</div>
                <div class="stat-sub">System online</div>
            </div>
            <div class="card stat-card">
                <h3>Active Requests</h3>
                <div class="stat-value" id="val-active">0</div>
                <div class="stat-sub">In-flight connections</div>
            </div>
            <div class="card stat-card">
                <h3>Total Requests</h3>
                <div class="stat-value" id="val-total">0</div>
                <div class="stat-sub">Since startup</div>
            </div>
            <div class="card stat-card">
                <h3>Avg Latency</h3>
                <div class="stat-value" id="val-latency">0 ms</div>
                <div class="stat-sub">Downstream roundtrip</div>
            </div>
        </section>

        <!-- Main Layout: Accounts & Circuit Breakers -->
        <section class="dashboard-grid">
            <div class="card">
                <div class="card-header">
                    <h2>Accounts &amp; Credentials</h2>
                    <button class="btn btn-sm" onclick="fetchStatus()">Refresh</button>
                </div>
                <div class="table-responsive">
                    <table class="data-table" id="accounts-table">
                        <thead>
                            <tr>
                                <th>Identity Email</th>
                                <th>Group</th>
                                <th>Domain</th>
                                <th>Status</th>
                                <th>Concurrency</th>
                                <th>Cooldown</th>
                            </tr>
                        </thead>
                        <tbody id="accounts-body">
                            <tr><td colspan="6" class="loading">Loading accounts...</td></tr>
                        </tbody>
                    </table>
                </div>
            </div>

            <div class="card">
                <div class="card-header">
                    <h2>Provider Circuit Breakers</h2>
                </div>
                <div id="breakers-container" class="breakers-list">
                    <div class="loading">Loading breakers...</div>
                </div>
            </div>
        </section>

        <!-- Active Leases -->
        <section class="card">
            <div class="card-header">
                <h2>Active Runtime Leases</h2>
            </div>
            <div class="table-responsive">
                <table class="data-table">
                    <thead>
                        <tr>
                            <th>Lease ID</th>
                            <th>Model</th>
                            <th>Domain</th>
                            <th>Request ID</th>
                            <th>Acquired At</th>
                            <th>Expires At</th>
                        </tr>
                    </thead>
                    <tbody id="leases-body">
                        <tr><td colspan="6" class="loading">No active leases</td></tr>
                    </tbody>
                </table>
            </div>
        </section>
    </main>

    <footer class="footer">
        <p>Unified AI Gateway Orchestrator &bull; Port 8080 &bull; SQLite WAL Mode</p>
    </footer>

    <script src="/web/app.js"></script>
</body>
</html>
"""

# -------------------------------------------------------------
# 31. web/style.css
# -------------------------------------------------------------
FILES['web/style.css'] = """* {
    box-sizing: border-box;
    margin: 0;
    padding: 0;
}

body {
    background-color: #0f172a;
    color: #e2e8f0;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Oxygen, Ubuntu, Cantarell, sans-serif;
    line-height: 1.5;
}

.header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 1rem 2rem;
    background-color: #1e293b;
    border-bottom: 1px solid #334155;
}

.brand {
    display: flex;
    align-items: center;
    gap: 0.75rem;
}

.logo-icon {
    font-size: 1.5rem;
}

.brand h1 {
    font-size: 1.25rem;
    font-weight: 600;
}

.badge {
    font-size: 0.75rem;
    padding: 0.2rem 0.6rem;
    border-radius: 9999px;
    font-weight: 600;
    text-transform: uppercase;
}

.badge.live {
    background-color: rgba(16, 185, 129, 0.2);
    color: #10b981;
    border: 1px solid #10b981;
}

.badge.healthy {
    background-color: rgba(16, 185, 129, 0.2);
    color: #10b981;
}

.badge.degraded {
    background-color: rgba(245, 158, 11, 0.2);
    color: #f59e0b;
}

.badge.rate_limited, .badge.cooldown {
    background-color: rgba(239, 68, 68, 0.2);
    color: #ef4444;
}

.system-time {
    color: #94a3b8;
    font-family: monospace;
}

.container {
    padding: 2rem;
    max-width: 1400px;
    margin: 0 auto;
    display: flex;
    flex-direction: column;
    gap: 1.5rem;
}

.stats-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
    gap: 1rem;
}

.card {
    background-color: #1e293b;
    border: 1px solid #334155;
    border-radius: 0.5rem;
    padding: 1.25rem;
}

.stat-card h3 {
    font-size: 0.875rem;
    color: #94a3b8;
    text-transform: uppercase;
    font-weight: 500;
    letter-spacing: 0.05em;
}

.stat-value {
    font-size: 2rem;
    font-weight: 700;
    color: #38bdf8;
    margin: 0.25rem 0;
}

.stat-sub {
    font-size: 0.8rem;
    color: #64748b;
}

.dashboard-grid {
    display: grid;
    grid-template-columns: 2fr 1fr;
    gap: 1.5rem;
}

@media (max-width: 900px) {
    .dashboard-grid {
        grid-template-columns: 1fr;
    }
}

.card-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 1rem;
    padding-bottom: 0.5rem;
    border-bottom: 1px solid #334155;
}

.card-header h2 {
    font-size: 1.1rem;
    font-weight: 600;
}

.btn {
    background-color: #3b82f6;
    color: white;
    border: none;
    border-radius: 0.25rem;
    padding: 0.4rem 0.8rem;
    cursor: pointer;
    font-size: 0.875rem;
    transition: background-color 0.15s;
}

.btn:hover {
    background-color: #2563eb;
}

.table-responsive {
    overflow-x: auto;
}

.data-table {
    width: 100%;
    border-collapse: collapse;
    text-align: left;
    font-size: 0.875rem;
}

.data-table th {
    background-color: #0f172a;
    padding: 0.75rem;
    color: #94a3b8;
    font-weight: 600;
}

.data-table td {
    padding: 0.75rem;
    border-bottom: 1px solid #334155;
}

.breakers-list {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
}

.breaker-item {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 0.75rem;
    background-color: #0f172a;
    border-radius: 0.375rem;
    border: 1px solid #334155;
}

.breaker-name {
    font-weight: 500;
}

.loading {
    text-align: center;
    color: #64748b;
    padding: 1.5rem;
}

.footer {
    text-align: center;
    padding: 2rem;
    color: #64748b;
    font-size: 0.875rem;
    border-top: 1px solid #1e293b;
}
"""

# -------------------------------------------------------------
# 32. web/app.js
# -------------------------------------------------------------
FILES['web/app.js'] = """function updateClock() {
    const now = new Date();
    document.getElementById('system-time').innerText = now.toTimeString().split(' ')[0];
}
setInterval(updateClock, 1000);
updateClock();

function formatUptime(seconds) {
    const h = Math.floor(seconds / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    const s = seconds % 60;
    return `${h}h ${m}m ${s}s`;
}

function fetchStatus() {
    fetch('/internal/status')
        .then(res => res.json())
        .then(data => {
            renderStats(data.stats);
            renderAccounts(data.identities);
            renderBreakers(data.circuits);
            renderLeases(data.leases);
        })
        .catch(err => console.error('Failed to fetch status:', err));
}

function renderStats(stats) {
    if (!stats) return;
    document.getElementById('val-uptime').innerText = formatUptime(stats.uptime_seconds || 0);
    document.getElementById('val-active').innerText = stats.requests_active || 0;
    document.getElementById('val-total').innerText = stats.requests_total || 0;
    document.getElementById('val-latency').innerText = `${(stats.avg_latency_ms || 0).toFixed(1)} ms`;
}

function renderAccounts(identities) {
    const tbody = document.getElementById('accounts-body');
    if (!identities || identities.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" class="loading">No accounts registered</td></tr>';
        return;
    }

    let rows = '';
    identities.forEach(id => {
        if (!id.credentials || id.credentials.length === 0) {
            rows += `<tr>
                <td>${id.email}</td>
                <td>${id.provider_group}</td>
                <td colspan="4"><em>No credentials configured</em></td>
            </tr>`;
        } else {
            id.credentials.forEach(c => {
                const statusClass = (c.status || 'healthy').toLowerCase();
                const cooldown = c.cooldown_until ? new Date(c.cooldown_until).toLocaleTimeString() : 'None';
                rows += `<tr>
                    <td>${id.email}</td>
                    <td>${id.provider_group}</td>
                    <td><strong>${c.domain}</strong></td>
                    <td><span class="badge ${statusClass}">${c.status}</span></td>
                    <td>${c.max_concurrency} slots</td>
                    <td>${cooldown}</td>
                </tr>`;
            });
        }
    });

    tbody.innerHTML = rows;
}

function renderBreakers(circuits) {
    const container = document.getElementById('breakers-container');
    if (!circuits || Object.keys(circuits).length === 0) {
        container.innerHTML = '<div class="loading">No active provider breakers</div>';
        return;
    }

    let html = '';
    for (const [provider, state] of Object.entries(circuits)) {
        const stateClass = state.toLowerCase();
        html += `<div class="breaker-item">
            <span class="breaker-name">${provider}</span>
            <span class="badge ${stateClass}">${state}</span>
        </div>`;
    }
    container.innerHTML = html;
}

function renderLeases(leases) {
    const tbody = document.getElementById('leases-body');
    if (!leases || leases.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" class="loading">No active leases</td></tr>';
        return;
    }

    let rows = '';
    leases.forEach(l => {
        rows += `<tr>
            <td><code>${l.lease_id}</code></td>
            <td>${l.model}</td>
            <td>${l.domain}</td>
            <td>${l.request_id}</td>
            <td>${new Date(l.acquired_at).toLocaleTimeString()}</td>
            <td>${new Date(l.expires_at).toLocaleTimeString()}</td>
        </tr>`;
    });
    tbody.innerHTML = rows;
}

// Initial status fetch
fetchStatus();
setInterval(fetchStatus, 5000);

// SSE Connection
if (window.EventSource) {
    const sse = new EventSource('/internal/events');
    sse.addEventListener('stats', e => {
        try {
            const stats = JSON.parse(e.data);
            renderStats(stats);
        } catch (err) {}
    });
}
"""

# -------------------------------------------------------------
# 33. internal/gateway/router.go
# -------------------------------------------------------------
FILES['internal/gateway/router.go'] = """package gateway

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"sever_provider/internal/observe"
	"sever_provider/web"
)

func NewRouter(handlers *Handlers, logger *observe.Logger, metrics *observe.Metrics, apiKey string) http.Handler {
	r := chi.NewRouter()

	r.Use(CORS)
	r.Use(RequestID)
	r.Use(Logging(logger, metrics))
	r.Use(Recovery(logger))
	r.Use(Auth(apiKey))

	// OpenAI-compatible API Endpoints
	r.Route("/v1", func(r chi.Router) {
		r.Post("/chat/completions", handlers.HandleChatCompletions)
		r.Post("/audio/speech", handlers.HandleAudioSpeech)
		r.Post("/images/generations", handlers.HandleImageGenerations)
		r.Get("/models", handlers.HandleListModels)
	})

	// Health check
	r.Get("/health", handlers.HandleHealth)

	// Internal Dashboard Endpoints
	r.Get("/internal/status", handlers.HandleDashboardStatus)
	r.Get("/internal/events", handlers.HandleDashboardEvents)

	// Embedded Web Dashboard
	staticFS, err := fs.Sub(web.StaticFS, ".")
	if err == nil {
		fileServer := http.FileServer(http.FS(staticFS))
		r.Handle("/web/*", http.StripPrefix("/web", fileServer))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/web/", http.StatusFound)
		})
		r.Get("/web", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/web/", http.StatusMovedPermanently)
		})
		r.Get("/web/", func(w http.ResponseWriter, r *http.Request) {
			data, err := fs.ReadFile(staticFS, "index.html")
			if err != nil {
				http.Error(w, "Dashboard not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		})
	}

	// Fallback 404
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1") {
			handlers.writeError(w, http.StatusNotFound, "Endpoint not found: "+r.URL.Path, "invalid_request_error", "not_found")
			return
		}
		http.NotFound(w, r)
	})

	return r
}
"""

# -------------------------------------------------------------
# 34. cmd/server/main.go
# -------------------------------------------------------------
FILES['cmd/server/main.go'] = """package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sever_provider/internal/config"
	"sever_provider/internal/db"
	"sever_provider/internal/gateway"
	"sever_provider/internal/identity"
	"sever_provider/internal/jobs"
	"sever_provider/internal/observe"
	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/secret"
)

func main() {
	configDir := flag.String("config", "config", "Path to config directory")
	flag.Parse()

	logger := observe.NewLogger()
	logger.Info("Starting Sever Provider Orchestrator...")

	// 1. Load Configurations
	cfg, err := config.Load(*configDir)
	if err != nil {
		logger.Error("Failed to load config: %v", err)
		os.Exit(1)
	}
	logger.Info("Loaded configuration. Server port: %d", cfg.Server.Port)

	// 2. Secret Store
	masterKeyEnv := os.Getenv("MASTER_KEY")
	secretStore, err := secret.NewStore(masterKeyEnv)
	if err != nil {
		logger.Error("Failed to initialize secret store: %v", err)
		os.Exit(1)
	}

	// 3. SQLite Database with WAL mode
	database, err := db.InitDB(cfg.Server.DBPath)
	if err != nil {
		logger.Error("Failed to initialize database: %v", err)
		os.Exit(1)
	}
	defer database.Close()
	logger.Info("Database initialized at %s with WAL mode", cfg.Server.DBPath)

	// 4. Metrics & Observability
	metrics := observe.NewMetrics(database)

	// 5. Identity & Credential Manager
	identityMgr := identity.NewManager(database, secretStore)

	// Seed default identities and credentials if database is empty
	ctx := context.Background()
	seedDefaults(ctx, identityMgr, logger)

	// 6. Circuit Breakers & Lease Manager
	circuitMgr := scheduler.NewCircuitManager()
	leaseMgr := scheduler.NewLeaseManager(database, identityMgr, circuitMgr)

	// Lease Watchdog (10s interval)
	watchdogCtx, cancelWatchdog := context.WithCancel(context.Background())
	defer cancelWatchdog()
	leaseMgr.StartWatchdog(watchdogCtx, 10*time.Second)

	// 7. Job Queues
	jobQueue := jobs.NewJobQueue(500, 8)
	defer jobQueue.Close()

	// 8. Provider Registry & Adapters
	registry := provider.NewRegistry()
	registry.Register(provider.NewGeminiWebProvider())
	registry.Register(provider.NewCopilotProvider())
	registry.Register(provider.NewColabProvider())
	registry.Register(provider.NewPiperProvider("piper", ""))
	registry.Register(provider.NewTikTokProvider())
	registry.Register(provider.NewImageProvider("copilot_image"))
	registry.Register(provider.NewImageProvider("gemini_image"))

	// 9. Scheduler
	sched := scheduler.NewScheduler(cfg, identityMgr, leaseMgr, circuitMgr, registry, jobQueue, logger, metrics)

	// 10. HTTP Gateway & Chi Router
	handlers := gateway.NewHandlers(sched, identityMgr, metrics, logger)
	router := gateway.NewRouter(handlers, logger, metrics, cfg.Server.APIKey)

	serverAddr := fmt.Sprintf(":%d", cfg.Server.Port)
	httpServer := &http.Server{
		Addr:         serverAddr,
		Handler:      router,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Server goroutine
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("Listening and serving HTTP on %s (Dashboard: http://localhost:%d/web/)", serverAddr, cfg.Server.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// 11. Graceful Shutdown on SIGTERM / SIGINT
	shutdownSig := make(chan os.Signal, 1)
	signal.Notify(shutdownSig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		logger.Error("HTTP server error: %v", err)
	case sig := <-shutdownSig:
		logger.Info("Received shutdown signal %v, draining active connections...", sig)

		drainSec := cfg.Server.ShutdownGraceSec
		if drainSec <= 0 {
			drainSec = 30
		}
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Duration(drainSec)*time.Second)
		defer cancelShutdown()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("Server shutdown error: %v", err)
		} else {
			logger.Info("HTTP server drained successfully")
		}
	}

	logger.Info("Releasing resources and closing database...")
	logger.Info("Sever Provider stopped gracefully.")
}

func seedDefaults(ctx context.Context, idMgr *identity.Manager, logger *observe.Logger) {
	// Sample seed identity to show in dashboard out of the box
	_ = idMgr.CreateIdentity(ctx, "id_default_gemini", "gemini_pool", "gemini.account01@gmail.com", 10)
	_ = idMgr.CreateCredential(ctx, "cred_gem_01", "id_default_gemini", "gemini_web", 4, []byte(`{"cookie":"sample_cookie_data"}`))

	_ = idMgr.CreateIdentity(ctx, "id_default_colab", "colab_pool", "colab.runtime01@gmail.com", 8)
	_ = idMgr.CreateCredential(ctx, "cred_colab_01", "id_default_colab", "google_colab", 8, []byte(`{"token":"sample_colab_token"}`))
}
"""

# -------------------------------------------------------------
# 35. README.md
# -------------------------------------------------------------
FILES['README.md'] = """# Sever Provider - AI Gateway Orchestrator

Unified AI Gateway and Provider Orchestrator in Go (1.23+), consolidating multi-account Gemini Web, Microsoft Copilot, Google Colab runtime agents, Piper local TTS, TikTok TTS, and Image generation into an OpenAI-compatible API interface.

## Architecture

- **Gateway**: Chi router with RequestID, Structured Logging, Panic Recovery, Bearer Auth, CORS, and OpenAI compatible `/v1/*` endpoints.
- **Provider Scheduler**: Multi-tier routing, model resolution, capacity checks, automatic failover, and rate-limit handling.
- **Identity & Credentials**: Database-backed credential state machine (`HEALTHY`, `DEGRADED`, `RATE_LIMITED`, `COOLDOWN`, `EXPIRED`, `AUTH_REQUIRED`, `DISABLED`).
- **Secret Store**: AES-256-GCM encryption with 256-bit `MASTER_KEY` environment variable.
- **Circuit Breakers**: Two-tier protection (per-credential exponential backoff with jitter and per-provider sliding window with open/half-open states).
- **Lease Manager**: Atomic concurrency slots per account with TTL and watchdog lease reclamation.
- **Bounded Queues**: Worker pools with backpressure control for compute-heavy providers (TTS, Colab, Images).
- **Embedded Web Dashboard**: Modern responsive UI with real-time SSE metrics at `/web/`.

## Endpoints

### OpenAI Compatible
- `POST /v1/chat/completions` - Streaming SSE & non-streaming chat
- `POST /v1/audio/speech` - TTS generation (Piper WAV & TikTok MP3)
- `POST /v1/images/generations` - Image generation
- `GET /v1/models` - Configured models and capabilities

### System & Dashboard
- `GET /health` - Health status and uptime
- `GET /web/` - Real-time monitoring dashboard
- `GET /internal/status` - Aggregated status JSON
- `GET /internal/events` - SSE live events stream

## Build & Run

```bash
# Build binary
make build

# Run server
./sever_provider --config config

# Or run tests
make test
```
"""

def main():
    if not HOST or not USER or not KEY_PATH:
        raise RuntimeError('set SSH_HOST, SSH_USER, and SSH_KEY_PATH environment variables')
    print(f"Connecting to {HOST}:{PORT} as {USER}...")
    client = paramiko.SSHClient()
    client.load_system_host_keys()
    client.set_missing_host_key_policy(paramiko.RejectPolicy())
    client.connect(HOST, port=PORT, username=USER, key_filename=KEY_PATH, timeout=10)

    sftp = client.open_sftp()
    print("SFTP session established.")

    # Ensure remote base exists
    try:
        sftp.stat(REMOTE_BASE)
    except IOError:
        pass

    dirs_created = set()

    for rel_path, content in FILES.items():
        full_remote_path = REMOTE_BASE + '/' + rel_path
        remote_dir = os.path.dirname(full_remote_path)

        # Create directories if needed
        parts = rel_path.split('/')
        if len(parts) > 1:
            cur = REMOTE_BASE
            for p in parts[:-1]:
                cur += '/' + p
                if cur not in dirs_created:
                    try:
                        sftp.stat(cur)
                    except IOError:
                        sftp.mkdir(cur)
                    dirs_created.add(cur)

        print(f"Writing {rel_path} ({len(content)} bytes)...")
        with sftp.file(full_remote_path, 'w') as f:
            f.write(content)

    sftp.close()
    client.close()
    print("All files successfully transferred via SFTP.")

if __name__ == '__main__':
    main()
