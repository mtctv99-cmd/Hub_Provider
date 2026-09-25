package identity

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"sever_provider/internal/secret"
)

var ErrMissingCredentialSecret = fmt.Errorf("credential secret is missing")

type Manager struct {
	db          *sql.DB
	secretStore *secret.Store
}

func NewManager(db *sql.DB, store *secret.Store) *Manager {
	return &Manager{db: db, secretStore: store}
}

type GoogleCredentialSpec struct {
	Domain         string
	ID             string
	SourceRef      string
	Fingerprint    string
	PoolRole       PoolRole
	MaxConcurrency int
	Secret         []byte
}

// UpsertGoogleAccount reuses an identity by normalized email and attaches
// provider-specific encrypted credentials without duplicating accounts.
func (m *Manager) UpsertGoogleAccount(ctx context.Context, email string, priority int, specs []GoogleCredentialSpec) (IdentityDetail, error) {
	email = strings.TrimSpace(email)
	if normalizeEmail(email) == "" {
		return IdentityDetail{}, fmt.Errorf("google email is required")
	}
	if len(specs) == 0 {
		return IdentityDetail{}, fmt.Errorf("at least one credential specification is required")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return IdentityDetail{}, err
	}
	defer tx.Rollback()

	var identity Identity
	err = tx.QueryRowContext(ctx, `SELECT id, provider_group, email, priority, global_status, created_at, updated_at FROM identities WHERE normalized_email=? LIMIT 1`, normalizeEmail(email)).Scan(&identity.ID, &identity.ProviderGroup, &identity.Email, &identity.Priority, &identity.GlobalStatus, &identity.CreatedAt, &identity.UpdatedAt)
	if err == sql.ErrNoRows {
		identity = Identity{ID: "google-" + newShortID(), ProviderGroup: "google", Email: email, Priority: priority, GlobalStatus: IdentityHealthy}
	} else if err != nil {
		return IdentityDetail{}, err
	}
	if identity.Priority == 0 {
		identity.Priority = priority
	}
	identity.Email = strings.TrimSpace(identity.Email)
	identity.GlobalStatus = identityStatusOrHealthy(identity.GlobalStatus)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO identities(id, provider_group, email, normalized_email, priority, global_status)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			provider_group=excluded.provider_group,
			email=excluded.email,
			priority=excluded.priority,
			normalized_email=excluded.normalized_email,
			global_status=excluded.global_status,
			updated_at=CURRENT_TIMESTAMP`,
		identity.ID, identity.ProviderGroup, identity.Email, normalizeEmail(identity.Email), identity.Priority, identity.GlobalStatus)
	if err != nil {
		return IdentityDetail{}, fmt.Errorf("upsert identity: %w", err)
	}

	for _, spec := range specs {
		if spec.ID == "" {
			spec.ID = identity.ID + "-" + spec.Domain
		}
		if spec.MaxConcurrency < 1 {
			spec.MaxConcurrency = 1
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO credentials(id, identity_id, domain, status, max_concurrency, source_ref, fingerprint, pool_role, last_error_kind)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				identity_id=excluded.identity_id,
				domain=excluded.domain,
				status=excluded.status,
				max_concurrency=excluded.max_concurrency,
				source_ref=excluded.source_ref,
				fingerprint=excluded.fingerprint,
				pool_role=excluded.pool_role,
				last_error_kind=excluded.last_error_kind,
				updated_at=CURRENT_TIMESTAMP`,
			spec.ID, identity.ID, spec.Domain, StatusHealthy, spec.MaxConcurrency, spec.SourceRef, spec.Fingerprint, poolRoleOrAuto(spec.PoolRole), "")
		if err != nil {
			return IdentityDetail{}, fmt.Errorf("upsert credential: %w", err)
		}
		if len(spec.Secret) > 0 {
			payload, nonce, encErr := m.secretStore.Encrypt(spec.Secret)
			if encErr != nil {
				return IdentityDetail{}, fmt.Errorf("encrypt credential secret: %w", encErr)
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO credential_secrets(credential_id, encrypted_payload, nonce, key_version)
				VALUES (?, ?, ?, 1)
				ON CONFLICT(credential_id) DO UPDATE SET
					encrypted_payload=excluded.encrypted_payload,
					nonce=excluded.nonce,
					key_version=excluded.key_version,
					updated_at=CURRENT_TIMESTAMP`, spec.ID, payload, nonce)
			if err != nil {
				return IdentityDetail{}, fmt.Errorf("upsert credential secret: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return IdentityDetail{}, err
	}
	return m.GetIdentityDetail(ctx, identity.ID)
}

func (m *Manager) UpsertIdentity(ctx context.Context, identity Identity) error {
	if identity.ID == "" || identity.Email == "" || identity.ProviderGroup == "" {
		return fmt.Errorf("identity id, email and provider_group are required")
	}
	identity.Email = strings.TrimSpace(identity.Email)
	identity.GlobalStatus = identityStatusOrHealthy(identity.GlobalStatus)
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO identities(id, provider_group, email, normalized_email, priority, global_status)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			provider_group=excluded.provider_group,
			email=excluded.email,
				priority=excluded.priority,
				normalized_email=excluded.normalized_email,
				global_status=excluded.global_status,
			updated_at=CURRENT_TIMESTAMP`,
		identity.ID, identity.ProviderGroup, identity.Email, normalizeEmail(identity.Email), identity.Priority, identity.GlobalStatus)
	return err
}

