package colabsupervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
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
	// StartGrace bounds how long Rotate waits to distinguish fast failure
	// from a healthy boot before marking the account ACTIVE. The worker
	// process itself runs for hours; only the verdict is bounded.
	StartGrace time.Duration
}

type Account struct {
	ID            string     `json:"id"`
	Email         string     `json:"email"`
	Status        string     `json:"status"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	ActiveSince   *time.Time `json:"active_since,omitempty"`
	// LastAttempt is when any cooldown row was last written for this account —
	// the best proxy for "last time Colab was asked for a GPU". Rotation tries
	// the least recently attempted account so one bad account cannot absorb
	// every retry while others might still get a T4.
	LastAttempt *time.Time `json:"last_attempt,omitempty"`
}

type Supervisor struct {
	cfg Config
	db  *sql.DB
	mu  sync.Mutex
	// busy guards long-running start/stop commands so ticker ticks and
	// admin HTTP calls pile up instead of stacking colab rotations.
	busy atomic.Bool
	// life is the server-lifetime context: workers must outlive the HTTP
	// request that started them, but not the process.
	lifeMu sync.RWMutex
	life   context.Context
	// retryAt/failStreak throttle the global ticker while Colab reports
	// capacity errors; admin Rotate/Start bypass it on purpose.
	retryAt    atomic.Int64
	failStreak atomic.Int32
}

func (s *Supervisor) setLife(ctx context.Context) {
	s.lifeMu.Lock()
	s.life = ctx
	s.lifeMu.Unlock()
}

func (s *Supervisor) workerCtx() context.Context {
	s.lifeMu.RLock()
	life := s.life
	s.lifeMu.RUnlock()
	if life == nil {
		return context.Background()
	}
	return life
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
	if cfg.StartGrace <= 0 {
		cfg.StartGrace = 90 * time.Second
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

		var best *time.Time
		var activeSince *time.Time
		var lastAttempt *time.Time
		if s.db != nil {
			rows, err := s.db.QueryContext(ctx, `SELECT identity_id, state, cooldown_until, updated_at FROM account_cooldowns WHERE identity_id=? OR identity_id=? OR identity_id=?`, accountID, name, email)
			if err == nil {
				for rows.Next() {
					var rowKey, state string
					var raw any
					var updatedAt sql.NullTime
					if rows.Scan(&rowKey, &state, &raw, &updatedAt) != nil {
						continue
					}
					if t, ok := parseCooldownTime(raw); ok {
						if best == nil || t.After(*best) {
							tt := t
							best = &tt
						}
					}
					if updatedAt.Valid && (lastAttempt == nil || updatedAt.Time.After(*lastAttempt)) {
						tt := updatedAt.Time
						lastAttempt = &tt
					}
					// Only the accountID key can mean "worker alive": this
					// supervisor always records ACTIVE there, while the legacy
					// python rotator left an email-key ACTIVE row that never
					// gets cleared and would resurrect a dead account once its
					// cooldown expires.
					if state == "ACTIVE" && updatedAt.Valid && rowKey == accountID {
						tt := updatedAt.Time
						activeSince = &tt
					}
				}
				rows.Close()
			}
		}
		acc := Account{ID: accountID, Email: email, Status: "READY", LastAttempt: lastAttempt}
		switch {
		case best != nil && now.Before(*best):
			acc.Status = "COOLDOWN"
			acc.CooldownUntil = best
		case activeSince != nil:
			acc.Status = "ACTIVE"
			acc.ActiveSince = activeSince
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
		// Strip Go String() monotonic suffix (" m=+123.456") written when a
		// time.Time carrying a monotonic reading was stored directly.
		if i := strings.Index(s, " m="); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		candidates := []string{s}
		// Go String() form "2006-01-02 15:04:05.999 +0700 Zone": drop the
		// trailing zone name, keep date + clock + numeric offset.
		if f := strings.Fields(s); len(f) == 4 {
			candidates = append(candidates, f[0]+" "+f[1]+f[2])
		}
		for _, c := range candidates {
			for _, layout := range []string{
				"2006-01-02 15:04:05.999999999-0700",
				"2006-01-02 15:04:05-0700",
				"2006-01-02 15:04:05.999999999-07:00",
				"2006-01-02 15:04:05",
				"2006-01-02T15:04:05Z07:00",
				time.RFC3339Nano,
				time.RFC3339,
			} {
				if t, err := time.Parse(layout, c); err == nil {
					return t, true
				}
			}
		}
		// Numeric-only fallback for unix-epoch encodings. ParseFloat needs a
		// full-string match, so "2026-10-13..." can't slip through as 2026.
		if f, err := strconv.ParseFloat(s, 64); err == nil {
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
	for f >= 1e12 {
		f /= 1000
	}
	secs := int64(f)
	return time.Unix(secs, int64((f-float64(secs))*1e9)), true
}

// ensureIdentity creates the parent identities row required by the
// account_cooldowns foreign key. Colab accounts live outside the identity
// manager, so without this every cooldown/ACTIVE write fails silently.
func (s *Supervisor) ensureIdentity(ctx context.Context, accountID string) {
	if s == nil || s.db == nil {
		return
	}
	email := accountIDToEmail(accountID)
	// Email may already belong to a non-colab identity (UNIQUE): INSERT OR
	// IGNORE would silently skip and leave the FK parent missing, so detect
	// the collision and fall back to a synthetic address.
	var owner string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM identities WHERE email=?`, email).Scan(&owner); err == nil && owner != "" && owner != accountID {
		email = accountID + "@colab.local"
	}
	_, _ = s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO identities(id, provider_group, email, normalized_email)
		VALUES (?, 'colab', ?, ?)`,
		accountID, email, strings.ToLower(email))
}

func accountIDToEmail(accountID string) string {
	name := strings.TrimPrefix(accountID, "colab-")
	if strings.Contains(name, "_at_") {
		parts := strings.SplitN(name, "_at_", 2)
		return parts[0] + "@" + strings.ReplaceAll(parts[1], "_", ".")
	}
	if strings.Contains(name, "@") {
		return name
	}
	return name + "@unknown.local"
}

// normalizedCooldown maps every cooldown_until encoding the Go reader accepts
// (integer/real epoch in s or ms, text-numeric epoch in s or ms, DATETIME text)
// to a comparable "YYYY-MM-DD HH:MM:SS" string, so the upsert guard can be
// atomic instead of a TOCTOU read-then-write.
const normalizedCooldown = `(CASE WHEN typeof(account_cooldowns.cooldown_until) IN ('integer','real') THEN datetime(CASE WHEN account_cooldowns.cooldown_until >= 1e12 THEN account_cooldowns.cooldown_until/1000.0 ELSE account_cooldowns.cooldown_until END,'unixepoch') WHEN typeof(account_cooldowns.cooldown_until) = 'text' AND account_cooldowns.cooldown_until NOT LIKE '%-%' THEN datetime(CAST(account_cooldowns.cooldown_until AS REAL)/(CASE WHEN CAST(account_cooldowns.cooldown_until AS REAL) >= 1e12 THEN 1000.0 ELSE 1 END),'unixepoch') ELSE account_cooldowns.cooldown_until END)`

// RecordCooldown marks an account in cooldown for duration. A shorter duration
// never overwrites a longer cooldown already stored under any key.
func (s *Supervisor) RecordCooldown(ctx context.Context, accountID string, duration time.Duration) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.ensureIdentity(ctx, accountID)
	until := time.Now().Add(duration).UTC().Truncate(time.Second).Format("2006-01-02 15:04:05")
	rawName := strings.TrimPrefix(accountID, "colab-")
	statement := `
			INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at)
			VALUES (?, 'COOLDOWN', ?, CURRENT_TIMESTAMP)
			ON CONFLICT(identity_id) DO UPDATE SET
				state='COOLDOWN',
				cooldown_until=excluded.cooldown_until,
				updated_at=CURRENT_TIMESTAMP
			WHERE account_cooldowns.cooldown_until IS NULL OR excluded.cooldown_until > ` + normalizedCooldown
	_, err := s.db.ExecContext(ctx, statement, accountID, until)
	if err != nil {
		_, err = s.db.ExecContext(ctx, statement, rawName, until)
	}
	return err
}

// RecordActive marks an account as currently ACTIVE unless a future cooldown
// is still stored under any key.
func (s *Supervisor) RecordActive(ctx context.Context, accountID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.ensureIdentity(ctx, accountID)
	rawName := strings.TrimPrefix(accountID, "colab-")
	statement := `
			INSERT INTO account_cooldowns(identity_id, state, cooldown_until, updated_at)
			VALUES (?, 'ACTIVE', NULL, CURRENT_TIMESTAMP)
			ON CONFLICT(identity_id) DO UPDATE SET
				state='ACTIVE',
				cooldown_until=NULL,
				updated_at=CURRENT_TIMESTAMP
			WHERE account_cooldowns.cooldown_until IS NULL OR ` + normalizedCooldown + ` <= CURRENT_TIMESTAMP`
	_, err := s.db.ExecContext(ctx, statement, accountID)
	if err != nil {
		_, err = s.db.ExecContext(ctx, statement, rawName)
	}
	return err
}

// triedEarlier reports whether a should be tried before b: never-attempted
// accounts first, then the least recently attempted one.
func triedEarlier(a, b *time.Time) bool {
	if a == nil {
		return b != nil
	}
	if b == nil {
		return false
	}
	return a.Before(*b)
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
		if accounts[i].Status != "READY" {
			continue
		}
		if eligible == nil || triedEarlier(accounts[i].LastAttempt, eligible.LastAttempt) {
			eligible = &accounts[i]
		}
	}
	if eligible == nil {
		return ErrNoAccountAvailable
	}

	// 1. Retire any active accounts and enter cooldown
	wctx := s.workerCtx()
	for _, acc := range accounts {
		if acc.Status == "ACTIVE" && acc.ID != eligible.ID {
			_ = s.runCommand(wctx, "stop", acc.ID)
			_ = s.RecordCooldown(wctx, acc.ID, s.cfg.Cooldown)
		}
	}

	// 2. Start eligible account (async: the worker process outlives Rotate).
	if err := s.trackStart(ctx, eligible.ID); err != nil {
		return err
	}

	return nil
}

func (s *Supervisor) StartAccount(ctx context.Context, accountID string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	if strings.TrimSpace(accountID) == "" {
		return errors.New("account id is required")
	}
	// Only an existing, READY account may start: arbitrary ids and accounts in
	// cooldown must not reach runCommand.
	accounts, err := s.DiscoverAccounts(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, acc := range accounts {
		if acc.ID != accountID {
			continue
		}
		found = true
		if acc.Status != "READY" {
			return fmt.Errorf("colab account %s is %s, not READY", accountID, acc.Status)
		}
	}
	if !found {
		return fmt.Errorf("unknown colab account %q", accountID)
	}
	if !s.tryBegin() {
		return errors.New("colab rotation already in progress")
	}
	defer s.end()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trackStart(ctx, accountID)
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
	wctx := s.workerCtx()
	if err := s.runCommand(wctx, "stop", accountID); err != nil {
		return err
	}
	return s.RecordCooldown(wctx, accountID, s.cfg.Cooldown)
}

// trackStart launches the blocking worker start in background and returns a
// fast verdict: fast failure (bad token, no GPU quota) is reported as an
// error with a cooldown, while a still-running worker past StartGrace is
// optimistically marked ACTIVE. A late exit is downgraded to cooldown so a
// dead worker does not squat the ACTIVE slot until MaxRuntime.
func (s *Supervisor) trackStart(ctx context.Context, accountID string) error {
	type result struct{ err error }
	done := make(chan result, 1)
	// The worker follows the server lifetime, not the HTTP request: a client
	// disconnect must not kill a booting worker.
	workerCtx := s.workerCtx()
	go func() {
		err := s.runCommand(workerCtx, "start", accountID)
		select {
		case done <- result{err}:
		default:
		}
		if err == nil {
			// A healthy worker never exits: the runner owns the Colab session for
			// its whole life. Exiting 0 means the VM or the wrapper ended, so the
			// ACTIVE row must not survive it — otherwise the supervisor believes a
			// worker is alive and stops bootstrapping until MaxRuntime expires.
			log.Printf("[COLAB_SUPERVISOR] worker %s start process exited cleanly; treating worker as gone", accountID)
			_ = s.RecordCooldown(context.Background(), accountID, time.Minute)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// CommandContext only killed the wrapper process; ask the stop
			// action to reap the worker it spawned.
			_ = s.runCommand(context.Background(), "stop", accountID)
		}
		log.Printf("[COLAB_SUPERVISOR] worker %s exited: %v", accountID, err)
		// Sole writer of the penalty/backoff: the select below only reports.
		_ = s.RecordCooldown(context.Background(), accountID, s.noteFailure(err))
	}()
	timer := time.NewTimer(s.cfg.StartGrace)
	defer timer.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		// Clean exit before StartGrace: the wrapper ended immediately, so this is
		// not a healthy boot. The goroutine recorded the cooldown; never claim
		// ACTIVE for a process that is already gone.
		log.Printf("[COLAB_SUPERVISOR] worker %s exited before becoming active", accountID)
		return nil
	case <-timer.C:
		s.resetBackoff()
		return s.RecordActive(workerCtx, accountID)
	case <-workerCtx.Done():
		return workerCtx.Err()
	}
}

// commandTimeout bounds one supervisor action. A start must outlive MaxRuntime
// (plus boot slack) because the wrapper runs the whole worker; stop/other
// actions are quick and keep the configured (or 5m) cap.
func (s *Supervisor) commandTimeout(action string) time.Duration {
	if action == "start" {
		return s.cfg.MaxRuntime + 30*time.Minute
	}
	if s.cfg.CommandTimeout > 0 {
		return s.cfg.CommandTimeout
	}
	return 5 * time.Minute
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
	timeout := s.commandTimeout(action)
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, parts[0], append(parts[1:], action, accountID)...)
	cmd.Env = append(os.Environ(), "COLAB_ACCOUNT_ID="+accountID, "COLAB_ACTION="+action)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if cmdCtx.Err() != nil {
			return fmt.Errorf("colab %s timed out after %v: %w", action, timeout, cmdCtx.Err())
		}
		return fmt.Errorf("colab supervisor command failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func isCapacityError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "service unavailable")
}

// penaltyFor benches a failed account on real errors and leaves a capacity
// failure immediately eligible: the account was not at fault, and the global
// retry interval already paces the next attempt (which picks another account).
func (s *Supervisor) penaltyFor(err error) time.Duration {
	if err == nil || errors.Is(err, context.Canceled) || isCapacityError(err) {
		return 0
	}
	return 30 * time.Minute
}

// capacityRetryInterval is how long the bootstrap waits after a capacity error.
// A failed assign burns no GPU quota, so the only cost is request volume:
// one attempt every two minutes spreads across the pool (each attempt moves to
// the least recently tried account) instead of backing off for half an hour and
// discovering returning capacity late.
const capacityRetryInterval = 2 * time.Minute

// noteFailure returns the account penalty and, on capacity errors, schedules the
// next global attempt.
func (s *Supervisor) noteFailure(err error) time.Duration {
	penalty := s.penaltyFor(err)
	if isCapacityError(err) {
		s.failStreak.Add(1)
		s.retryAt.Store(time.Now().Add(capacityRetryInterval).UnixNano())
	}
	return penalty
}

func (s *Supervisor) resetBackoff() {
	s.failStreak.Store(0)
	s.retryAt.Store(0)
}

// clearStaleActive drops ACTIVE rows left by a previous server process. The
// supervisor spawns workers as its children, so a restart killed them; without
// this the status endpoint and dashboard would report a live worker for up to
// MaxRuntime while none exists.
func (s *Supervisor) clearStaleActive(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE account_cooldowns
		SET state='READY', cooldown_until=NULL, updated_at=CURRENT_TIMESTAMP
		WHERE state='ACTIVE'`)
	return err
}

// Start runs a maintenance loop: bootstrap the first worker when none is
// ACTIVE, and auto-rotate when MaxRuntime expires.
func (s *Supervisor) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.setLife(ctx)
	go func() {
		// Workers are this process's children; a restart killed them, so any
		// ACTIVE row predates this run.
		if err := s.clearStaleActive(ctx); err != nil {
			log.Printf("[COLAB_SUPERVISOR] clearing stale ACTIVE rows: %v", err)
		}
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
	// Wait out the capacity backoff so the ticker stops hammering Colab;
	// admin Rotate/Start bypass this on purpose.
	if !hasActive && hasReady && time.Now().UnixNano() >= s.retryAt.Load() {
		log.Printf("[COLAB_SUPERVISOR] no active worker; bootstrapping first colab worker...")
		if err := s.Rotate(ctx); err != nil {
			log.Printf("[COLAB_SUPERVISOR] bootstrap failed: %v", err)
		}
	}
}
