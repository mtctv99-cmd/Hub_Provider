package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"sever_provider/internal/browser"
	"sever_provider/internal/identity"
	"sever_provider/internal/provider/copilot"
)

// CopilotImport provisions an encrypted Copilot token URL under a dedicated
// identity. The token URL is never returned after ingestion.
func (h *Handler) CopilotImport(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	manager, ok := h.Credentials.(*identity.Manager)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "identity manager is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	var req struct {
		Identity struct {
			ID       string `json:"id"`
			Email    string `json:"email"`
			Priority int    `json:"priority"`
		} `json:"identity"`
		Credential struct {
			ID             string            `json:"id"`
			MaxConcurrency int               `json:"max_concurrency"`
			PoolRole       identity.PoolRole `json:"pool_role"`
			TokenURL       string            `json:"token_url"`
			Secret         json.RawMessage   `json:"secret"`
		} `json:"credential"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20*1024)
	if !decodeJSON(w, r, &req) {
		return
	}
	tokenURL := strings.TrimSpace(req.Credential.TokenURL)
	if tokenURL == "" && len(req.Credential.Secret) > 0 {
		var secret struct {
			TokenURL string `json:"token_url"`
		}
		if json.Unmarshal(req.Credential.Secret, &secret) == nil {
			tokenURL = strings.TrimSpace(secret.TokenURL)
		}
	}
	expiresAt, tokenEmail, err := copilot.ValidateCapturedToken(tokenURL, 30)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Copilot token URL is invalid or expired", "invalid_request_error", "invalid_copilot_token")
		return
	}
	if strings.TrimSpace(req.Identity.Email) == "" {
		req.Identity.Email = tokenEmail
	} else if !strings.EqualFold(strings.TrimSpace(req.Identity.Email), tokenEmail) {
		writeError(w, http.StatusUnprocessableEntity, "Copilot identity does not match token user", "invalid_request_error", "identity_mismatch")
		return
	}

	if req.Credential.PoolRole != "" && req.Credential.PoolRole != identity.PoolAuto && req.Credential.PoolRole != identity.PoolSession && req.Credential.PoolRole != identity.PoolWorker {
		writeError(w, http.StatusUnprocessableEntity, "invalid pool role", "invalid_request_error", "invalid_pool_role")
		return
	}
	secret, _ := json.Marshal(map[string]string{"token_url": tokenURL})
	detail, err := manager.UpsertCopilotAccount(r.Context(), identity.CopilotCredentialSpec{
		IdentityID: strings.TrimSpace(req.Identity.ID), CredentialID: strings.TrimSpace(req.Credential.ID),
		Email: req.Identity.Email, Priority: req.Identity.Priority, MaxConcurrency: req.Credential.MaxConcurrency,
		PoolRole: req.Credential.PoolRole, Secret: secret,
	})
	if errors.Is(err, identity.ErrCopilotIdentityConflict) {
		writeError(w, http.StatusConflict, "Copilot identity or credential belongs to another account", "invalid_request_error", "identity_conflict")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store Copilot credential", "server_error", "credential_import_failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"identity_id": detail.Identity.ID, "credential_id": detail.Credentials[0].ID, "domain": "copilot_ws", "status": "DEGRADED", "expires_at": expiresAt, "validation": "claims_only"})
}

var copilotBrowsers sync.Map // *Handler -> *browser.CopilotManager

func (h *Handler) CopilotBrowserManager() *browser.CopilotManager {
	if existing, ok := copilotBrowsers.Load(h); ok {
		return existing.(*browser.CopilotManager)
	}
	candidate := browser.NewCopilotManager("data/profiles", func(ctx context.Context, accountID, email, tokenURL string, expiresAt time.Time) error {
		manager, _ := h.Credentials.(*identity.Manager)
		if manager == nil {
			return errors.New("identity manager is unavailable")
		}
		secret, _ := json.Marshal(map[string]string{"token_url": tokenURL})
		if existing, findErr := manager.FindIdentityByEmail(ctx, email); findErr == nil {
			if existing.ProviderGroup != "copilot" {
				return identity.ErrCopilotIdentityConflict
			}
			accountID = existing.ID
		}
		_, err := manager.UpsertCopilotAccount(ctx, identity.CopilotCredentialSpec{
			IdentityID:     accountID,
			Email:          email,
			MaxConcurrency: 3,
			PoolRole:       identity.PoolSession,
			Secret:         secret,
		})
		return err
	})
	actual, _ := copilotBrowsers.LoadOrStore(h, candidate)
	return actual.(*browser.CopilotManager)
}

func (h *Handler) CopilotAutoLogin(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	id, err := h.CopilotBrowserManager().Start(strings.TrimSpace(req.ID))
	if err != nil {
		status := http.StatusInternalServerError
		code := "browser_start_failed"
		if errors.Is(err, browser.ErrAutoLoginBusy) {
			status = http.StatusConflict
			code = "browser_login_busy"
		} else if errors.Is(err, browser.ErrBrowserNotFound) {
			status = http.StatusNotImplemented
			code = "browser_not_found"
		}
		writeError(w, status, "unable to start Copilot browser login", "provider_error", code)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"success":    true,
		"account_id": id,
		"message":    "Browser opened for Copilot. Sign in manually to capture session.",
	})
}

func (h *Handler) CopilotAutoLoginStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	status := h.CopilotBrowserManager().Status()
	writeJSON(w, http.StatusOK, status)
}

func (h *Handler) CopilotAutoLoginStop(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	h.CopilotBrowserManager().Stop()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Copilot browser login stopped."})
}

func (h *Handler) CopilotAccounts(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if h.IdentityDetails == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	accounts, err := h.IdentityDetails.ListIdentityDetails(r.Context(), "copilot")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list Copilot accounts", "server_error", "identity_list_failed")
		return
	}
	type safeCredential struct {
		ID             string                    `json:"id"`
		Domain         string                    `json:"domain"`
		Status         identity.CredentialStatus `json:"status"`
		MaxConcurrency int                       `json:"max_concurrency"`
		ActiveLeases   int32                     `json:"active_leases"`
		PoolRole       identity.PoolRole         `json:"pool_role"`
		UpdatedAt      time.Time                 `json:"updated_at"`
	}
	type safeAccount struct {
		Identity    identity.Identity `json:"identity"`
		Credentials []safeCredential  `json:"credentials"`
	}
	result := make([]safeAccount, 0, len(accounts))
	for _, account := range accounts {
		entry := safeAccount{Identity: account.Identity, Credentials: make([]safeCredential, 0, len(account.Credentials))}
		for _, cred := range account.Credentials {
			if cred != nil && cred.Domain == "copilot_ws" {
				entry.Credentials = append(entry.Credentials, safeCredential{ID: cred.ID, Domain: cred.Domain, Status: cred.Status, MaxConcurrency: cred.MaxConcurrency, ActiveLeases: cred.ActiveLeases, PoolRole: cred.PoolRole, UpdatedAt: cred.UpdatedAt})
			}
		}
		result = append(result, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": result})
}

func (h *Handler) CopilotSaveToken(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID       string `json:"id"`
		URL      string `json:"url"`
		TokenURL string `json:"token_url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tokenURL := strings.TrimSpace(req.URL)
	if tokenURL == "" {
		tokenURL = strings.TrimSpace(req.TokenURL)
	}
	if tokenURL == "" {
		writeError(w, http.StatusBadRequest, "token url is required", "invalid_request_error", "missing_url")
		return
	}
	expiresAt, tokenEmail, err := copilot.ValidateCapturedToken(tokenURL, 30)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Copilot token URL is invalid or expired: "+err.Error(), "invalid_request_error", "invalid_copilot_token")
		return
	}
	manager, ok := h.Credentials.(*identity.Manager)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "identity manager is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	accountID := strings.TrimSpace(req.ID)
	secret, _ := json.Marshal(map[string]string{"token_url": tokenURL})
	detail, err := manager.UpsertCopilotAccount(r.Context(), identity.CopilotCredentialSpec{
		IdentityID:     accountID,
		Email:          tokenEmail,
		MaxConcurrency: 3,
		PoolRole:       identity.PoolSession,
		Secret:         secret,
	})
	if errors.Is(err, identity.ErrCopilotIdentityConflict) {
		if existing, findErr := manager.FindIdentityByEmail(r.Context(), tokenEmail); findErr == nil {
			accountID = existing.ID
			detail, err = manager.UpsertCopilotAccount(r.Context(), identity.CopilotCredentialSpec{
				IdentityID:     accountID,
				Email:          tokenEmail,
				MaxConcurrency: 3,
				PoolRole:       identity.PoolSession,
				Secret:         secret,
			})
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store Copilot token: "+err.Error(), "server_error", "credential_save_failed")
		return
	}
	credID := ""
	if len(detail.Credentials) > 0 && detail.Credentials[0] != nil {
		credID = detail.Credentials[0].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"identity_id":   detail.Identity.ID,
		"credential_id": credID,
		"email":         tokenEmail,
		"expires_at":    expiresAt,
	})
}

func (h *Handler) CopilotDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required", "invalid_request_error", "missing_id")
		return
	}
	manager, ok := h.Credentials.(*identity.Manager)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "identity manager is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	account, err := manager.GetIdentityDetail(r.Context(), id)
	if err != nil || account.Identity.ProviderGroup != "copilot" {
		writeError(w, http.StatusNotFound, "Copilot identity not found", "invalid_request_error", "account_not_found")
		return
	}
	if err := manager.DeleteIdentity(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete Copilot identity: "+err.Error(), "server_error", "delete_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted_id": id})
}

func (h *Handler) CopilotStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if h.IdentityDetails == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	accounts, err := h.IdentityDetails.ListIdentityDetails(r.Context(), "copilot")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list Copilot accounts", "server_error", "identity_list_failed")
		return
	}
	loginStatus := h.CopilotBrowserManager().Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"accounts_count": len(accounts),
		"is_logging_in":  loginStatus.Running,
		"login_message":  loginStatus.Message,
	})
}
