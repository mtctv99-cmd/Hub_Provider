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
)

var metaBrowsers sync.Map

func (h *Handler) MetaBrowserManager() *browser.MetaManager {
	if existing, ok := metaBrowsers.Load(h); ok {
		return existing.(*browser.MetaManager)
	}
	candidate := browser.NewMetaManager("data/profiles", func(ctx context.Context, accountID, email, cookieStr, tokenStr string) error {
		manager, _ := h.Credentials.(*identity.Manager)
		if manager == nil {
			return errors.New("identity manager is unavailable")
		}

		secretBytes, err := json.Marshal(map[string]string{
			"cookies": cookieStr,
			"token":   tokenStr,
		})
		if err != nil {
			return err
		}

		_, err = manager.UpsertMetaAccount(ctx, identity.MetaCredentialSpec{
			IdentityID:     accountID,
			Email:          email,
			MaxConcurrency: 2,
			PoolRole:       identity.PoolSession,
			SourceRef:      "browser_profile:" + accountID,
			Domain:         identity.DomainMetaWeb,
			Secret:         secretBytes,
		})
		return err
	})
	actual, _ := metaBrowsers.LoadOrStore(h, candidate)
	return actual.(*browser.MetaManager)
}

// MetaAutoLogin triggers opening the browser for Meta / muse.ai login.
func (h *Handler) MetaAutoLogin(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}

	targetURL := strings.TrimSpace(req.URL)
	if targetURL == "" {
		targetURL = browser.DefaultMetaHome
	}

	id, err := h.MetaBrowserManager().StartWithURL(strings.TrimSpace(req.ID), targetURL)
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
		writeError(w, status, "unable to start Meta browser login", "provider_error", code)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"success":    true,
		"account_id": id,
		"message":    "Trình duyệt đã mở. Hãy đăng nhập tài khoản Meta (muse.ai) trên cửa sổ vừa mở.",
	})
}

// MetaAutoLoginStatus returns the current status of the Meta auto-login process.
func (h *Handler) MetaAutoLoginStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	status := h.MetaBrowserManager().Status()
	writeJSON(w, http.StatusOK, status)
}

// MetaAutoLoginStop stops the Meta auto-login browser session.
func (h *Handler) MetaAutoLoginStop(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	h.MetaBrowserManager().Stop()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Đã dừng trình duyệt đăng nhập Meta."})
}

// MetaAccounts handles list, create, and delete for Meta / muse.ai accounts.
func (h *Handler) MetaAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}

	if !h.requireAdmin(w, r) {
		return
	}

	manager, _ := h.Credentials.(*identity.Manager)

	if r.Method == http.MethodPost {
		if manager == nil {
			writeError(w, http.StatusServiceUnavailable, "identity manager is unavailable", "server_error", "identity_store_unavailable")
			return
		}

		var req struct {
			Email          string            `json:"email"`
			Cookies        string            `json:"cookies"`
			Token          string            `json:"token"`
			Session        string            `json:"session"`
			MaxConcurrency int               `json:"max_concurrency"`
			IdentityID     string            `json:"identity_id"`
			PoolRole       identity.PoolRole `json:"pool_role"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}

		cookieStr := strings.TrimSpace(req.Cookies)
		if cookieStr == "" {
			cookieStr = strings.TrimSpace(req.Session)
		}
		tokenStr := strings.TrimSpace(req.Token)
		if cookieStr == "" && tokenStr == "" {
			writeError(w, http.StatusUnprocessableEntity, "cookies or access token is required", "invalid_request_error", "missing_credentials")
			return
		}

		email := strings.TrimSpace(req.Email)
		if email == "" {
			if tokenStr != "" {
				email = browser.ExtractEmailFromToken(tokenStr)
			}
			if email == "" {
				email = "meta_user_" + strings.ToLower(identity.NewShortID())[:6] + "@meta.ai"
			}
		}

		identityID := strings.TrimSpace(req.IdentityID)
		if identityID == "" {
			identityID = "meta-" + strings.ToLower(email)
		}

		if req.MaxConcurrency < 1 {
			req.MaxConcurrency = 2
		}

		secretBytes, err := json.Marshal(map[string]string{
			"cookies": cookieStr,
			"token":   tokenStr,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to marshal secret", "server_error", "secret_marshal_failed")
			return
		}

		detail, err := manager.UpsertMetaAccount(r.Context(), identity.MetaCredentialSpec{
			IdentityID:     identityID,
			Email:          email,
			MaxConcurrency: req.MaxConcurrency,
			PoolRole:       req.PoolRole,
			Domain:         identity.DomainMetaWeb,
			Secret:         secretBytes,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to store Meta account: "+err.Error(), "server_error", "account_upsert_failed")
			return
		}

		credID := ""
		if len(detail.Credentials) > 0 && detail.Credentials[0] != nil {
			credID = detail.Credentials[0].ID
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"identity_id":   detail.Identity.ID,
			"credential_id": credID,
			"status":        "DEGRADED",
			"email":         email,
		})
		return
	}

	if r.Method == http.MethodDelete {
		if manager == nil {
			writeError(w, http.StatusServiceUnavailable, "identity manager is unavailable", "server_error", "identity_store_unavailable")
			return
		}

		var req struct {
			IdentityID string `json:"identity_id"`
			ID         string `json:"id"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}

		identityID := strings.TrimSpace(req.IdentityID)
		if identityID == "" {
			identityID = strings.TrimSpace(req.ID)
		}
		if identityID == "" {
			writeError(w, http.StatusBadRequest, "identity_id is required", "invalid_request_error", "missing_identity")
			return
		}

		detail, err := manager.GetIdentityDetail(r.Context(), identityID)
		if err != nil || detail.Identity.ProviderGroup != "meta" {
			writeError(w, http.StatusNotFound, "Meta identity not found", "invalid_request_error", "account_not_found")
			return
		}

		for _, credential := range detail.Credentials {
			if credential == nil || (credential.Domain != identity.DomainMetaWeb && credential.Domain != identity.DomainMuseWeb) {
				continue
			}
			if err := manager.DeleteCredential(r.Context(), credential.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to delete credential", "server_error", "credential_delete_failed")
				return
			}
		}

		if err := manager.DeleteIdentity(r.Context(), identityID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete identity", "server_error", "identity_delete_failed")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"deleted_id": identityID,
			"success":    true,
		})
		return
	}

	// GET: list Meta accounts
	if h.IdentityDetails == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store is unavailable", "server_error", "identity_store_unavailable")
		return
	}

	accounts, err := h.IdentityDetails.ListIdentityDetails(r.Context(), "meta")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list Meta accounts", "server_error", "identity_list_failed")
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
			if cred != nil && (cred.Domain == identity.DomainMetaWeb || cred.Domain == identity.DomainMuseWeb) {
				entry.Credentials = append(entry.Credentials, safeCredential{
					ID:             cred.ID,
					Domain:         cred.Domain,
					Status:         cred.Status,
					MaxConcurrency: cred.MaxConcurrency,
					ActiveLeases:   cred.ActiveLeases,
					PoolRole:       cred.PoolRole,
					UpdatedAt:      cred.UpdatedAt,
				})
			}
		}
		result = append(result, entry)
	}

	writeJSON(w, http.StatusOK, map[string]any{"accounts": result})
}
