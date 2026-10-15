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

var ErrNotConfigured = errors.New("admin authentication is not configured")
var ErrInvalidPassword = errors.New("invalid admin password")
var ErrPasswordPolicy = errors.New("password does not meet policy (minimum 6 characters)")

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
// If ADMIN_PASSWORD is set, it uses that; otherwise it uses DefaultPassword ("123456").
func NewFromEnv() *Auth {
	return newAuth(nil, strings.TrimSpace(os.Getenv("ADMIN_PASSWORD")))
}

// NewFromEnvWithDB initializes or reads the singleton admin credential in SQLite.
// An existing database record always preserves the user's saved password across restarts.
func NewFromEnvWithDB(db *sql.DB) (*Auth, error) {
	if db == nil {
		return newAuth(nil, strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))), nil
	}
	configured := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))
	if configured == "" {
		configured = DefaultPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(configured), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return nil, err
	}
	// Insert default or env password if table is empty
	_, err = db.Exec(`INSERT OR IGNORE INTO admin_credentials (id, password_hash, salt, must_change_password) VALUES (?, ?, ?, 1)`,
		adminCredentialID, string(hash), hex.EncodeToString(saltBytes))
	if err != nil {
		return nil, err
	}

	var storedHash string
	var pending int
	if err := db.QueryRow(`SELECT password_hash, must_change_password FROM admin_credentials WHERE id = ?`, adminCredentialID).Scan(&storedHash, &pending); err != nil {
		return nil, err
	}
	return &Auth{
		passwordHash: storedHash,
		db:           db,
		firstLogin:   pending != 0,
		configured:   true,
		tokens:       make(map[string]time.Time),
		ttl:          24 * time.Hour,
	}, nil
}

func newAuth(db *sql.DB, password string) *Auth {
	if password == "" {
		password = DefaultPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return &Auth{db: db, tokens: make(map[string]time.Time), ttl: 24 * time.Hour}
	}
	return &Auth{
		passwordHash: string(hash),
		db:           db,
		firstLogin:   true,
		configured:   true,
		tokens:       make(map[string]time.Time),
		ttl:          24 * time.Hour,
	}
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
	if len([]rune(next)) < 6 || len([]rune(next)) > 256 {
		return ErrPasswordPolicy
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
