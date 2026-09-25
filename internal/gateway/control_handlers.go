package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"sever_provider/internal/colabsupervisor"
	"sever_provider/internal/identity"
)

func (h *Handler) ColabAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	workers := []any{}
	if h.Workers != nil {
		for _, worker := range h.Workers.Snapshot() {
			workers = append(workers, worker)
		}
	}
	accounts := []any{}
	if h.ColabSupervisor != nil {
		items, err := h.ColabSupervisor.DiscoverAccounts(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to discover Colab accounts", "server_error", "colab_account_discovery_failed")
			return
		}
		for _, item := range items {
			accounts = append(accounts, item)
		}
	}
	writeJSON(w, 200, map[string]any{"workers": workers, "accounts": accounts})
}

func (h *Handler) ColabRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if h.ColabSupervisor == nil || !h.ColabSupervisor.Configured() {
		writeError(w, http.StatusNotImplemented, "Colab rotation requires the optional colab supervisor", "provider_error", "colab_supervisor_unavailable")
		return
	}
	if err := h.ColabSupervisor.Rotate(r.Context()); err != nil {
		if errors.Is(err, colabsupervisor.ErrNoAccountAvailable) {
			writeError(w, http.StatusServiceUnavailable, "no eligible Colab accounts available for rotation", "provider_error", "no_colab_accounts_available")
			return
		}
		writeError(w, http.StatusBadGateway, err.Error(), "provider_error", "colab_rotation_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "rotated"})
}

func (h *Handler) ColabStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if h.ColabSupervisor == nil || !h.ColabSupervisor.Configured() {
		writeError(w, http.StatusNotImplemented, "Colab provisioning requires the optional colab supervisor", "provider_error", "colab_supervisor_unavailable")
		return
	}
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	if err := h.ColabSupervisor.StartAccount(r.Context(), accountID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "provider_error", "colab_start_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "started", "account_id": accountID})
}

func (h *Handler) ColabStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	workerID := strings.TrimSpace(r.URL.Query().Get("worker_id"))
	if workerID == "" {
		writeError(w, 400, "worker_id is required", "invalid_request_error", "missing_parameter")
		return
	}
	if h.Workers == nil {
		writeError(w, 503, "worker hub is unavailable", "server_error", "worker_hub_unavailable")
		return
	}
	if err := h.Workers.CloseWorker(workerID); err != nil {
		writeError(w, 404, "worker not found", "invalid_request_error", "not_found")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "stopped", "worker_id": workerID})
}

func (h *Handler) TikTokAccounts(w http.ResponseWriter, r *http.Request) {
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
			writeError(w, 503, "identity manager is unavailable", "server_error", "identity_store_unavailable")
			return
		}
		var req struct {
			IdentityID     string `json:"identity_id"`
			Email          string `json:"email"`
			SessionID      string `json:"session_id"`
			MaxConcurrency int    `json:"max_concurrency"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.MaxConcurrency < 1 || req.MaxConcurrency > 16 {
			writeError(w, http.StatusUnprocessableEntity, "max_concurrency must be between 1 and 16", "invalid_request_error", "invalid_concurrency")
			return
		}
		sessionID := strings.TrimSpace(req.SessionID)

		if sessionID == "" || len(sessionID) > 512 {
			writeError(w, 422, "valid TikTok session_id is required", "invalid_request_error", "invalid_session")
			return
		}
		identityID := strings.TrimSpace(req.IdentityID)
		if identityID == "" {
			identityID = "tiktok-" + strings.ToLower(strings.TrimSpace(req.Email))
		}
		if identityID == "tiktok-" || strings.TrimSpace(req.Email) == "" {
			writeError(w, 422, "email is required", "invalid_request_error", "missing_identity")
			return
		}
		if err := manager.UpsertIdentity(r.Context(), identity.Identity{ID: identityID, ProviderGroup: "tiktok", Email: req.Email, Priority: 1, GlobalStatus: identity.IdentityHealthy}); err != nil {
			writeError(w, 500, "failed to store identity", "server_error", "identity_import_failed")
			return
		}
		credentialID := identityID + "-tiktok_api"
		secret, _ := json.Marshal(map[string]string{"session_id": sessionID})
		if err := manager.UpsertCredential(r.Context(), identity.Credential{ID: credentialID, IdentityID: identityID, Domain: "tiktok_api", MaxConcurrency: req.MaxConcurrency}, secret); err != nil {
			writeError(w, 500, "failed to store TikTok credential", "server_error", "credential_import_failed")
			return
		}
		if h.TikTokPool != nil {
			h.TikTokPool.AddSession(sessionID)
		}
		writeJSON(w, 201, map[string]any{"credential_id": credentialID, "identity_id": identityID, "status": "HEALTHY"})
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
		if err != nil || detail.Identity.ProviderGroup != "tiktok" {
			writeError(w, http.StatusNotFound, "TikTok identity not found", "invalid_request_error", "account_not_found")
			return
		}
		var sessionID string
		for _, credential := range detail.Credentials {
			if credential == nil || credential.Domain != "tiktok_api" {
				continue
			}
			secretBytes, readErr := manager.ReadSecret(r.Context(), credential.ID)
			if readErr == nil {
				var payload struct {
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal(secretBytes, &payload) == nil {
					sessionID = payload.SessionID
				}
			}
			if err := manager.DeleteCredential(r.Context(), credential.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to delete TikTok credential", "server_error", "credential_delete_failed")
				return
			}
		}
		if err := manager.DeleteIdentity(r.Context(), identityID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete TikTok identity", "server_error", "identity_delete_failed")
			return
		}
		if h.TikTokPool != nil && sessionID != "" {
			h.TikTokPool.RemoveSession(sessionID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted_id": identityID, "success": true})
		return
	}
	if h.Credentials == nil {
		writeError(w, 503, "identity store is unavailable", "server_error", "identity_store_unavailable")
		return
	}
	creds, err := h.Credentials.List(r.Context(), "tiktok_api")
	if err != nil {
		writeError(w, 500, "failed to list TikTok credentials", "server_error", "credential_list_failed")
		return
	}
	items := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		items = append(items, map[string]any{"id": c.ID, "identity_id": c.IdentityID, "status": c.Status, "max_concurrency": c.MaxConcurrency})
	}
	writeJSON(w, 200, map[string]any{"accounts": items})
}

func (h *Handler) TTSClone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	writeError(w, http.StatusNotImplemented, "OmniVoice clone is disabled; use Piper or TikTok TTS", "provider_error", "omnivoice_disabled")
}

func (h *Handler) TikTokWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if r.Method == http.MethodPost {
		writeError(w, http.StatusNotImplemented, "TikTok worker scaling is managed by the unified scheduler", "provider_error", "worker_scaling_unavailable")
		return
	}
	desired, active := 0, 0
	if h.Credentials != nil {
		if creds, err := h.Credentials.List(r.Context(), "tiktok_api"); err == nil {
			for _, c := range creds {
				if c.Status == identity.StatusHealthy || c.Status == identity.StatusDegraded {
					desired += c.MaxConcurrency
				}
			}
		}
	}
	if desired > 32 {
		desired = 32
	}
	if desired > 0 {
		active = desired
	}
	writeJSON(w, 200, map[string]any{"desired": desired, "active": active, "max": 32})
}
