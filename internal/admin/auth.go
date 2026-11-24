package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const CookieName = "sever_provider_admin_session"
const DefaultPassword = "123456"
const adminCredentialID = "local-admin"

// MinPasswordLength is the minimum length for any newly configured admin
// password (environment bootstrap, change-password, and ApplyEnvPassword).
// Existing stored hashes are never rejected because of this policy.
const MinPasswordLength = 8

const (
	// LoginMaxFailures is the number of failed attempts per key (remote IP)
	// before a lockout is applied.
	LoginMaxFailures = 5
	// LoginFailureWindow is how long failed attempts are counted.
	LoginFailureWindow = 15 * time.Minute
	// LoginLockoutDuration is the base lockout. Repeated lockouts double up to
	// LoginMaxLockoutDuration.
	LoginLockoutDuration = 5 * time.Minute
	// LoginMaxLockoutDuration caps progressive lockouts.
	LoginMaxLockoutDuration = time.Hour
	// loginLimiterMaxEntries bounds memory if many distinct remote IPs fail.
	loginLimiterMaxEntries = 4096
	// loginLimiterStaleAfter lets entries with no recent activity be pruned.
	loginLimiterStaleAfter = 2 * time.Hour
)

var ErrNotConfigured = errors.New("admin authentication is not configured")
var ErrInvalidPassword = errors.New("invalid admin password")
var ErrPasswordPolicy = errors.New("password does not meet policy (minimum 8 characters)")

func validatePassword(password string) error {
	length := len([]rune(password))
	if length < MinPasswordLength || length > 256 {
		return ErrPasswordPolicy
	}
	return nil
}

// Auth manages admin sessions and credential verifiers.
// Password hashes and first-login state are persisted in SQLite when a database handle is supplied.
type Auth struct {
	mu           sync.RWMutex
	passwordHash string
	db           *sql.DB
	firstLogin   bool
	configured   bool
	tokens       map[string]time.Time
	ttl          time.Duration
}

// NewFromEnv keeps the lightweight constructor used by tests.
// Without ADMIN_PASSWORD the returned Auth is not configured, so callers fail closed.
func NewFromEnv() *Auth {
	return newAuth(nil, strings.TrimSpace(os.Getenv("ADMIN_PASSWORD")))
}

// NewFromEnvWithDB initializes or reads the singleton admin credential in SQLite.
// An existing database record always preserves the user's saved password across restarts.
// Fail-closed only when there is no stored credential AND ADMIN_PASSWORD is empty.
func NewFromEnvWithDB(db *sql.DB) (*Auth, error) {
	envPassword := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))
	if db == nil {
		return newAuth(nil, envPassword), nil
	}
	a := &Auth{db: db, tokens: make(map[string]time.Time), ttl: 24 * time.Hour}
	var storedHash string
	var pending int
	err := db.QueryRow(`SELECT password_hash, must_change_password FROM admin_credentials WHERE id = ?`, adminCredentialID).Scan(&storedHash, &pending)
	if err == nil {
		a.passwordHash = storedHash
		a.firstLogin = pending != 0
		a.configured = storedHash != ""
		return a, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if envPassword == "" {
		return nil, ErrNotConfigured
	}
	if err := validatePassword(envPassword); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(envPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return nil, err
	}
	// Fresh install with an explicit password: no forced change, the operator
	// already chose it.
	if _, err := db.Exec(`INSERT INTO admin_credentials (id, password_hash, salt, must_change_password) VALUES (?, ?, ?, 0)`,
		adminCredentialID, string(hash), hex.EncodeToString(saltBytes)); err != nil {
		return nil, err
	}
	a.passwordHash = string(hash)
	a.configured = true
	return a, nil
}

func newAuth(db *sql.DB, password string) *Auth {
	a := &Auth{db: db, tokens: make(map[string]time.Time), ttl: 24 * time.Hour}
	if password == "" {
		// No fallback default: an unconfigured Auth rejects every login.
		return a
	}
	if validatePassword(password) != nil {
		// Weak environment passwords must fail closed instead of silently
		// creating an admin account with a short password.
		return a
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return a
	}
	a.passwordHash = string(hash)
	a.configured = true
	return a
}

