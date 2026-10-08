package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrMetaIdentityConflict = errors.New("Meta identity conflicts with an existing account")

type MetaCredentialSpec struct {
	IdentityID     string
	CredentialID   string
	Email          string
	Priority       int
	MaxConcurrency int
	PoolRole       PoolRole
	SourceRef      string
	Domain         string
	Secret         []byte
}

func metaDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == DomainMuseWeb {
		return DomainMuseWeb
	}
	return DomainMetaWeb
}

// UpsertMetaAccount commits identity, credential and encrypted cookie together.
func (m *Manager) UpsertMetaAccount(ctx context.Context, spec MetaCredentialSpec) (IdentityDetail, error) {
	spec.Email = strings.TrimSpace(spec.Email)
	if normalizeEmail(spec.Email) == "" || len(spec.Secret) == 0 {
		return IdentityDetail{}, fmt.Errorf("Meta email and cookie are required")
	}
	if spec.MaxConcurrency < 1 {
		spec.MaxConcurrency = 1
	}
	domain := metaDomain(spec.Domain)
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return IdentityDetail{}, err
	}
	defer tx.Rollback()

	var existingID, existingGroup string
	err = tx.QueryRowContext(ctx, `SELECT id, provider_group FROM identities WHERE normalized_email=? LIMIT 1`, normalizeEmail(spec.Email)).Scan(&existingID, &existingGroup)
	if err != nil && err != sql.ErrNoRows {
		return IdentityDetail{}, err
	}
	if err == nil {
		if existingGroup != "meta" || (spec.IdentityID != "" && spec.IdentityID != existingID) {
			return IdentityDetail{}, ErrMetaIdentityConflict
		}
		spec.IdentityID = existingID
	} else if spec.IdentityID == "" {
		spec.IdentityID = "meta-" + newShortID()
	}
	var ownerEmail, ownerGroup string
	err = tx.QueryRowContext(ctx, `SELECT normalized_email, provider_group FROM identities WHERE id=?`, spec.IdentityID).Scan(&ownerEmail, &ownerGroup)
	if err != nil && err != sql.ErrNoRows {
		return IdentityDetail{}, err
	}
	if err == nil && (ownerEmail != normalizeEmail(spec.Email) || ownerGroup != "meta") {
		return IdentityDetail{}, ErrMetaIdentityConflict
	}
	if spec.CredentialID == "" {
		spec.CredentialID = spec.IdentityID + "-" + domain
	}
	var credentialOwner, credentialDomain string
	err = tx.QueryRowContext(ctx, `SELECT identity_id, domain FROM credentials WHERE id=?`, spec.CredentialID).Scan(&credentialOwner, &credentialDomain)
	if err != nil && err != sql.ErrNoRows {
		return IdentityDetail{}, err
	}
	if err == nil && (credentialOwner != spec.IdentityID || (credentialDomain != DomainMetaWeb && credentialDomain != DomainMuseWeb)) {
		return IdentityDetail{}, ErrMetaIdentityConflict
	}
	var existingCredentialID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM credentials WHERE identity_id=? AND domain=?`, spec.IdentityID, domain).Scan(&existingCredentialID)
	if err != nil && err != sql.ErrNoRows {
		return IdentityDetail{}, err
	}
	if err == nil && existingCredentialID != spec.CredentialID {
		return IdentityDetail{}, ErrMetaIdentityConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO identities(id, provider_group, email, normalized_email, priority, global_status)
		VALUES (?, 'meta', ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET email=excluded.email, normalized_email=excluded.normalized_email,
			priority=excluded.priority, global_status=excluded.global_status, updated_at=CURRENT_TIMESTAMP`,
		spec.IdentityID, spec.Email, normalizeEmail(spec.Email), spec.Priority, IdentityDegraded)
	if err != nil {
		return IdentityDetail{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO credentials(id, identity_id, domain, status, max_concurrency, source_ref, pool_role, last_error_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'upstream_unverified')
		ON CONFLICT(id) DO UPDATE SET status=excluded.status, max_concurrency=excluded.max_concurrency,
			source_ref=CASE WHEN excluded.source_ref <> '' THEN excluded.source_ref ELSE source_ref END,
			pool_role=excluded.pool_role, last_error_kind=excluded.last_error_kind, updated_at=CURRENT_TIMESTAMP`,
		spec.CredentialID, spec.IdentityID, domain, StatusDegraded, spec.MaxConcurrency, spec.SourceRef, poolRoleOrAuto(spec.PoolRole))
	if err != nil {
		return IdentityDetail{}, err
	}
	payload, nonce, err := m.secretStore.Encrypt(spec.Secret)
	if err != nil {
		return IdentityDetail{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO credential_secrets(credential_id, encrypted_payload, nonce, key_version)
		VALUES (?, ?, ?, 1) ON CONFLICT(credential_id) DO UPDATE SET encrypted_payload=excluded.encrypted_payload,
			nonce=excluded.nonce, key_version=excluded.key_version, updated_at=CURRENT_TIMESTAMP`, spec.CredentialID, payload, nonce)
	if err != nil {
		return IdentityDetail{}, err
	}
	if err := tx.Commit(); err != nil {
		return IdentityDetail{}, err
	}
	return m.GetIdentityDetail(ctx, spec.IdentityID)
}
