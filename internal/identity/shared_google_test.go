package identity

import (
	"context"
	"path/filepath"
	"testing"

	"sever_provider/internal/db"
	"sever_provider/internal/secret"
)

func TestGoogleIdentitySharesGeminiAndColabCredentials(t *testing.T) {
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
	if err := manager.UpsertIdentity(ctx, Identity{ID: "google-1", ProviderGroup: "google", Email: " User@Example.COM "}); err != nil {
		t.Fatal(err)
	}
	if err := manager.UpsertCredential(ctx, Credential{ID: "gemini-1", IdentityID: "google-1", Domain: "gemini_web", MaxConcurrency: 4}, []byte(`{"cookie":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := manager.UpsertCredential(ctx, Credential{ID: "colab-1", IdentityID: "google-1", Domain: "google_colab", MaxConcurrency: 8}, []byte(`{"refresh_token":"y"}`)); err != nil {
		t.Fatal(err)
	}
	detail, err := manager.GetIdentityDetail(ctx, "google-1")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Identity.Email != "User@Example.COM" {
		t.Fatalf("email=%q", detail.Identity.Email)
	}
	if len(detail.Credentials) != 2 {
		t.Fatalf("credentials=%d", len(detail.Credentials))
	}
	if detail.Identity.GlobalStatus != IdentityHealthy {
		t.Fatalf("status=%s", detail.Identity.GlobalStatus)
	}
	byEmail, err := manager.FindIdentityByEmail(ctx, " user@example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if byEmail.ID != "google-1" {
		t.Fatalf("found=%s", byEmail.ID)
	}
}