func (m *Manager) UpsertCredential(ctx context.Context, c Credential, plaintextSecret []byte) error {
	if c.ID == "" || c.IdentityID == "" || c.Domain == "" {
		return fmt.Errorf("credential id, identity_id and domain are required")
	}
	if c.MaxConcurrency < 1 {
		c.MaxConcurrency = 1
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
			INSERT INTO credentials(id, identity_id, domain, status, max_concurrency, source_ref, fingerprint, pool_role, last_error_kind)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			identity_id=excluded.identity_id,
			domain=excluded.domain,
				status=excluded.status,
				max_concurrency=excluded.max_concurrency,
				source_ref=excluded.source_ref,
				fingerprint=excluded.fingerprint,
				pool_role=excluded.pool_role,
				last_error_kind=excluded.last_error_kind,
				updated_at=CURRENT_TIMESTAMP`,
		c.ID, c.IdentityID, c.Domain, statusOrHealthy(c.Status), c.MaxConcurrency, c.SourceRef, c.Fingerprint, poolRoleOrAuto(c.PoolRole), c.LastErrorKind)
	if err != nil {
		return fmt.Errorf("upsert credential: %w", err)
	}

	if len(plaintextSecret) > 0 {
		payload, nonce, err := m.secretStore.Encrypt(plaintextSecret)
		if err != nil {
			return fmt.Errorf("encrypt credential secret: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO credential_secrets(credential_id, encrypted_payload, nonce, key_version)
			VALUES (?, ?, ?, 1)
			ON CONFLICT(credential_id) DO UPDATE SET
				encrypted_payload=excluded.encrypted_payload,
				nonce=excluded.nonce,
				key_version=excluded.key_version,
				updated_at=CURRENT_TIMESTAMP`, c.ID, payload, nonce)
		if err != nil {
			return fmt.Errorf("upsert credential secret: %w", err)
		}
	}
	return tx.Commit()
}

func statusOrHealthy(status CredentialStatus) CredentialStatus {
	if status == "" {
		return StatusHealthy
	}
	return status
}

