package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"sever_provider/internal/browser"
	"sever_provider/internal/identity"
)

var deepseekBrowsers sync.Map

func (h *Handler) DeepSeekBrowserManager() *browser.DeepSeekManager {
	if existing, ok := deepseekBrowsers.Load(h); ok {
		return existing.(*browser.DeepSeekManager)
	}
	candidate := browser.NewDeepSeekManager("data/profiles", func(ctx context.Context, accountID, email, token string) error {
		manager, _ := h.Credentials.(*identity.Manager)
		if manager == nil {
			return errors.New("identity manager is unavailable")
		}

		identityID := "deepseek-" + strings.ToLower(email)
		if err := manager.UpsertIdentity(ctx, identity.Identity{
			ID:            identityID,
			ProviderGroup: "deepseek",
			Email:         email,
			Priority:      1,
			GlobalStatus:  identity.IdentityHealthy,
		}); err != nil {
			return err
		}

		credentialID := identityID + "-deepseek_web"
		secretBytes, _ := json.Marshal(map[string]string{
			"token": token,
		})

		return manager.UpsertCredential(ctx, identity.Credential{
			ID:             credentialID,
			IdentityID:     identityID,
			Domain:         "deepseek_web",
			MaxConcurrency: 2,
			Status:         identity.StatusHealthy,
			SourceRef:      "browser_profile:" + accountID,
		}, secretBytes)
	})
	actual, _ := deepseekBrowsers.LoadOrStore(h, candidate)
	return actual.(*browser.DeepSeekManager)
}

// DeepSeekAutoLogin kích hoạt mở trình duyệt để người dùng đăng nhập DeepSeek.
func (h *Handler) DeepSeekAutoLogin(w http.ResponseWriter, r *http.Request) {
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
	requestedID := strings.TrimSpace(req.ID)
	profileID := requestedID
	manager, _ := h.Credentials.(*identity.Manager)
	if manager != nil && requestedID != "" {
		if detail, err := manager.GetIdentityDetail(r.Context(), requestedID); err == nil {
			for _, cred := range detail.Credentials {
				if cred == nil || cred.Domain != "deepseek_web" {
					continue
				}
				if profile := strings.TrimPrefix(cred.SourceRef, "browser_profile:"); profile != cred.SourceRef && profile != "" {
					profileID = profile
					break
				}
			}
		}
	}
	id, err := h.DeepSeekBrowserManager().Start(profileID)
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
		writeError(w, status, "unable to start DeepSeek browser login", "provider_error", code)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"success":    true,
		"account_id": id,
		"message":    "Trình duyệt đã mở. Hãy đăng nhập tài khoản DeepSeek trên cửa sổ trình duyệt.",
	})
}

// DeepSeekAutoLoginStatus trả về trạng thái tiến trình đăng nhập tự động.
func (h *Handler) DeepSeekAutoLoginStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	status := h.DeepSeekBrowserManager().Status()
	writeJSON(w, http.StatusOK, status)
}

// DeepSeekAutoLoginStop dừng tiến trình đăng nhập tự động.
func (h *Handler) DeepSeekAutoLoginStop(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	h.DeepSeekBrowserManager().Stop()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Đã dừng trình duyệt đăng nhập DeepSeek."})
}

// DeepSeekAccounts xử lý danh sách, tạo mới và xóa tài khoản DeepSeek Web.
func (h *Handler) DeepSeekAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}

	manager, _ := h.Credentials.(*identity.Manager)
	if !h.requireAdmin(w, r) {
		return
	}

	if r.Method == http.MethodPost {
		if manager == nil {
			writeError(w, http.StatusServiceUnavailable, "identity manager is unavailable", "server_error", "identity_store_unavailable")
			return
		}

		var req struct {
			Email          string `json:"email"`
			Token          string `json:"token"`
			SessionID      string `json:"session_id"`
			MaxConcurrency int    `json:"max_concurrency"`
			IdentityID     string `json:"identity_id"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}

		if req.MaxConcurrency <= 0 {
			req.MaxConcurrency = 2
		}
		if req.MaxConcurrency > 16 {
			writeError(w, http.StatusUnprocessableEntity, "max_concurrency must be between 1 and 16", "invalid_request_error", "invalid_concurrency")
			return
		}

		token := strings.TrimSpace(req.Token)
		if token == "" || len(token) > 4096 {
			writeError(w, http.StatusUnprocessableEntity, "valid DeepSeek user token is required", "invalid_request_error", "invalid_token")
			return
		}

		email := strings.TrimSpace(req.Email)
		identityID := strings.TrimSpace(req.IdentityID)
		if identityID == "" {
			identityID = "deepseek-" + strings.ToLower(email)
		}
		if identityID == "deepseek-" || email == "" {
			writeError(w, http.StatusUnprocessableEntity, "email or account label is required", "invalid_request_error", "missing_identity")
			return
		}

		if err := manager.UpsertIdentity(r.Context(), identity.Identity{
			ID:            identityID,
			ProviderGroup: "deepseek",
			Email:         email,
			Priority:      1,
			GlobalStatus:  identity.IdentityHealthy,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to store identity", "server_error", "identity_import_failed")
			return
		}

		credentialID := identityID + "-deepseek_web"
		secretBytes, _ := json.Marshal(map[string]string{
			"token":      token,
			"session_id": strings.TrimSpace(req.SessionID),
		})

		if err := manager.UpsertCredential(r.Context(), identity.Credential{
			ID:             credentialID,
			IdentityID:     identityID,
			Domain:         "deepseek_web",
			MaxConcurrency: req.MaxConcurrency,
			Status:         identity.StatusHealthy,
		}, secretBytes); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to store DeepSeek credential", "server_error", "credential_import_failed")
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"credential_id": credentialID,
			"identity_id":   identityID,
			"status":        "HEALTHY",
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
		if err != nil || detail.Identity.ProviderGroup != "deepseek" {
			writeError(w, http.StatusNotFound, "DeepSeek identity not found", "invalid_request_error", "account_not_found")
			return
		}

		for _, credential := range detail.Credentials {
			if credential == nil || credential.Domain != "deepseek_web" {
				continue
			}
			if err := manager.DeleteCredential(r.Context(), credential.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to delete DeepSeek credential", "server_error", "credential_delete_failed")
				return
			}
		}

		if err := manager.DeleteIdentity(r.Context(), identityID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete DeepSeek identity", "server_error", "identity_delete_failed")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"deleted_id": identityID,
			"success":    true,
		})
		return
	}

	if h.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store is unavailable", "server_error", "identity_store_unavailable")
		return
	}

	creds, err := h.Credentials.List(r.Context(), "deepseek_web")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list DeepSeek credentials", "server_error", "credential_list_failed")
		return
	}

	items := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		profileID := strings.TrimPrefix(c.SourceRef, "browser_profile:")
		if profileID == c.SourceRef || profileID == "" {
			profileID = c.IdentityID
		}
		items = append(items, map[string]any{
			"id":              c.ID,
			"identity_id":     c.IdentityID,
			"profile_id":      profileID,
			"status":          c.Status,
			"max_concurrency": c.MaxConcurrency,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": items})
}