// UsesDefaultPassword reports whether the stored hash still accepts DefaultPassword.
func (a *Auth) UsesDefaultPassword() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	hash := a.passwordHash
	a.mu.RUnlock()
	return hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(DefaultPassword)) == nil
}

// ApplyEnvPassword replaces a stored default-password hash with the ADMIN_PASSWORD
// value, forces a change on the next login, and drops sessions that were issued
// with the default password.
func (a *Auth) ApplyEnvPassword(password string) error {
	password = strings.TrimSpace(password)
	if a == nil || a.db == nil || password == "" {
		return ErrNotConfigured
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE admin_credentials SET password_hash = ?, must_change_password = 1, updated_at = CURRENT_TIMESTAMP, password_changed_at = CURRENT_TIMESTAMP WHERE id = ?`, string(hash), adminCredentialID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM admin_sessions`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.mu.Lock()
	a.passwordHash = string(hash)
	a.firstLogin = true
	a.configured = true
	a.tokens = make(map[string]time.Time)
	a.mu.Unlock()
	return nil
}

func (a *Auth) Configured() bool { return a != nil && a.configured && a.passwordHash != "" }

func (a *Auth) FirstLoginPending() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.firstLogin
}

func (a *Auth) Login(password string) (string, error) {
	if !a.Configured() {
		return "", ErrNotConfigured
	}
	a.mu.RLock()
	hash := a.passwordHash
	a.mu.RUnlock()
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", ErrInvalidPassword
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	expiresAt := time.Now().Add(a.ttl)
	if a.db != nil {
		if _, err := a.db.Exec(`DELETE FROM admin_sessions WHERE expires_at <= ?`, time.Now().Unix()); err != nil {
			return "", err
		}
		if _, err := a.db.Exec(`INSERT INTO admin_sessions(token_hash, expires_at) VALUES(?, ?)`, hashToken(token), expiresAt.Unix()); err != nil {
			return "", err
		}
	}
	a.mu.Lock()
	a.tokens[token] = expiresAt
	a.mu.Unlock()
	return token, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *Auth) ChangePassword(current, next string) error {
	if !a.Configured() {
		return ErrNotConfigured
	}
	if err := validatePassword(next); err != nil {
		return err
	}
	a.mu.RLock()
	oldHash := a.passwordHash
	a.mu.RUnlock()
	if bcrypt.CompareHashAndPassword([]byte(oldHash), []byte(current)) != nil {
		return ErrInvalidPassword
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if a.db != nil {
		if _, err := a.db.Exec(`UPDATE admin_credentials SET password_hash = ?, must_change_password = 0, updated_at = CURRENT_TIMESTAMP, password_changed_at = CURRENT_TIMESTAMP WHERE id = ?`, string(newHash), adminCredentialID); err != nil {
			return err
		}
		if _, err := a.db.Exec(`DELETE FROM admin_sessions`); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.passwordHash = string(newHash)
	a.firstLogin = false
	a.tokens = make(map[string]time.Time)
	a.mu.Unlock()
	return nil
}

func (a *Auth) DismissFirstLoginPrompt() error {
	if !a.Configured() {
		return ErrNotConfigured
	}
	if a.db != nil {
		if _, err := a.db.Exec(`UPDATE admin_credentials SET must_change_password = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, adminCredentialID); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.firstLogin = false
	a.mu.Unlock()
	return nil
}

func (a *Auth) ValidToken(token string) bool {
	if a == nil || token == "" {
		return false
	}
	now := time.Now()
	a.mu.Lock()
	expires, ok := a.tokens[token]
	if ok && now.Before(expires) {
		a.mu.Unlock()
		return true
	}
	if ok {
		delete(a.tokens, token)
	}
	a.mu.Unlock()
	if a.db == nil {
		return false
	}
	var valid int
	return a.db.QueryRow(`SELECT COUNT(*) FROM admin_sessions WHERE token_hash=? AND expires_at > ?`, hashToken(token), now.Unix()).Scan(&valid) == nil && valid > 0
}

func (a *Auth) Logout(token string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.tokens, token)
	a.mu.Unlock()
	if a.db != nil && token != "" {
		_, _ = a.db.Exec(`DELETE FROM admin_sessions WHERE token_hash=?`, hashToken(token))
	}
}

func (a *Auth) TokenFromRequest(r *http.Request) string {
	if cookie, err := r.Cookie(CookieName); err == nil && a.ValidToken(cookie.Value) {
		return cookie.Value
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		token := strings.TrimSpace(auth[7:])
		if a.ValidToken(token) {
			return token
		}
	}
	return ""
}

func (a *Auth) IsAuthenticated(r *http.Request) bool { return a != nil && a.TokenFromRequest(r) != "" }

// LoginLimiter applies per-IP failed-login throttling and lockout. The state
// map is bounded and pruned on access so unauthenticated traffic cannot grow it
// without limit.
type LoginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginLimiterEntry
	now     func() time.Time
}

type loginLimiterEntry struct {
	failures     int
	firstFailure time.Time
	lastFailure  time.Time
	blockedUntil time.Time
	lockouts     int
}

func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{entries: make(map[string]*loginLimiterEntry), now: time.Now}
}

func (l *LoginLimiter) currentTime() time.Time {
	if l != nil && l.now != nil {
		return l.now()
	}
	return time.Now()
}

// BlockedFor returns how long the key is still locked out. Zero means allowed.
func (l *LoginLimiter) BlockedFor(key string) time.Duration {
	if l == nil {
		return 0
	}
	now := l.currentTime()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	entry, ok := l.entries[key]
	if !ok {
		return 0
	}
	if now.Before(entry.blockedUntil) {
		return entry.blockedUntil.Sub(now)
	}
	if !entry.blockedUntil.IsZero() {
		// The lockout expired; start a fresh failure window.
		entry.blockedUntil = time.Time{}
		entry.failures = 0
		entry.firstFailure = time.Time{}
	}
	return 0
}

// RecordFailure counts one failed attempt. Once LoginMaxFailures are reached
// inside LoginFailureWindow the key is locked with progressive backoff.
func (l *LoginLimiter) RecordFailure(key string) {
	if l == nil {
		return
	}
	now := l.currentTime()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	entry, ok := l.entries[key]
	if !ok {
		entry = &loginLimiterEntry{}
		l.entries[key] = entry
	}
	if !entry.blockedUntil.IsZero() {
		if now.Before(entry.blockedUntil) {
			return
		}
		entry.blockedUntil = time.Time{}
		entry.failures = 0
		entry.firstFailure = time.Time{}
	}
	if entry.firstFailure.IsZero() || now.Sub(entry.firstFailure) > LoginFailureWindow {
		entry.failures = 0
		entry.firstFailure = now
	}
	entry.failures++
	entry.lastFailure = now
	if entry.failures >= LoginMaxFailures {
		entry.lockouts++
		lockout := LoginLockoutDuration
		for i := 1; i < entry.lockouts && lockout < LoginMaxLockoutDuration; i++ {
			lockout *= 2
		}
		if lockout > LoginMaxLockoutDuration {
			lockout = LoginMaxLockoutDuration
		}
		entry.blockedUntil = now.Add(lockout)
		entry.failures = 0
		entry.firstFailure = now
	}
}

// RecordSuccess clears the failure state for a key after a valid login.
func (l *LoginLimiter) RecordSuccess(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// pruneLocked removes stale entries and keeps the map bounded. Callers must hold l.mu.
func (l *LoginLimiter) pruneLocked(now time.Time) {
	for key, entry := range l.entries {
		if now.Before(entry.blockedUntil) {
			continue
		}
		last := entry.lastFailure
		if entry.firstFailure.After(last) {
			last = entry.firstFailure
		}
		if last.IsZero() || now.Sub(last) > loginLimiterStaleAfter {
			delete(l.entries, key)
		}
	}
	if len(l.entries) <= loginLimiterMaxEntries {
		return
	}
	// The map is still too large after pruning (for example a distributed
	// attack). Dropping state is safer than unbounded memory; legitimate users
	// simply get a fresh failure budget.
	l.entries = make(map[string]*loginLimiterEntry)
}
