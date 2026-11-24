package admin

import (
	"database/sql"
	"errors"
	"os"
	"strconv"
	"testing"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func newAuthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE admin_credentials (
		id TEXT PRIMARY KEY,
		password_hash TEXT NOT NULL,
		salt TEXT NOT NULL,
		must_change_password INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		password_changed_at DATETIME
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE admin_sessions (
		token_hash TEXT PRIMARY KEY,
		expires_at INTEGER NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAdminSessionSurvivesAuthRecreationAndLogoutRevokesIt(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "persisted-session-pass")
	db := newAuthTestDB(t)
	first, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	token, err := first.Login("persisted-session-pass")
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if !afterRestart.ValidToken(token) {
		t.Fatal("admin session should remain valid after the auth service is recreated")
	}

	afterRestart.Logout(token)
	afterLogout, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if afterLogout.ValidToken(token) {
		t.Fatal("logged-out session must not remain valid in persistent storage")
	}
	var storedToken string
	if err := db.QueryRow(`SELECT token_hash FROM admin_sessions LIMIT 1`).Scan(&storedToken); err == nil {
		t.Fatal("raw session tokens must not be stored in SQLite")
	}
}

func TestAdminAuthFailsClosedWithoutStoredCredentialOrEnv(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	db := newAuthTestDB(t)
	if _, err := NewFromEnvWithDB(db); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}

func TestUsesDefaultPasswordDetectsShippedDefault(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	db := newAuthTestDB(t)
	hash, err := bcrypt.GenerateFromPassword([]byte(DefaultPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO admin_credentials (id, password_hash, salt, must_change_password) VALUES (?, ?, ?, 1)`, adminCredentialID, string(hash), "salt"); err != nil {
		t.Fatal(err)
	}
	auth, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.UsesDefaultPassword() {
		t.Fatal("stored default password must be detected")
	}
	if err := auth.ChangePassword(DefaultPassword, "new-secret"); err != nil {
		t.Fatal(err)
	}
	if auth.UsesDefaultPassword() {
		t.Fatal("changed password must not be flagged as default")
	}
}

func TestApplyEnvPasswordReplacesDefaultAndForcesChange(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	db := newAuthTestDB(t)
	hash, err := bcrypt.GenerateFromPassword([]byte(DefaultPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO admin_credentials (id, password_hash, salt, must_change_password) VALUES (?, ?, ?, 0)`, adminCredentialID, string(hash), "salt"); err != nil {
		t.Fatal(err)
	}
	auth, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	oldToken, err := auth.Login(DefaultPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.ApplyEnvPassword("env-bootstrap-pass"); err != nil {
		t.Fatal(err)
	}
	if auth.UsesDefaultPassword() {
		t.Fatal("default password must no longer authenticate after bootstrap")
	}
	if !auth.FirstLoginPending() {
		t.Fatal("bootstrap must force a password change")
	}
	if auth.ValidToken(oldToken) {
		t.Fatal("sessions issued with the default password must be dropped")
	}
	if _, err := auth.Login("env-bootstrap-pass"); err != nil {
		t.Fatalf("env password should authenticate: %v", err)
	}
}

func TestAdminPasswordChangeAndDismiss(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "bootstrap-pass")
	db := newAuthTestDB(t)
	auth, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.ChangePassword("bootstrap-pass", "new-secret"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if auth.FirstLoginPending() {
		t.Fatal("password change should clear first-login state")
	}
	if _, err := auth.Login("bootstrap-pass"); err == nil {
		t.Fatal("old password should fail after change")
	}
	if _, err := auth.Login("new-secret"); err != nil {
		t.Fatalf("new password should authenticate: %v", err)
	}

	db2 := newAuthTestDB(t)
	auth2, err := NewFromEnvWithDB(db2)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth2.DismissFirstLoginPrompt(); err != nil {
		t.Fatalf("dismiss prompt: %v", err)
	}
	if auth2.FirstLoginPending() {
		t.Fatal("dismiss should clear first-login state")
	}
	if _, err := auth2.Login("bootstrap-pass"); err != nil {
		t.Fatalf("dismiss must not change password: %v", err)
	}
}

func TestEnvPasswordBootstrapsOnlyOnce(t *testing.T) {
	db := newAuthTestDB(t)
	if err := os.Setenv("ADMIN_PASSWORD", "env-secret"); err != nil {
		t.Fatal(err)
	}
	auth, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Login("env-secret"); err != nil {
		t.Fatalf("env password should bootstrap: %v", err)
	}
	if err := os.Setenv("ADMIN_PASSWORD", "different-secret"); err != nil {
		t.Fatal(err)
	}
	auth2, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth2.Login("env-secret"); err != nil {
		t.Fatalf("saved password should win after restart: %v", err)
	}
	if _, err := auth2.Login("different-secret"); err == nil {
		t.Fatal("environment must not overwrite saved password")
	}
}

func TestLoginLimiterLocksAfterRepeatedFailures(t *testing.T) {
	limiter := NewLoginLimiter()
	key := "203.0.113.10"
	for i := 0; i < LoginMaxFailures-1; i++ {
		limiter.RecordFailure(key)
		if wait := limiter.BlockedFor(key); wait != 0 {
			t.Fatalf("locked too early after %d failures: %v", i+1, wait)
		}
	}
	limiter.RecordFailure(key)
	if wait := limiter.BlockedFor(key); wait <= 0 {
		t.Fatal("expected lockout after reaching failure limit")
	}
	if wait := limiter.BlockedFor("198.51.100.7"); wait != 0 {
		t.Fatal("another remote IP must not inherit the lockout")
	}
	limiter.RecordSuccess(key)
	if wait := limiter.BlockedFor(key); wait != 0 {
		t.Fatal("successful login must clear the failure state")
	}
}

func TestLoginLimiterStateIsBounded(t *testing.T) {
	limiter := NewLoginLimiter()
	for i := 0; i < loginLimiterMaxEntries+100; i++ {
		limiter.RecordFailure("10.0." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256))
	}
	if len(limiter.entries) > loginLimiterMaxEntries {
		t.Fatalf("limiter map grew to %d entries", len(limiter.entries))
	}
}

func TestAdminPasswordPolicyAppliesToFreshAndEnvBootstrap(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "short")
	if _, err := NewFromEnvWithDB(newAuthTestDB(t)); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("fresh bootstrap with short password: got %v, want ErrPasswordPolicy", err)
	}
	if NewFromEnv().Configured() {
		t.Fatal("NewFromEnv with a short password must fail closed")
	}

	t.Setenv("ADMIN_PASSWORD", "")
	db := newAuthTestDB(t)
	hash, err := bcrypt.GenerateFromPassword([]byte(DefaultPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO admin_credentials (id, password_hash, salt, must_change_password) VALUES (?, ?, ?, 0)`, adminCredentialID, string(hash), "salt"); err != nil {
		t.Fatal(err)
	}
	auth, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.ApplyEnvPassword("short"); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("ApplyEnvPassword short: got %v, want ErrPasswordPolicy", err)
	}
	if err := auth.ChangePassword(DefaultPassword, "short"); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("ChangePassword short: got %v, want ErrPasswordPolicy", err)
	}
}

func TestStoredShortPasswordHashStillAuthenticates(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	db := newAuthTestDB(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("abc"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO admin_credentials (id, password_hash, salt, must_change_password) VALUES (?, ?, ?, 0)`, adminCredentialID, string(hash), "salt"); err != nil {
		t.Fatal(err)
	}
	auth, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Login("abc"); err != nil {
		t.Fatalf("existing stored hash must keep working: %v", err)
	}
}
