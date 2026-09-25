package gateway

import (
	"net/http"
	"strings"

	"sever_provider/internal/provider/piper"
)

func (h *Handler) TTSSettings(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime settings are unavailable", "server_error", "settings_unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		voices := map[string]bool{}
		if h.Dispatcher != nil && h.Dispatcher.Adapters != nil {
			if adapter, ok := h.Dispatcher.Adapters.Get("piper_local"); ok {
				if discovered, ok := adapter.(interface{ Voices() []piper.Voice }); ok {
					for _, voice := range discovered.Voices() {
						voices[voice.ID] = h.Settings.GetBool(r.Context(), "tts.voice."+voice.ID+".enabled", true)
					}
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"piper_mode": h.Settings.Get(r.Context(), "tts.piper_mode", "auto"),
			"voices":     voices,
		})
	case http.MethodPost:
		var req struct {
			PiperMode *string `json:"piper_mode"`
			VoiceID   string  `json:"voice_id"`
			Enabled   *bool   `json:"enabled"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.PiperMode != nil {
			mode := strings.ToLower(strings.TrimSpace(*req.PiperMode))
			if mode != "auto" && mode != "local" && mode != "remote" {
				writeError(w, http.StatusUnprocessableEntity, "piper_mode must be local, remote, or auto", "invalid_request_error", "invalid_piper_mode")
				return
			}
			if err := h.Settings.Set(r.Context(), "tts.piper_mode", mode); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to save Piper mode", "server_error", "settings_write_failed")
				return
			}
		}
		if strings.TrimSpace(req.VoiceID) != "" && req.Enabled != nil {
			voiceID := strings.TrimSpace(req.VoiceID)
			if strings.ContainsAny(voiceID, `/\\`) {
				writeError(w, http.StatusUnprocessableEntity, "invalid voice_id", "invalid_request_error", "invalid_voice_id")
				return
			}
			if err := h.Settings.SetBool(r.Context(), "tts.voice."+voiceID+".enabled", *req.Enabled); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to save voice setting", "server_error", "settings_write_failed")
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": true})
	default:
		methodNotAllowed(w)
	}
}
