package colabsupervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	TokenDir   string
	Command    string
	MaxRuntime time.Duration
	Prewarm    time.Duration
	Cooldown   time.Duration
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
		accountID := "colab-" + name

		var cooldown sql.NullTime
		var state string
		if s.db != nil {
			_ = s.db.QueryRowContext(ctx, `SELECT state, cooldown_until FROM account_cooldowns WHERE identity_id=?`, accountID).Scan(&state, &cooldown)
		}
		acc := Account{ID: accountID, Email: email, Status: "READY"}
		if cooldown.Valid && now.Before(cooldown.Time) {
			acc.Status = "COOLDOWN"
			acc.CooldownUntil = &cooldown.Time
		}
		accounts = append(accounts, acc)
	}
	return accounts, nil
}

// RecordCooldown marks an account in cooldown for duration.
func (s *Supervisor) RecordCooldown(ctx context.Context, accountID string, duration time.Duration) error {
	if s == nil || s.db == nil {
		return nil
	}
	until := time.Now().Add(duration)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at)
		VALUES (?, 'COOLDOWN', ?, CURRENT_TIMESTAMP)
		ON CONFLICT(identity_id) DO UPDATE SET
			state='COOLDOWN',
			cooldown_until=excluded.cooldown_until,
			updated_at=CURRENT_TIMESTAMP`,
		accountID, until)
	return err
}

// Rotate triggers rotation to the next available Colab account.
func (s *Supervisor) Rotate(ctx context.Context) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
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
	if err := s.runCommand(ctx, "rotate", eligible.ID); err != nil {
		return err
	}
	return s.RecordCooldown(ctx, eligible.ID, s.cfg.Cooldown)
}

func (s *Supervisor) StartAccount(ctx context.Context, accountID string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	if strings.TrimSpace(accountID) == "" {
		return errors.New("account id is required")
	}
	return s.runCommand(ctx, "start", accountID)
}

func (s *Supervisor) StopAccount(ctx context.Context, accountID string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	if strings.TrimSpace(accountID) == "" {
		return errors.New("account id is required")
	}
	return s.runCommand(ctx, "stop", accountID)
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
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, parts[0], append(parts[1:], action, accountID)...)
	cmd.Env = append(os.Environ(), "COLAB_ACCOUNT_ID="+accountID, "COLAB_ACTION="+action)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("colab supervisor command failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Start runs a lightweight maintenance loop. It does not read token contents.
func (s *Supervisor) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.DiscoverAccounts(ctx)
			}
		}
	}()
}
