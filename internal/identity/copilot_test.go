package identity

import (
	"context"
	"path/filepath"
	"testing"

	"sever_provider/internal/db"
	"sever_provider/internal/secret"
)

func TestUpsertCopilotAccount(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := secret.NewStore("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(database.DB, store)

	ctx := context.Background()
	spec := CopilotCredentialSpec{
		Email:          "copilot-user@example.com",
		Priority:       2,
		MaxConcurrency: 3,
		PoolRole:       PoolSession,
		Secret:         []byte(`{"token_url":"wss://substrate.office.com/m365Copilot/Chathub/11111111-2222-3333-4444-555555555555@66666666-7777-8888-9999-000000000000?access_token=dummy"}`),
	}

	detail, err := manager.UpsertCopilotAccount(ctx, spec)
	if err != nil {
		t.Fatalf("failed to upsert Copilot account: %v", err)
	}
	if detail.Identity.Email != spec.Email || detail.Identity.ProviderGroup != "copilot" {
		t.Fatalf("unexpected identity: %+v", detail.Identity)
	}
	if len(detail.Credentials) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(detail.Credentials))
	}
	cred := detail.Credentials[0]
	if cred.Domain != "copilot_ws" || cred.Status != StatusDegraded || cred.MaxConcurrency != 3 || cred.PoolRole != PoolSession {
		t.Fatalf("unexpected credential state: %+v", cred)
	}

	// Verify secret was encrypted and is decodable
	decrypted, err := manager.ReadSecret(ctx, cred.ID)
	if err != nil {
		t.Fatalf("failed to read secret: %v", err)
	}
	if string(decrypted) != string(spec.Secret) {
		t.Fatalf("decrypted secret mismatch: got %s", string(decrypted))
	}

	// Verify conflict with google group
	_, err = manager.UpsertGoogleAccount(ctx, "google-only@example.com", 1, []GoogleCredentialSpec{
		{Domain: "gemini_web", Secret: []byte("s")},
	})
	if err != nil {
		t.Fatal(err)
	}

	conflictSpec := spec
	conflictSpec.Email = "google-only@example.com"
	conflictSpec.IdentityID = ""
	_, err = manager.UpsertCopilotAccount(ctx, conflictSpec)
	if err == nil {
		t.Fatal("expected conflict error when Copilot account collides with google identity")
	}
}
