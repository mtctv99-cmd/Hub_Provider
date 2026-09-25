package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"sever_provider/internal/browser"
	"sever_provider/internal/identity"
	"sever_provider/internal/provider/gemini"
)

func (h *Handler) GoogleAutoLogin(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.Browser == nil {
		writeError(w, http.StatusServiceUnavailable, "browser manager is unavailable", "server_error", "browser_manager_unavailable")
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
	id, err := h.Browser.StartAutoLogin(strings.TrimSpace(req.ID), strings.TrimSpace(req.URL))
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
		writeError(w, status, err.Error(), "provider_error", code)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "account_id": id, "message": "Browser opened. Please login to Google; the session will be captured automatically."})
}

func (h *Handler) GoogleAutoLoginStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.Browser == nil {
		writeError(w, http.StatusServiceUnavailable, "browser manager is unavailable", "server_error", "browser_manager_unavailable")
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, h.Browser.Status())
}

func publicGoogleDetails(accounts []identity.IdentityDetail) []identity.IdentityDetail {
	for i := range accounts {
		accounts[i] = publicGoogleDetail(accounts[i])
	}
	return accounts
}

func publicGoogleDetail(account identity.IdentityDetail) identity.IdentityDetail {
	for _, credential := range account.Credentials {
		if credential == nil {
			continue
		}
		credential.SourceRef = ""
		credential.Fingerprint = ""
		credential.LastErrorKind = ""
	}
	return account
}

func (h *Handler) GoogleAccounts(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.IdentityDetails == nil {
		writeError(w, 503, "identity store is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	accounts, err := h.IdentityDetails.ListIdentityDetails(r.Context(), "google")
	if err != nil {
		writeError(w, 500, "failed to list Google accounts", "server_error", "identity_list_failed")
		return
	}
	writeJSON(w, 200, map[string]any{"accounts": publicGoogleDetails(accounts)})
}

func (h *Handler) GoogleAccount(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.IdentityDetails == nil {
		writeError(w, 503, "identity store is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/api/google/accounts/"), "/")
	if id == "" {
		writeError(w, 400, "account id is required", "invalid_request_error", "missing_parameter")
		return
	}
	account, err := h.IdentityDetails.GetIdentityDetail(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, publicGoogleDetail(account))
		return
	}
	if r.Method == http.MethodDelete {
		manager, ok := h.Credentials.(*identity.Manager)
		if !ok {
			writeError(w, 503, "identity manager is unavailable", "server_error", "identity_store_unavailable")
			return
		}
		if err := manager.DeleteIdentity(r.Context(), id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			writeError(w, 500, "failed to delete Google account", "server_error", "identity_delete_failed")
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "identity_id": id})
		return
	}
	if r.Method == http.MethodPatch {
		writeError(w, 501, "Google account mutation requires an explicit provisioning workflow", "provider_error", "provisioning_unavailable")
		return
	}
	methodNotAllowed(w)
}

func (h *Handler) GoogleDiscover(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"dry_run": true, "candidates": []any{}, "message": "configure a provider-specific discovery source to import credentials"})
}

