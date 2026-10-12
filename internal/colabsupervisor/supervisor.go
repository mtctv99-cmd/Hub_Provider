package colabsupervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNotConfigured      = errors.New("colab supervisor command or token directory is not configured")
	ErrNoAccountAvailable = errors.New("no eligible colab account available (all accounts in cooldown)")
)

const (
	DefaultMaxRuntime = 4 * time.Hour
	DefaultPrewarm    = 10 * time.Minute
	DefaultCooldown   = 16 * time.Hour
)

type Config struct {
	TokenDir       string
	Command        string
	MaxRuntime     time.Duration
	Prewarm        time.Duration
	Cooldown       time.Duration
	CommandTimeout time.Duration
}

type Account struct {
	ID            string     `json:"id"`
	Email         string     `json:"email"`
	Status        string     `json:"status"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	ActiveSince   *time.Time `json:"active_since,omitempty"`
}

type Supervisor struct {
	cfg Config
	db  *sql.DB
	mu  sync.Mutex
	// busy guards long-running start/stop commands so ticker ticks and
	// admin HTTP calls pile up instead of stacking colab rotations.
	busy atomic.Bool
}

func (s *Supervisor) tryBegin() bool {
	return s.busy.CompareAndSwap(false, true)
}

func (s *Supervisor) end() {
	s.busy.Store(false)
}

func New(db *sql.DB, cfg Config) *Supervisor {
	if cfg.MaxRuntime <= 0 {
		cfg.MaxRuntime = DefaultMaxRuntime
	}
	if cfg.Prewarm <= 0 {
		cfg.Prewarm = DefaultPrewarm
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultCooldown
	}
	return &Supervisor{db: db, cfg: cfg}
}

func (s *Supervisor) Configured() bool {
	return s != nil && (s.cfg.TokenDir != "" || s.cfg.Command != "")
}

func (s *Supervisor) CanExecute() bool {
	return s != nil && strings.TrimSpace(s.cfg.Command) != ""
}

// DiscoverAccounts lists token files without exposing file content or tokens.
func (s *Supervisor) DiscoverAccounts(ctx context.Context) ([]Account, error) {
	if s == nil || s.cfg.TokenDir == "" {
		return []Account{}, nil
	}
	entries, err := os.ReadDir(s.cfg.TokenDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Account{}, nil
		}
		return nil, fmt.Errorf("read token dir: %w", err)
	}
	now := time.Now()
	var accounts []Account
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "token_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "token_"), ".json")
		email := name
		if strings.Contains(name, "_at_") {
			parts := strings.SplitN(name, "_at_", 2)
			email = parts[0] + "@" + strings.ReplaceAll(parts[1], "_", ".")
		}
		accountID := "colab-" + name

		var cooldown sql.NullTime
		var state string
		var updatedAt sql.NullTime
		if s.db != nil {
			var raw any
			if err := s.db.QueryRowContext(ctx, `SELECT state, cooldown_until, updated_at FROM account_cooldowns WHERE identity_id=? OR identity_id=? OR identity_id=? ORDER BY updated_at DESC LIMIT 1`, accountID, name, email).Scan(&state, &raw, &updatedAt); err == nil {
				if t, ok := parseCooldownTime(raw); ok {
					cooldown.Time, cooldown.Valid = t, true
				}
			}
		}
		acc := Account{ID: accountID, Email: email, Status: "READY"}
		if cooldown.Valid && now.Before(cooldown.Time) {
			acc.Status = "COOLDOWN"
			acc.CooldownUntil = &cooldown.Time
		} else if state == "ACTIVE" {
			acc.Status = "ACTIVE"
			if updatedAt.Valid {
				acc.ActiveSince = &updatedAt.Time
			}
		}
		accounts = append(accounts, acc)
	}
	return accounts, nil
}

// parseCooldownTime accepts the mixed cooldown_until encodings found in the
// wild: DATETIME strings written by Go, and unix-epoch REALs written by the
// legacy Python rotator.
func parseCooldownTime(raw any) (time.Time, bool) {
	switch v := raw.(type) {
	case nil:
		return time.Time{}, false
	case time.Time:
		return v, !v.IsZero()
	case int64:
		return unixSecondsToTime(float64(v))
	case float64:
		return unixSecondsToTime(v)
	case []byte:
		return parseCooldownTime(string(v))
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}, false
		}
		for _, layout := range []string{
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05Z07:00",
			time.RFC3339Nano,
			time.RFC3339,
		} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
		}
		var f float64
		if _, err := fmt.Sscanf(s, "%f", &f); err == nil {
			return unixSecondsToTime(f)
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

func unixSecondsToTime(f float64) (time.Time, bool) {
	if f <= 0 {
		return time.Time{}, false
	}
	// Tolerate millisecond-epoch values.
	for f > 1e12 {
		f /= 1000
	}
	secs := int64(f)
	return time.Unix(secs, int64((f - float64(secs)) * 1e9)), true
}

// RecordCooldown marks an account in cooldown for duration.
func (s *Supervisor) RecordCooldown(ctx context.Context, accountID string, duration time.Duration) error {
	if s == nil || s.db == nil {
		return nil
	}
	until := time.Now().Add(duration)
	rawName := strings.TrimPrefix(accountID, "colab-")
	_, err := s.db.ExecContext(ctx, `
			INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at)
			VALUES (?, 'COOLDOWN', ?, CURRENT_TIMESTAMP)
			ON CONFLICT(identity_id) DO UPDATE SET
				state='COOLDOWN',
				cooldown_until=excluded.cooldown_until,
				updated_at=CURRENT_TIMESTAMP`,
		accountID, until)
	if err != nil {
		_, err = s.db.ExecContext(ctx, `
				INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at)
				VALUES (?, 'COOLDOWN', ?, CURRENT_TIMESTAMP)
				ON CONFLICT(identity_id) DO UPDATE SET
					state='COOLDOWN',
					cooldown_until=excluded.cooldown_until,
					updated_at=CURRENT_TIMESTAMP`,
			rawName, until)
	}
	return err
}

// RecordActive marks an account as currently ACTIVE.
func (s *Supervisor) RecordActive(ctx context.Context, accountID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	rawName := strings.TrimPrefix(accountID, "colab-")
	_, err := s.db.ExecContext(ctx, `
			INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at)
			VALUES (?, 'ACTIVE', NULL, CURRENT_TIMESTAMP)
			ON CONFLICT(identity_id) DO UPDATE SET
				state='ACTIVE',
				cooldown_until=NULL,
				updated_at=CURRENT_TIMESTAMP`,
		accountID)
	if err != nil {
		_, err = s.db.ExecContext(ctx, `
				INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at)
				VALUES (?, 'ACTIVE', NULL, CURRENT_TIMESTAMP)
				ON CONFLICT(identity_id) DO UPDATE SET
					state='ACTIVE',
					cooldown_until=NULL,
					updated_at=CURRENT_TIMESTAMP`,
			rawName)
	}
	return err
}

// Rotate triggers rotation: stops current active account(s), puts them in cooldown,
// and starts the next eligible READY account as ACTIVE.
func (s *Supervisor) Rotate(ctx context.Context) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	if !s.tryBegin() {
		return errors.New("colab rotation already in progress")
	}
	defer s.end()
	s.mu.Lock()
	defer s.mu.Unlock()

	accounts, err := s.DiscoverAccounts(ctx)
	if err != nil {
		return err
	}
	var eligible *Account
	for i := range accounts {
		if accounts[i].Status == "READY" {
			eligible = &accounts[i]
			break
		}
	}
	if eligible == nil {
		return ErrNoAccountAvailable
	}

	// 1. Retire any active accounts and enter cooldown
	for _, acc := range accounts {
		if acc.Status == "ACTIVE" && acc.ID != eligible.ID {
			_ = s.runCommand(ctx, "stop", acc.ID)
			_ = s.RecordCooldown(ctx, acc.ID, s.cfg.Cooldown)
		}
	}

	// 2. Start eligible account
	if err := s.runCommand(ctx, "start", eligible.ID); err != nil {
		_ = s.RecordCooldown(ctx, eligible.ID, 3*time.Minute)
		return err
	}

	// 3. Mark newly allocated account as ACTIVE
	return s.RecordActive(ctx, eligible.ID)
}

func (s *Supervisor) StartAccount(ctx context.Context, accountID string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	if strings.TrimSpace(accountID) == "" {
		return errors.New("account id is required")
	}
	if !s.tryBegin() {
		return errors.New("colab rotation already in progress")
	}
	defer s.end()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.runCommand(ctx, "start", accountID); err != nil {
		return err
	}
	return s.RecordActive(ctx, accountID)
}

func (s *Supervisor) StopAccount(ctx context.Context, accountID string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	if strings.TrimSpace(accountID) == "" {
		return errors.New("account id is required")
	}
	if !s.tryBegin() {
		return errors.New("colab rotation already in progress")
	}
	defer s.end()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.runCommand(ctx, "stop", accountID); err != nil {
		return err
	}
	return s.RecordCooldown(ctx, accountID, s.cfg.Cooldown)
}

func (s *Supervisor) runCommand(ctx context.Context, action, accountID string) error {
	parts := strings.Fields(s.cfg.Command)
	if len(parts) == 0 {
		return ErrNotConfigured
	}
	for _, part := range parts {
		if strings.ContainsAny(part, ";&|$<>`") {
			return errors.New("unsafe supervisor command")
		}
	}
	timeout := 300 * time.Second
	if s.cfg.CommandTimeout > 0 {
		timeout = s.cfg.CommandTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, parts[0], append(parts[1:], action, accountID)...)
	cmd.Env = append(os.Environ(), "COLAB_ACCOUNT_ID="+accountID, "COLAB_ACTION="+action)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("colab supervisor command failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Start runs a maintenance loop: bootstrap the first worker when none is
// ACTIVE, and auto-rotate when MaxRuntime expires.
func (s *Supervisor) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go func() {
		// First check immediately so a fresh server does not wait a full tick.
		s.check(ctx)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.check(ctx)
			}
		}
	}()
}

func (s *Supervisor) check(ctx context.Context) {
	if ctx.Err() != nil || !s.CanExecute() {
		return
	}
	iterCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	accounts, err := s.DiscoverAccounts(iterCtx)
	if err != nil {
		cancel()
		return
	}
	cancel()
	now := time.Now()
	hasActive := false
	hasReady := false
	for _, acc := range accounts {
		switch acc.Status {
		case "ACTIVE":
			hasActive = true
			if acc.ActiveSince != nil && now.Sub(*acc.ActiveSince) >= s.cfg.MaxRuntime {
				log.Printf("[COLAB_SUPERVISOR] account %s reached max runtime (%v); rotating...", acc.ID, s.cfg.MaxRuntime)
				_ = s.Rotate(ctx)
				return
			}
		case "READY":
			hasReady = true
		}
	}
	// Bootstrap: no worker alive and at least one account out of cooldown.
	if !hasActive && hasReady {
		log.Printf("[COLAB_SUPERVISOR] no active worker; bootstrapping first colab worker...")
		if err := s.Rotate(ctx); err != nil {
			log.Printf("[COLAB_SUPERVISOR] bootstrap failed: %v", err)
		}
	}
}
