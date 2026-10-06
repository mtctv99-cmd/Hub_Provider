package identity

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"sever_provider/internal/db"
	"sever_provider/internal/secret"

	"modernc.org/sqlite"
)

// countingDriver counts SELECTs on identities and credentials without changing SQLite behavior.
type countingDriver struct {
	driver.Driver
	selects atomic.Int64
}

type countingConn struct {
	driver.Conn
	selects *atomic.Int64
}

func (d *countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, selects: &d.selects}, nil
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(strings.TrimSpace(query), "SELECT ") {
		c.selects.Add(1)
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestListIdentityDetailsBatchesCredentialsAndPreservesDetails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	database, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := secret.NewStore("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	writer := NewManager(database.DB, store)
	for _, item := range []Identity{
		{ID: "z", ProviderGroup: "google", Email: "z@example.com", GlobalStatus: IdentityDisabled},
		{ID: "a", ProviderGroup: "google", Email: "A@example.com", GlobalStatus: IdentityHealthy},
		{ID: "m", ProviderGroup: "other", Email: "m@example.com"},
		{ID: "e", ProviderGroup: "google", Email: "e@example.com"},
	} {
		if err := writer.UpsertIdentity(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []Credential{
		{ID: "z1", IdentityID: "z", Domain: "gemini_web", Status: StatusDisabled},
		{ID: "a1", IdentityID: "a", Domain: "gemini_web", Status: StatusDegraded, SourceRef: "reference"},
		{ID: "a2", IdentityID: "a", Domain: "google_colab", Status: StatusHealthy, PoolRole: PoolWorker},
		{ID: "m1", IdentityID: "m", Domain: "gemini_web", Status: StatusCooldown},
	} {
		if err := writer.UpsertCredential(ctx, item, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Count only the reads under test, using the same on-disk database and schema.
	counter := &countingDriver{Driver: &sqlite.Driver{}}
	readerDB := sql.OpenDB(&countingConnector{driver: counter, dsn: "file:" + filepath.ToSlash(path)})
	defer readerDB.Close()
	readerDB.SetMaxOpenConns(1)
	reader := NewManager(readerDB, store)
	for _, group := range []string{"google", ""} {
		counter.selects.Store(0)
		got, err := reader.ListIdentityDetails(ctx, group)
		if err != nil {
			t.Fatal(err)
		}
		if selects := counter.selects.Load(); selects != 2 {
			t.Fatalf("group %q: got %d SELECTs, want 2", group, selects)
		}
		wantIDs := []string{"a", "e", "z"}
		if group == "" {
			wantIDs = []string{"a", "e", "m", "z"}
		}
		ids := make([]string, 0, len(got))
		statuses := map[string]IdentityStatus{"a": IdentityPartial, "e": IdentityUnconfigured, "m": IdentityDegraded, "z": IdentityDisabled}
		for _, detail := range got {
			ids = append(ids, detail.Identity.ID)
			if detail.Identity.GlobalStatus != statuses[detail.Identity.ID] {
				t.Fatalf("identity %q: status = %s, want %s", detail.Identity.ID, detail.Identity.GlobalStatus, statuses[detail.Identity.ID])
			}
			want, err := writer.GetIdentityDetail(ctx, detail.Identity.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(detail, want) {
				t.Fatalf("group %q, identity %q: got %+v, want %+v", group, detail.Identity.ID, detail, want)
			}
		}
		if !reflect.DeepEqual(ids, wantIDs) {
			t.Fatalf("group %q: ids = %v, want %v", group, ids, wantIDs)
		}
	}
	for _, id := range []string{"a", "e", "m", "z"} {
		if err := writer.DeleteIdentity(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	counter.selects.Store(0)
	empty, err := reader.ListIdentityDetails(ctx, "")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list: got %v, err %v", empty, err)
	}
}

type countingConnector struct {
	driver *countingDriver
	dsn    string
}

func (c *countingConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}
func (c *countingConnector) Driver() driver.Driver { return c.driver }

func TestDeleteIdentityCascadesCredentialsAndSecrets(t *testing.T) {
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
	account, err := manager.UpsertGoogleAccount(context.Background(), "delete-me@example.com", 1, []GoogleCredentialSpec{{Domain: "gemini_web", Secret: []byte(`{"cookie":"__Secure-1PSID=x; SAPISID=y"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteIdentity(context.Background(), account.Identity.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GetIdentityDetail(context.Background(), account.Identity.ID); err == nil {
		t.Fatal("expected identity to be deleted")
	}
	var count int
	if err := database.DB.QueryRow(`SELECT COUNT(*) FROM credential_secrets`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("credential secrets remaining: %d", count)
	}
}

func TestCredentialSecretRoundTrip(t *testing.T) {
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
	identity := Identity{ID: "google-1", ProviderGroup: "google", Email: "account@example.com", Priority: 10, GlobalStatus: "active"}
	if err := manager.UpsertIdentity(ctx, identity); err != nil {
		t.Fatal(err)
	}
	credential := Credential{ID: "gemini-1", IdentityID: identity.ID, Domain: "gemini_web", MaxConcurrency: 4}
	if err := manager.UpsertCredential(ctx, credential, []byte(`{"cookie":"redacted"}`)); err != nil {
		t.Fatal(err)
	}

	got, err := manager.ReadSecret(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"cookie":"redacted"}` {
		t.Fatalf("unexpected secret: %s", got)
	}
	list, err := manager.List(ctx, "gemini_web")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].MaxConcurrency != 4 {
		t.Fatalf("unexpected credential list: %+v", list)
	}
}

func TestUpsertGoogleSecretNotOverwrittenWhenUndecryptable(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	keyA := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	storeA, err := secret.NewStore(keyA)
	if err != nil {
		t.Fatal(err)
	}
	managerA := NewManager(database.DB, storeA)
	account, err := managerA.UpsertGoogleAccount(context.Background(), "secret@example.com", 1, []GoogleCredentialSpec{{Domain: "gemini_web", Secret: []byte(`{"cookie":"original"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	credID := account.Credentials[0].ID
	keyB := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	storeB, err := secret.NewStore(keyB)
	if err != nil {
		t.Fatal(err)
	}
	managerB := NewManager(database.DB, storeB)
	if _, err := managerB.UpsertGoogleAccount(context.Background(), "secret@example.com", 1, []GoogleCredentialSpec{{ID: credID, Domain: "gemini_web", Status: StatusAuthRequired, Secret: []byte(`{"cookie":"new"}`)}}); err != nil {
		t.Fatal(err)
	}
	// Old secret must remain readable with keyA (new write must not clobber it).
	if secret, err := managerA.ReadSecret(context.Background(), credID); err != nil || string(secret) != `{"cookie":"original"}` {
		t.Fatalf("original secret clobbered or unreadable: %q err=%v", string(secret), err)
	}
	// Credential status should reflect the caller-supplied Status, not forced HEALTHY.
	creds, err := managerB.List(context.Background(), "gemini_web")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].Status != StatusAuthRequired {
		t.Fatalf("expected caller-supplied status, got %+v", creds)
	}
}
