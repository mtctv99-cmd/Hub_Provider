package admin

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newAuthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
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
	return db
}

func TestDefaultAdminPasswordAndFirstLogin(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	db := newAuthTestDB(t)
	auth, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.Configured() || !auth.FirstLoginPending() {
		t.Fatalf("expected configured first-login auth")
	}
	if _, err := auth.Login(DefaultPassword); err != nil {
		t.Fatalf("default password should authenticate: %v", err)
	}
	var hash string
	if err := db.QueryRow(`SELECT password_hash FROM admin_credentials WHERE id = ?`, adminCredentialID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, DefaultPassword) {
		t.Fatal("database must not contain the plaintext default password")
	}
}

func TestAdminPasswordChangeAndDismiss(t *testing.T) {
	db := newAuthTestDB(t)
	auth, err := NewFromEnvWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.ChangePassword(DefaultPassword, "new-secret"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if auth.FirstLoginPending() {
		t.Fatal("password change should clear first-login state")
	}
	if _, err := auth.Login(DefaultPassword); err == nil {
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
	if _, err := auth2.Login(DefaultPassword); err != nil {
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