func (h *Handler) GoogleImport(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	manager, ok := h.Credentials.(*identity.Manager)
	if !ok {
		writeError(w, 503, "identity manager is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	var req struct {
		Identity    identity.Identity `json:"identity"`
		Credentials []struct {
			Domain         string            `json:"domain"`
			ID             string            `json:"id"`
			SourceRef      string            `json:"source_ref"`
			Fingerprint    string            `json:"fingerprint"`
			PoolRole       identity.PoolRole `json:"pool_role"`
			MaxConcurrency int               `json:"max_concurrency"`
			Secret         json.RawMessage   `json:"secret"`
		} `json:"credentials"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Identity.Email) == "" {
		writeError(w, 422, "identity email is required", "invalid_request_error", "ambiguous_identity")
		return
	}
	specs := make([]identity.GoogleCredentialSpec, 0, len(req.Credentials))
	if len(req.Credentials) == 0 {
		writeError(w, 422, "at least one Google credential is required", "invalid_request_error", "missing_credential")
		return
	}
	for _, c := range req.Credentials {
		if c.Domain != "gemini_web" && c.Domain != "google_colab" {
			writeError(w, 422, "unsupported Google credential domain", "invalid_request_error", "invalid_domain")
			return
		}
		secret := c.Secret
		if c.Domain == "gemini_web" {
			var payload struct {
				Cookie string `json:"cookie"`
			}
			if err := json.Unmarshal(secret, &payload); err != nil || strings.TrimSpace(payload.Cookie) == "" {
				writeError(w, 422, "gemini credential secret must contain a cookie", "invalid_request_error", "invalid_credential_secret")
				return
			}
			clean, err := gemini.SanitizeCookie(payload.Cookie)
			if err != nil {
				writeError(w, 422, "invalid Gemini cookie", "invalid_request_error", "invalid_cookie")
				return
			}
			secret, _ = json.Marshal(map[string]string{"cookie": clean})
		}
		specs = append(specs, identity.GoogleCredentialSpec{Domain: c.Domain, ID: c.ID, SourceRef: c.SourceRef, Fingerprint: c.Fingerprint, PoolRole: c.PoolRole, MaxConcurrency: c.MaxConcurrency, Secret: secret})
	}
	account, err := manager.UpsertGoogleAccount(r.Context(), req.Identity.Email, req.Identity.Priority, specs)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "fingerprint") {
			writeError(w, 409, "credential fingerprint already exists", "invalid_request_error", "duplicate_credential")
			return
		}
		writeError(w, 500, "failed to import Google account", "server_error", "identity_import_failed")
		return
	}
	writeJSON(w, 201, account)
}

func findCredential(manager *identity.Manager, ctx context.Context, id string) (*identity.Credential, error) {
	rows, err := manager.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

func profileIDForCredential(cred *identity.Credential) string {
	if cred == nil {
		return ""
	}
	const prefix = "browser_profile:"
	if strings.HasPrefix(cred.SourceRef, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(cred.SourceRef, prefix))
	}
	return ""
}

func (h *Handler) GoogleCredential(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	manager, ok := h.Credentials.(*identity.Manager)
	if !ok {
		writeError(w, 503, "identity manager is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/api/google/credentials/"), "/")
	parts := strings.Split(rel, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, 400, "credential id is required", "invalid_request_error", "missing_parameter")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case action == "open-profile" && r.Method == http.MethodPost:
		if h.Browser == nil {
			writeError(w, http.StatusServiceUnavailable, "browser manager is unavailable", "server_error", "browser_manager_unavailable")
			return
		}
		cred, err := findCredential(manager, r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		profileID := profileIDForCredential(cred)
		if profileID == "" {
			profileID = id
		}
		if err := h.Browser.OpenProfile(profileID, "https://gemini.google.com/app"); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to open browser profile", "server_error", "browser_start_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "credential_id": id, "profile_id": profileID})
	case action == "refresh-cdp" && r.Method == http.MethodPost:
		if h.Browser == nil {
			writeError(w, http.StatusServiceUnavailable, "browser manager is unavailable", "server_error", "browser_manager_unavailable")
			return
		}
		cred, err := findCredential(manager, r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		profileID := profileIDForCredential(cred)
		if profileID == "" {
			profileID = id
		}
		cookie, err := h.Browser.RefreshAccountFromProfile(profileID)
		if err != nil {
			_ = manager.SetStatus(r.Context(), id, identity.StatusAuthRequired, nil, true)
			writeError(w, http.StatusUnprocessableEntity, "unable to refresh Google session from browser profile", "provider_error", "browser_refresh_failed")
			return
		}
		account, err := h.IdentityDetails.GetIdentityDetail(r.Context(), cred.IdentityID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load Google identity", "server_error", "identity_lookup_failed")
			return
		}
		secret, _ := json.Marshal(map[string]string{"cookie": cookie})
		_, err = manager.UpsertGoogleAccount(r.Context(), account.Identity.Email, account.Identity.Priority, []identity.GoogleCredentialSpec{{ID: id, Domain: "gemini_web", SourceRef: "browser_profile:" + profileID, PoolRole: cred.PoolRole, MaxConcurrency: cred.MaxConcurrency, Secret: secret}})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to store refreshed Google session", "server_error", "credential_refresh_store_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "credential_id": id, "profile_id": profileID, "status": "HEALTHY"})
	case action == "disable" && r.Method == http.MethodPost:
		if err := manager.DisableCredential(r.Context(), id); err != nil {
			http.NotFound(w, r)
			return
		}
		if h.Leases != nil {
			h.Leases.Unregister(id)
		}
		writeJSON(w, 200, map[string]any{"credential_id": id, "status": "DISABLED"})
	case action == "validate" && r.Method == http.MethodPost:
		secret, err := manager.ReadSecret(r.Context(), id)
		if err != nil {
			writeError(w, 422, "credential secret is unavailable", "invalid_request_error", "credential_secret_unavailable")
			return
		}
		var payload struct {
			Cookie string `json:"cookie"`
		}
		if err := json.Unmarshal(secret, &payload); err != nil {
			writeError(w, 422, "credential secret is invalid", "invalid_request_error", "invalid_credential_secret")
			return
		}
		validator := h.GeminiValidator
		if validator == nil && h.Dispatcher != nil && h.Dispatcher.Adapters != nil {
			if adapter, ok := h.Dispatcher.Adapters.Get("gemini_web"); ok {
				if geminiAdapter, ok := adapter.(*gemini.Adapter); ok {
					validator = geminiAdapter
				}
			}
		}
		if validator == nil {
			// Keep the admin action usable in lightweight/test routers without a provider adapter;
			// this records no upstream validation and explicitly reports that fact.
			if err := manager.MarkSuccess(r.Context(), id); err != nil {
				writeError(w, 500, "failed to update credential status", "server_error", "credential_status_update_failed")
				return
			}
			writeJSON(w, 200, map[string]any{"credential_id": id, "status": "HEALTHY", "valid": false, "validation_available": false})
			return
		}
		if err := validator.ValidateCookie(r.Context(), payload.Cookie); err != nil {
			_ = manager.SetStatus(r.Context(), id, identity.StatusAuthRequired, nil, true)
			writeError(w, 422, "Gemini cookie is invalid or expired", "invalid_request_error", "cookie_invalid_or_expired")
			return
		}
		if err := manager.MarkSuccess(r.Context(), id); err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"credential_id": id, "status": "HEALTHY", "valid": true})
	case action == "get-api" && r.Method == http.MethodPost:
		cmdStr := strings.TrimSpace(os.Getenv("GEMINI_BROWSER_CMD"))
		if cmdStr == "" {
			cmdStr = strings.TrimSpace(os.Getenv("BROWSER_CMD"))
		}
		if cmdStr == "" {
			writeError(w, 501, "browser launcher is not configured (set GEMINI_BROWSER_CMD)", "provider_error", "browser_launcher_unavailable")
			return
		}
		parts := strings.Fields(cmdStr)
		if len(parts) == 0 {
			writeError(w, 501, "invalid browser launcher command", "provider_error", "browser_launcher_invalid")
			return
		}
		cmd := exec.CommandContext(context.Background(), parts[0], append(parts[1:], "https://aistudio.google.com/app/apikey")...)
		if err := cmd.Start(); err != nil {
			writeError(w, 500, "failed to start browser process", "server_error", "browser_start_failed")
			return
		}
		writeJSON(w, 200, map[string]any{"success": true, "credential_id": id, "message": "AI Studio opened. Create a key, then paste it into the secure prompt."})
	case action == "store-api-key" && r.Method == http.MethodPost:
		var req struct {
			APIKey string `json:"api_key"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		key := strings.TrimSpace(req.APIKey)
		if len(key) < 20 || len(key) > 512 {
			writeError(w, 422, "invalid API key format", "invalid_request_error", "invalid_api_key")
			return
		}
		secretPayload, _ := json.Marshal(map[string]string{"kind": "google_ai_studio_api_key", "api_key": key})
		if err := manager.StoreCredentialSecret(r.Context(), id, secretPayload); err != nil {
			writeError(w, 500, "failed to store API key securely", "server_error", "credential_secret_store_failed")
			return
		}
		if err := manager.MarkSuccess(r.Context(), id); err != nil {
			writeError(w, 500, "failed to update credential status", "server_error", "credential_status_update_failed")
			return
		}
		writeJSON(w, 200, map[string]any{"success": true, "credential_id": id, "status": "HEALTHY", "message": "API key encrypted and stored; browser may be closed."})
	case action == "open-browser" && r.Method == http.MethodPost:
		cmdStr := strings.TrimSpace(os.Getenv("GEMINI_BROWSER_CMD"))
		urlTarget := "https://gemini.google.com"
		if cmdStr == "" {
			cmdStr = strings.TrimSpace(os.Getenv("BROWSER_CMD"))
		}
		if cmdStr == "" {
			writeError(w, 501, "browser launcher is not configured (set GEMINI_BROWSER_CMD)", "provider_error", "browser_launcher_unavailable")
			return
		}
		parts := strings.Fields(cmdStr)
		if len(parts) == 0 {
			writeError(w, 501, "invalid browser launcher command", "provider_error", "browser_launcher_invalid")
			return
		}
		args := append(parts[1:], urlTarget)
		cmd := exec.CommandContext(context.Background(), parts[0], args...)
		if err := cmd.Start(); err != nil {
			writeError(w, 500, "failed to start browser process", "server_error", "browser_start_failed")
			return
		}
		writeJSON(w, 200, map[string]any{
			"success":       true,
			"credential_id": id,
			"url":           urlTarget,
			"message":       "Browser opened. Please check login state.",
		})
	case action == "rotate" && r.Method == http.MethodPost:
		writeError(w, 501, "credential rotation requires provider authentication", "provider_error", "credential_validation_unavailable")
	case action == "" && r.Method == http.MethodDelete:
		if err := manager.DeleteCredential(r.Context(), id); err != nil {
			http.NotFound(w, r)
			return
		}
		if h.Leases != nil {
			h.Leases.Unregister(id)
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "credential_id": id})
	default:
		methodNotAllowed(w)
	}
}
