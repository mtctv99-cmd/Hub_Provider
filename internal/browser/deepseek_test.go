package browser

import (
	"context"
	"encoding/base64"
	"testing"
)

func TestExtractRawTokenString(t *testing.T) {
	// 1. Raw JWT string
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.dummy.sig"
	if got := extractRawTokenString(jwt); got != jwt {
		t.Fatalf("expected %s, got %s", jwt, got)
	}

	// 2. JSON object with "value"
	jsonStr := `{"value":"eyTokenVal123","expire_at":1700000000}`
	if got := extractRawTokenString(jsonStr); got != "eyTokenVal123" {
		t.Fatalf("expected eyTokenVal123, got %s", got)
	}

	// 3. Quoted string
	quoted := `"eyQuoted123"`
	if got := extractRawTokenString(quoted); got != "eyQuoted123" {
		t.Fatalf("expected eyQuoted123, got %s", got)
	}

	// 4. Null / empty
	if got := extractRawTokenString("null"); got != "" {
		t.Fatalf("expected empty, got %s", got)
	}
}

func TestExtractEmailFromToken(t *testing.T) {
	payload := `{"email":"tester@deepseek.com","sub":"123456"}`
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	token := "header." + encoded + ".signature"

	email := extractEmailFromToken(token)
	if email != "tester@deepseek.com" {
		t.Fatalf("expected tester@deepseek.com, got %s", email)
	}
}

func TestValidateDeepSeekTokenRejectsInvalid(t *testing.T) {
	// Empty or short or invalid format
	if _, ok := validateDeepSeekToken(""); ok {
		t.Fatal("expected empty token to be rejected")
	}
	if _, ok := validateDeepSeekToken("short"); ok {
		t.Fatal("expected short token to be rejected")
	}
	// Dummy token should fail network validation with DeepSeek API
	if _, ok := validateDeepSeekToken("dummy.token.signature_that_does_not_exist_on_deepseek"); ok {
		t.Fatal("expected dummy token to fail validation against live deepseek api")
	}
}

func TestDeepSeekManagerStatusInitial(t *testing.T) {
	mgr := NewDeepSeekManager(t.TempDir(), func(ctx context.Context, accountID, email, token string) error {
		return nil
	})
	status := mgr.Status()
	if status.Running || status.Captured {
		t.Fatalf("unexpected initial status: %+v", status)
	}
}