func identityStatusOrHealthy(status IdentityStatus) IdentityStatus {
	if status == "" || strings.EqualFold(string(status), "active") {
		return IdentityHealthy
	}
	return status
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
func poolRoleOrAuto(role PoolRole) PoolRole {
	if role == "" {
		return PoolAuto
	}
	return role
}

func newShortID() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

func (m *Manager) ReadSecret(ctx context.Context, credentialID string) ([]byte, error) {
	var payload, nonce []byte
	err := m.db.QueryRowContext(ctx, `SELECT encrypted_payload, nonce FROM credential_secrets WHERE credential_id=?`, credentialID).Scan(&payload, &nonce)
	if err == sql.ErrNoRows {
		return nil, ErrMissingCredentialSecret
	}
	if err != nil {
		return nil, err
	}
	return m.secretStore.Decrypt(payload, nonce)
}

func (m *Manager) FindIdentityByEmail(ctx context.Context, email string) (Identity, error) {
	var out Identity
	err := m.db.QueryRowContext(ctx, `SELECT id, provider_group, email, priority, global_status, created_at, updated_at FROM identities WHERE normalized_email=? OR lower(trim(email))=? LIMIT 1`, normalizeEmail(email), normalizeEmail(email)).Scan(&out.ID, &out.ProviderGroup, &out.Email, &out.Priority, &out.GlobalStatus, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

func (m *Manager) GetIdentityDetail(ctx context.Context, id string) (IdentityDetail, error) {
	var out IdentityDetail
	if err := m.db.QueryRowContext(ctx, `SELECT id, provider_group, email, priority, global_status, created_at, updated_at FROM identities WHERE id=?`, id).Scan(&out.Identity.ID, &out.Identity.ProviderGroup, &out.Identity.Email, &out.Identity.Priority, &out.Identity.GlobalStatus, &out.Identity.CreatedAt, &out.Identity.UpdatedAt); err != nil {
		return out, err
	}
	creds, err := m.List(ctx, "")
	if err != nil {
		return out, err
	}
	for i := range creds {
		if creds[i].IdentityID == id {
			c := creds[i]
			out.Credentials = append(out.Credentials, &c)
		}
	}
	out.Identity.GlobalStatus = aggregateIdentityStatus(out.Credentials)
	return out, nil
}

func (m *Manager) ListIdentityDetails(ctx context.Context, providerGroup string) ([]IdentityDetail, error) {
	query := `SELECT id, provider_group, email, priority, global_status, created_at, updated_at FROM identities`
	args := []any{}
	if providerGroup != "" {
		query += ` WHERE provider_group=?`
		args = append(args, providerGroup)
	}
	query += ` ORDER BY lower(email)`
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	identities := make([]Identity, 0)
	for rows.Next() {
		var item Identity
		if err := rows.Scan(&item.ID, &item.ProviderGroup, &item.Email, &item.Priority, &item.GlobalStatus, &item.CreatedAt, &item.UpdatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		identities = append(identities, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	// Close the identity cursor before querying credentials. SQLite is configured
	// with a single writer connection, so nested queries while rows are open can block.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]IdentityDetail, len(identities))
	if len(identities) == 0 {
		return out, nil
	}
	byID := make(map[string]int, len(identities))
	for i, item := range identities {
		out[i].Identity = item
		byID[item.ID] = i
	}
	// List once and group in its existing credential order, rather than fetching
	// the entire credential table again for every identity.
	creds, err := m.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range creds {
		if index, ok := byID[creds[i].IdentityID]; ok {
			out[index].Credentials = append(out[index].Credentials, &creds[i])
		}
	}
	for i := range out {
		out[i].Identity.GlobalStatus = aggregateIdentityStatus(out[i].Credentials)
	}
	return out, nil
}

func aggregateIdentityStatus(creds []*Credential) IdentityStatus {
	if len(creds) == 0 {
		return IdentityUnconfigured
	}
	allDisabled, anyHealthy, anyProblem := true, false, false
	for _, c := range creds {
		if c.Status != StatusDisabled {
			allDisabled = false
		}
		if c.Status == StatusHealthy || c.Status == StatusDegraded {
			anyHealthy = true
		}
		if c.Status != StatusHealthy {
			anyProblem = true
		}
	}
	if allDisabled {
		return IdentityDisabled
	}
	if anyHealthy && anyProblem {
		return IdentityPartial
	}
	if anyHealthy {
		return IdentityHealthy
	}
	return IdentityDegraded
}

func (m *Manager) List(ctx context.Context, domain string) ([]Credential, error) {
	query := `SELECT id, identity_id, domain, status, max_concurrency, cooldown_until, consecutive_errors, last_used_at, created_at, updated_at, source_ref, fingerprint, pool_role, last_error_kind FROM credentials`
	args := []any{}
	if domain != "" {
		query += " WHERE domain=?"
		args = append(args, domain)
	}
	query += " ORDER BY last_used_at IS NOT NULL, last_used_at ASC"
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Credential
	for rows.Next() {
		var c Credential
		var cooldown, lastUsed sql.NullTime
		var sourceRef, fingerprint, poolRole, lastError sql.NullString
		if err := rows.Scan(&c.ID, &c.IdentityID, &c.Domain, &c.Status, &c.MaxConcurrency, &cooldown, &c.ConsecutiveErrors, &lastUsed, &c.CreatedAt, &c.UpdatedAt, &sourceRef, &fingerprint, &poolRole, &lastError); err != nil {
			return nil, err
		}
		if cooldown.Valid {
			c.CooldownUntil = &cooldown.Time
		}
		if lastUsed.Valid {
			c.LastUsedAt = &lastUsed.Time
		}
		c.SourceRef, c.Fingerprint, c.LastErrorKind = sourceRef.String, fingerprint.String, lastError.String
		c.PoolRole = poolRoleOrAuto(PoolRole(poolRole.String))
		out = append(out, c)
	}
	return out, rows.Err()
}

func (m *Manager) SetStatus(ctx context.Context, id string, status CredentialStatus, cooldownUntil *time.Time, incrementError bool) error {
	_, err := m.db.ExecContext(ctx, `UPDATE credentials SET status=?, cooldown_until=?, consecutive_errors=CASE WHEN ? THEN consecutive_errors+1 ELSE consecutive_errors END, last_error_kind=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, cooldownUntil, incrementError, string(status), id)
	return err
}

func (m *Manager) MarkSuccess(ctx context.Context, id string) error {
	_, err := m.db.ExecContext(ctx, `UPDATE credentials SET status=?, cooldown_until=NULL, consecutive_errors=0, last_error_kind=NULL, last_used_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP WHERE id=?`, StatusHealthy, id)
	return err
}

func (m *Manager) StoreCredentialSecret(ctx context.Context, id string, plaintext []byte) error {
	if m == nil || m.secretStore == nil || len(plaintext) == 0 {
		return fmt.Errorf("credential secret store is unavailable")
	}
	payload, nonce, err := m.secretStore.Encrypt(plaintext)
	if err != nil {
		return err
	}
	_, err = m.db.ExecContext(ctx, `INSERT INTO credential_secrets(credential_id, encrypted_payload, nonce, key_version, updated_at) VALUES (?, ?, ?, 1, CURRENT_TIMESTAMP) ON CONFLICT(credential_id) DO UPDATE SET encrypted_payload=excluded.encrypted_payload, nonce=excluded.nonce, updated_at=CURRENT_TIMESTAMP`, id, payload, nonce)
	return err
}

func (m *Manager) DisableCredential(ctx context.Context, id string) error {
	result, err := m.db.ExecContext(ctx, `UPDATE credentials SET status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, StatusDisabled, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Manager) DeleteCredential(ctx context.Context, id string) error {
	result, err := m.db.ExecContext(ctx, `DELETE FROM credentials WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Manager) DeleteIdentity(ctx context.Context, id string) error {
	result, err := m.db.ExecContext(ctx, `DELETE FROM identities WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Manager) SetPoolRole(ctx context.Context, id string, role PoolRole) error {
	role = poolRoleOrAuto(role)
	if role != PoolAuto && role != PoolSession && role != PoolWorker {
		return fmt.Errorf("invalid pool role %q", role)
	}
	_, err := m.db.ExecContext(ctx, `UPDATE credentials SET pool_role=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, role, id)
	return err
}
