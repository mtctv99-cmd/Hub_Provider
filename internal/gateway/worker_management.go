package gateway

import (
	"errors"
	"net/http"
	"strings"

	"sever_provider/internal/colabsupervisor"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/workerhub"
)

func classifyWorker(worker workerhub.WorkerSnapshot) string {
	models := worker.Models
	if len(models) == 0 && worker.Model != "" {
		models = []string{worker.Model}
	}
	if len(models) == 0 {
		return "unknown"
	}
	piperCount := 0
	for _, model := range models {
		if strings.HasPrefix(model, "piper:") {
			piperCount++
		}
	}
	if piperCount == len(models) {
		return "piper"
	}
	if piperCount > 0 {
		return "mixed"
	}
	for _, cap := range worker.Capabilities {
		if cap == "audio_speech" || cap == "tts" {
			return "unknown"
		}
	}
	return "colab"
}

func (h *Handler) AdminWorkers(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	piperWorkers := make([]workerhub.WorkerSnapshot, 0)
	colabWorkers := make([]workerhub.WorkerSnapshot, 0)
	if h.Workers != nil {
		for _, worker := range h.Workers.Snapshot() {
			switch classifyWorker(worker) {
			case "piper":
				piperWorkers = append(piperWorkers, worker)
			case "colab":
				colabWorkers = append(colabWorkers, worker)
			}
		}
	}
	local := struct {
		Ready  bool     `json:"ready"`
		Voices []string `json:"voices"`
	}{Voices: []string{}}
	if h.Dispatcher != nil && h.Dispatcher.Adapters != nil {
		if adapter, ok := h.Dispatcher.Adapters.Get("piper_local"); ok {
			if p, ok := adapter.(interface {
				Ready() bool
				Voices() []piper.Voice
			}); ok {
				local.Ready = p.Ready()
				for _, voice := range p.Voices() {
					local.Voices = append(local.Voices, voice.ID)
				}
			}
		}
	}
	accounts := make([]colabsupervisor.Account, 0)
	available := h.ColabSupervisor != nil && h.ColabSupervisor.CanExecute()
	if h.ColabSupervisor != nil {
		items, err := h.ColabSupervisor.DiscoverAccounts(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "Colab account discovery unavailable", "server_error", "colab_accounts_unavailable")
			return
		}
		accounts = append(accounts, items...)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"piper_local": local, "piper_remote": piperWorkers, "colab_workers": colabWorkers,
		"colab_accounts": accounts, "colab_supervisor_available": available, "tiktok_async_slots": h.TikTokAsyncSlots,
	})
}

func (h *Handler) AdminWorkerAction(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/admin/api/workers/")
	if action == "colab/rotate" {
		if h.ColabSupervisor == nil || !h.ColabSupervisor.CanExecute() {
			writeError(w, http.StatusNotImplemented, "Colab supervisor command is not configured", "provider_error", "colab_supervisor_unavailable")
			return
		}
		if err := h.ColabSupervisor.Rotate(r.Context()); err != nil {
			if errors.Is(err, colabsupervisor.ErrNoAccountAvailable) {
				writeError(w, http.StatusConflict, "no eligible Colab account", "provider_error", "no_colab_accounts_available")
				return
			}
			writeError(w, http.StatusBadGateway, "Colab supervisor command failed", "provider_error", "colab_rotate_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"command_executed": true, "worker_connected": false})
		return
	}
	if action == "colab/start" {
		if h.ColabSupervisor == nil || !h.ColabSupervisor.CanExecute() {
			writeError(w, http.StatusNotImplemented, "Colab supervisor command is not configured", "provider_error", "colab_supervisor_unavailable")
			return
		}
		var req struct {
			AccountID string `json:"account_id"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		accounts, err := h.ColabSupervisor.DiscoverAccounts(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "Colab account discovery unavailable", "server_error", "colab_accounts_unavailable")
			return
		}
		allowed := false
		for _, account := range accounts {
			if account.ID == req.AccountID && account.Status == "READY" {
				allowed = true
				break
			}
		}
		if !allowed {
			writeError(w, http.StatusConflict, "Colab account is not eligible", "invalid_request_error", "colab_account_unavailable")
			return
		}
		if err := h.ColabSupervisor.StartAccount(r.Context(), req.AccountID); err != nil {
			writeError(w, http.StatusBadGateway, "Colab supervisor command failed", "provider_error", "colab_start_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"command_executed": true, "worker_connected": false})
		return
	}
	if action == "colab/stop" {
		if h.ColabSupervisor == nil || !h.ColabSupervisor.CanExecute() {
			writeError(w, http.StatusNotImplemented, "Colab supervisor command is not configured", "provider_error", "colab_supervisor_unavailable")
			return
		}
		var req struct {
			AccountID string `json:"account_id"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.AccountID) == "" {
			writeError(w, http.StatusBadRequest, "account_id is required", "invalid_request_error", "missing_account_id")
			return
		}
		if err := h.ColabSupervisor.StopAccount(r.Context(), req.AccountID); err != nil {
			writeError(w, http.StatusBadGateway, "Colab supervisor command failed", "provider_error", "colab_stop_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"command_executed": true})
		return
	}
	if !strings.HasSuffix(action, "/disconnect") {
		writeError(w, http.StatusNotFound, "worker action not found", "invalid_request_error", "not_found")
		return
	}
	id := strings.TrimSuffix(action, "/disconnect")
	if id == "" || strings.ContainsAny(id, "/\\") || len(id) > 128 || h.Workers == nil {
		writeError(w, http.StatusNotFound, "worker not found", "invalid_request_error", "not_found")
		return
	}
	var target *workerhub.WorkerSnapshot
	for _, worker := range h.Workers.Snapshot() {
		if worker.WorkerID == id {
			selected := worker
			target = &selected
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "worker not found", "invalid_request_error", "not_found")
		return
	}
	if kind := classifyWorker(*target); kind != "piper" && kind != "colab" {
		writeError(w, http.StatusConflict, "worker type cannot be managed", "invalid_request_error", "worker_type_unsupported")
		return
	}
	if target.Active > 0 {
		writeError(w, http.StatusConflict, "worker has active requests", "invalid_request_error", "worker_busy")
		return
	}
	if err := h.Workers.DisconnectIdleWorker(id); err != nil {
		if errors.Is(err, workerhub.ErrWorkerBusy) {
			writeError(w, http.StatusConflict, "worker has active requests", "invalid_request_error", "worker_busy")
			return
		}
		writeError(w, http.StatusNotFound, "worker not found", "invalid_request_error", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disconnected": true, "temporary": true})
}
