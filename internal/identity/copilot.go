package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrCopilotIdentityConflict = errors.New("Copilot identity conflicts with an existing account")

type CopilotCredentialSpec struct {
	IdentityID     string
	CredentialID   string
	Email          string
	Priority       int
	MaxConcurrency int
	PoolRole       PoolRole
	SourceRef      string
	Secret         []byte
}

// UpsertCopilotAccount commits identity, credential and encrypted token together.
func (m *Manager) UpsertCopilotAccount(ctx context.Context, spec CopilotCredentialSpec) (IdentityDetail, error) {
	spec.Email = strings.TrimSpace(spec.Email)
	if normalizeEmail(spec.Email) == "" || len(spec.Secret) == 0 {
		return IdentityDetail{}, fmt.Errorf("Copilot email and token are required")
	}
	if spec.MaxConcurrency < 1 {
		spec.MaxConcurrency = 1
	}
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
		if existingGroup != "copilot" || (spec.IdentityID != "" && spec.IdentityID != existingID) {
			return IdentityDetail{}, ErrCopilotIdentityConflict
		}
		spec.IdentityID = existingID
	} else if spec.IdentityID == "" {
		spec.IdentityID = "copilot-" + newShortID()
	}
	var ownerEmail, ownerGroup string
	err = tx.QueryRowContext(ctx, `SELECT normalized_email, provider_group FROM identities WHERE id=?`, spec.IdentityID).Scan(&ownerEmail, &ownerGroup)
	if err != nil && err != sql.ErrNoRows {
		return IdentityDetail{}, err
	}
	if err == nil && (ownerEmail != normalizeEmail(spec.Email) || ownerGroup != "copilot") {
		return IdentityDetail{}, ErrCopilotIdentityConflict
	}
	if spec.CredentialID == "" {
		spec.CredentialID = spec.IdentityID + "-copilot_ws"
	}
	var credentialOwner, credentialDomain string
	err = tx.QueryRowContext(ctx, `SELECT identity_id, domain FROM credentials WHERE id=?`, spec.CredentialID).Scan(&credentialOwner, &credentialDomain)
	if err != nil && err != sql.ErrNoRows {
		return IdentityDetail{}, err
	}
	if err == nil && (credentialOwner != spec.IdentityID || credentialDomain != "copilot_ws") {
		return IdentityDetail{}, ErrCopilotIdentityConflict
	}
	var existingCredentialID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM credentials WHERE identity_id=? AND domain='copilot_ws'`, spec.IdentityID).Scan(&existingCredentialID)
	if err != nil && err != sql.ErrNoRows {
		return IdentityDetail{}, err
	}
	if err == nil && existingCredentialID != spec.CredentialID {
		return IdentityDetail{}, ErrCopilotIdentityConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO identities(id, provider_group, email, normalized_email, priority, global_status)
		VALUES (?, 'copilot', ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET email=excluded.email, normalized_email=excluded.normalized_email,
			priority=excluded.priority, global_status=excluded.global_status, updated_at=CURRENT_TIMESTAMP`,
		spec.IdentityID, spec.Email, normalizeEmail(spec.Email), spec.Priority, IdentityDegraded)
	if err != nil {
		return IdentityDetail{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO credentials(id, identity_id, domain, status, max_concurrency, source_ref, pool_role, last_error_kind)
		VALUES (?, ?, 'copilot_ws', ?, ?, ?, ?, 'upstream_unverified')
		ON CONFLICT(id) DO UPDATE SET status=excluded.status, max_concurrency=excluded.max_concurrency,
			source_ref=CASE WHEN excluded.source_ref <> '' THEN excluded.source_ref ELSE source_ref END,
			pool_role=excluded.pool_role, last_error_kind=excluded.last_error_kind, updated_at=CURRENT_TIMESTAMP`,
		spec.CredentialID, spec.IdentityID, StatusDegraded, spec.MaxConcurrency, spec.SourceRef, poolRoleOrAuto(spec.PoolRole))
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
