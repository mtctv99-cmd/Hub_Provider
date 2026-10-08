package identity

import (
	"context"
	"path/filepath"
	"testing"

	"sever_provider/internal/db"
	"sever_provider/internal/secret"
)

func TestUpsertMetaAccount(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	store, err := secret.NewStore("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(database.DB, store)

	ctx := context.Background()
	detail, err := mgr.UpsertMetaAccount(ctx, MetaCredentialSpec{
		Email:          "user@meta.ai",
		MaxConcurrency: 2,
		Secret:         []byte(`{"cookie":"datr=123; c_user=456; xs=789"}`),
	})
	if err != nil {
		t.Fatalf("UpsertMetaAccount failed: %v", err)
	}

	if detail.Identity.ProviderGroup != "meta" {
		t.Fatalf("expected provider_group meta, got %s", detail.Identity.ProviderGroup)
	}
	if len(detail.Credentials) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(detail.Credentials))
	}
	if detail.Credentials[0].Domain != DomainMetaWeb {
		t.Fatalf("expected domain %s, got %s", DomainMetaWeb, detail.Credentials[0].Domain)
	}

	// Secret can be decrypted
	secretBytes, err := mgr.ReadSecret(ctx, detail.Credentials[0].ID)
	if err != nil {
		t.Fatalf("ReadSecret failed: %v", err)
	}
	if string(secretBytes) != `{"cookie":"datr=123; c_user=456; xs=789"}` {
		t.Fatalf("unexpected secret: %s", string(secretBytes))
	}
}
