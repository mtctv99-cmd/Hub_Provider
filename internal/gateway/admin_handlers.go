package gateway

import (
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/colabsupervisor"
	"sever_provider/internal/session"
)

func (h *Handler) SetAdminAuth(auth *admin.Auth)                     { h.AdminAuth = auth }
func (h *Handler) SetAPIKeys(keys *admin.APIKeys)                    { h.APIKeys = keys }
func (h *Handler) SetAudit(audit *admin.AuditLogger)                 { h.AuditLog = audit }
func (h *Handler) SetColabSupervisor(cs *colabsupervisor.Supervisor) { h.ColabSupervisor = cs }
func (h *Handler) SetSessions(s *session.Manager)                    { h.Sessions = s }

func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if h.AdminAuth == nil || !h.AdminAuth.Configured() {
		writeError(w, http.StatusNotImplemented, "admin authentication is not configured", "server_error", "admin_auth_unavailable")
		return false
	}
	if !h.AdminAuth.IsAuthenticated(r) {
		writeError(w, http.StatusUnauthorized, "admin authentication required", "authentication_error", "admin_auth_required")
		return false
	}
	return true
}

func (h *Handler) AdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if h.AdminAuth == nil || !h.AdminAuth.Configured() {
		writeError(w, 501, "admin authentication is not configured", "server_error", "admin_auth_unavailable")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	token, err := h.AdminAuth.Login(req.Password)
	if err != nil {
		writeError(w, 401, "invalid admin credentials", "authentication_error", "invalid_credentials")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: admin.CookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 86400})
	writeJSON(w, 200, map[string]any{"authenticated": true, "first_login": h.AdminAuth.FirstLoginPending()})
}

func (h *Handler) AdminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if h.AdminAuth != nil {
		h.AdminAuth.Logout(h.AdminAuth.TokenFromRequest(r))
	}
	http.SetCookie(w, &http.Cookie{Name: admin.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]any{"authenticated": false})
}

func (h *Handler) AdminAuthCheck(w http.ResponseWriter, r *http.Request) {
	auth := h.AdminAuth != nil && h.AdminAuth.IsAuthenticated(r)
	configured := h.AdminAuth != nil && h.AdminAuth.Configured()
	firstLogin := false
	if h.AdminAuth != nil {
		firstLogin = h.AdminAuth.FirstLoginPending()
	}
	writeJSON(w, 200, map[string]any{"authenticated": auth, "configured": configured, "first_login": firstLogin})
}

func (h *Handler) AdminChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !h.requireAdmin(w, r) {
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.AdminAuth.ChangePassword(req.CurrentPassword, req.NewPassword); err != nil {
		if errors.Is(err, admin.ErrPasswordPolicy) {
			writeError(w, http.StatusBadRequest, err.Error(), "validation_error", "password_policy")
			return
		}
		if errors.Is(err, admin.ErrInvalidPassword) {
			writeError(w, http.StatusUnauthorized, "mật khẩu hiện tại không chính xác", "authentication_error", "invalid_credentials")
			return
		}
		writeError(w, http.StatusInternalServerError, "không thể cập nhật mật khẩu", "server_error", "password_update_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "password updated successfully"})
}

func (h *Handler) AdminDismissFirstLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !h.requireAdmin(w, r) {
		return
	}
	if err := h.AdminAuth.DismissFirstLoginPrompt(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to dismiss first login prompt", "server_error", "dismiss_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dismissed": true})
}

func (h *Handler) AdminStats(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	status := map[string]any{"uptime_seconds": int(time.Since(h.StartedAt).Seconds()), "requests_total": h.Requests.Load(), "models": len(h.Models.List())}
	if h.Leases != nil {
		status["active_leases"] = h.Leases.ActiveCount()
	}
	if h.Workers != nil {
		status["workers"] = h.Workers.Snapshot()
	}
	writeJSON(w, 200, status)
}

func (h *Handler) AdminSessions(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.Sessions == nil {
		writeJSON(w, 200, map[string]any{"sessions": []any{}})
		return
	}
	writeJSON(w, 200, map[string]any{"sessions": h.Sessions.List()})
}
func (h *Handler) AdminModels(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	h.ModelsList(w, r)
}
func (h *Handler) AdminModelsSync(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	writeError(w, http.StatusNotImplemented, "model sync requires a connected provider backend", "provider_error", "model_sync_unavailable")
}
func (h *Handler) AdminAPIKeys(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.APIKeys == nil {
		writeError(w, 503, "API key store is unavailable", "server_error", "apikey_store_unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		keys, err := h.APIKeys.List()
		if err != nil {
			writeError(w, 500, "failed to list API keys", "server_error", "apikey_list_failed")
			return
		}
		writeJSON(w, 200, map[string]any{"keys": keys})
	case http.MethodPost:
		var req admin.CreateKeySpec
		if !decodeJSON(w, r, &req) {
			return
		}
		meta, plain, err := h.APIKeys.CreateWithOptions(req)
		if errors.Is(err, admin.ErrInvalidKeyOptions) {
			writeError(w, http.StatusUnprocessableEntity, "invalid API key options", "invalid_request_error", "invalid_key_options")
			return
		}
		if err != nil {
			writeError(w, 500, "failed to create API key", "server_error", "apikey_create_failed")
			return
		}
		writeJSON(w, 201, map[string]any{"key": plain, "metadata": meta})
	default:
		methodNotAllowed(w)
	}
}
func (h *Handler) AdminLogs(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.AuditLog == nil {
		writeError(w, http.StatusNotImplemented, "audit log storage is not configured", "server_error", "audit_log_unavailable")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	lines, err := h.AuditLog.Recent(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read audit logs", "server_error", "audit_log_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}
func (h *Handler) AdminLogsClear(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if h.AuditLog == nil {
		writeError(w, http.StatusNotImplemented, "audit log storage is not configured", "server_error", "audit_log_unavailable")
		return
	}
	if err := h.AuditLog.Clear(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear audit logs", "server_error", "audit_log_clear_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}
func (h *Handler) AdminAPIKeyMutation(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.APIKeys == nil {
		writeError(w, 503, "API key store is unavailable", "server_error", "apikey_store_unavailable")
		return
	}
	id := path.Base(strings.TrimSuffix(r.URL.Path, "/"))
	if len(id) != 24 || strings.ContainsAny(id, "/\\.") {
		writeError(w, http.StatusBadRequest, "invalid API key id", "invalid_request_error", "invalid_key_id")
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/admin/api/apikeys/revoke/") && r.Method == http.MethodPost:
		if err := h.APIKeys.Revoke(id); err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"revoked": true, "id": id})
	case strings.HasPrefix(r.URL.Path, "/admin/api/apikeys/activate/") && r.Method == http.MethodPost:
		err := h.APIKeys.Activate(id)
		if errors.Is(err, admin.ErrExpiredAPIKey) {
			writeError(w, http.StatusConflict, "API key has expired", "invalid_request_error", "key_expired")
			return
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"activated": true, "id": id})
	case strings.HasPrefix(r.URL.Path, "/admin/api/apikeys/delete/") && r.Method == http.MethodDelete:
		if err := h.APIKeys.Delete(id); err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
	default:
		methodNotAllowed(w)
	}
}

func (h *Handler) AdminUnsupported(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	writeError(w, 501, "account management requires provider-specific provisioning", "provider_error", "provisioning_unavailable")
}

func adminPathMatch(path string, suffix string) bool { return strings.HasSuffix(path, suffix) }
