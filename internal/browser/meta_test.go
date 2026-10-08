package browser

import (
	"context"
	"testing"
)

func TestValidMetaProfileID(t *testing.T) {
	valid := []string{"meta-abc123", "account_1", "user@test.com"}
	for _, id := range valid {
		if !validMetaProfileID(id) {
			t.Errorf("expected valid profile id %q", id)
		}
	}
	invalid := []string{"../escape", "has/slash", "bad id", ""}
	for _, id := range invalid {
		if validMetaProfileID(id) {
			t.Errorf("expected invalid profile id %q", id)
		}
	}
}

func TestMetaManagerInvalidAccountID(t *testing.T) {
	mgr := NewMetaManager(t.TempDir(), nil)
	for _, id := range []string{"../../etc/passwd", "bad/slashes", "bad id with spaces"} {
		if _, err := mgr.Start(id); err == nil {
			t.Fatalf("expected error for account ID %q", id)
		}
	}
}

func TestMetaManagerStatusInitial(t *testing.T) {
	mgr := NewMetaManager(t.TempDir(), func(ctx context.Context, accountID, email, cookieStr, tokenStr string) error {
		return nil
	})
	status := mgr.Status()
	if status.Running || status.Captured {
		t.Fatalf("unexpected initial status: %+v", status)
	}
}

func TestMetaManagerStartInvalidURL(t *testing.T) {
	mgr := NewMetaManager(t.TempDir(), nil)
	if _, err := mgr.StartWithURL("meta-abc", "https://evil.com/phish"); err == nil {
		t.Fatal("expected error for unsupported Meta login URL")
	}
}

func TestMetaManagerStopIdempotent(t *testing.T) {
	mgr := NewMetaManager(t.TempDir(), nil)
	mgr.Stop()
	status := mgr.Status()
	if status.Running {
		t.Fatal("expected status not running after Stop()")
	}
}
