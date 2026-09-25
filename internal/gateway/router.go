package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/web"
)

const DefaultMaxRequestBodyBytes int64 = 10 * 1024 * 1024

func NewRouter(h *Handler, apiKey string) http.Handler {
	return NewRouterWithLimits(h, apiKey, DefaultMaxRequestBodyBytes)
}

func NewRouterWithLimits(h *Handler, apiKey string, maxBodyBytes int64) http.Handler {
	return newRouterFull(h, apiKey, h.APIKeys, maxBodyBytes)
}

func newRouterFull(h *Handler, apiKey string, dbKeys *admin.APIKeys, maxBodyBytes int64) http.Handler {
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxRequestBodyBytes
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.Health)
	mux.HandleFunc("/ready", h.Ready)
	mux.HandleFunc("/v1/models", h.ModelsList)
	mux.HandleFunc("/v1/chat/completions", h.ChatCompletions)
	mux.HandleFunc("/v1/audio/speech", h.AudioSpeech)
	mux.HandleFunc("/v1/images/generations", h.Images)
	mux.HandleFunc("/v1/messages", h.AnthropicMessages)
	mux.HandleFunc("/v1/videos/generations", h.VideoGenerations)
	mux.HandleFunc("/v1/video/generations", h.VideoGenerations)
	mux.HandleFunc("/v1/videos/tasks/", h.VideoTask)
	mux.HandleFunc("/v1/files/", h.Files)
	mux.HandleFunc("/api/tts/voices", h.TTSVoices)
	mux.HandleFunc("/api/tts/generate", h.TTSGenerate)
	mux.HandleFunc("/api/tts/synthesize", h.LegacyTTS)
	mux.HandleFunc("/api/tts/clone", h.TTSClone)
	mux.HandleFunc("/api/tts/jobs/", h.TTSJob)
	mux.HandleFunc("/api/llm/jobs", h.LLMJobCreate)
	mux.HandleFunc("/api/llm/jobs/", h.LLMJob)
	mux.HandleFunc("/api/llm/ws/worker", h.WorkerWebSocket)
	mux.HandleFunc("/api/llm/colab/accounts", h.ColabAccounts)
	mux.HandleFunc("/api/llm/colab/rotate", h.AdminUnsupported)
	mux.HandleFunc("/api/llm/colab/start", h.AdminUnsupported)
	mux.HandleFunc("/api/llm/colab/stop", h.AdminUnsupported)
	mux.HandleFunc("/api/tts/tiktok/accounts", h.TikTokAccounts)
	mux.HandleFunc("/admin/api/tts/tiktok/accounts", h.TikTokAccounts)
	mux.HandleFunc("/admin/api/tts/settings", h.TTSSettings)
	mux.HandleFunc("/admin/api/workers", h.AdminWorkers)
	mux.HandleFunc("/admin/api/workers/", h.AdminWorkerAction)
	mux.HandleFunc("/api/tts/tiktok/workers", h.TikTokWorkers)
	mux.HandleFunc("/admin/api/login", h.AdminLogin)
	mux.HandleFunc("/admin/api/logout", h.AdminLogout)
	mux.HandleFunc("/admin/api/auth/check", h.AdminAuthCheck)
	mux.HandleFunc("/admin/api/change-password", h.AdminChangePassword)
	mux.HandleFunc("/admin/api/first-login/dismiss", h.AdminDismissFirstLogin)
	mux.HandleFunc("/admin/api/stats", h.AdminStats)
	mux.HandleFunc("/admin/api/pool/status", h.AdminStats)
	mux.HandleFunc("/admin/api/sessions", h.AdminSessions)
	mux.HandleFunc("/admin/api/models", h.AdminModels)
	mux.HandleFunc("/admin/api/models/sync", h.AdminModelsSync)
	mux.HandleFunc("/admin/api/apikeys", h.AdminAPIKeys)
	mux.HandleFunc("/admin/api/apikeys/add", h.AdminAPIKeys)
	mux.HandleFunc("/admin/api/apikeys/delete/", h.AdminAPIKeyMutation)
	mux.HandleFunc("/admin/api/apikeys/revoke/", h.AdminAPIKeyMutation)
	mux.HandleFunc("/admin/api/apikeys/activate/", h.AdminAPIKeyMutation)
	mux.HandleFunc("/admin/api/apikeys/delete", h.AdminUnsupported)

	mux.HandleFunc("/admin/api/auth/status", h.AdminAuthCheck)
	mux.HandleFunc("/admin/api/auth/toggle", h.AdminUnsupported)
	mux.HandleFunc("/admin/api/logs", h.AdminLogs)
	mux.HandleFunc("/admin/api/logs/clear", h.AdminLogsClear)
	mux.HandleFunc("/admin/api/google/auto-login/status", h.GoogleAutoLoginStatus)
	mux.HandleFunc("/admin/api/google/auto-login", h.GoogleAutoLogin)
	mux.HandleFunc("/admin/api/google/accounts", h.GoogleAccounts)
	mux.HandleFunc("/admin/api/google/accounts/", h.GoogleAccount)
	mux.HandleFunc("/admin/api/google/discover", h.GoogleDiscover)
	mux.HandleFunc("/admin/api/google/import", h.GoogleImport)
	mux.HandleFunc("/admin/api/google/credentials/", h.GoogleCredential)
	mux.HandleFunc("/admin/api/cookies", h.AdminUnsupported)
	mux.HandleFunc("/admin/api/cookies/save", h.AdminUnsupported)
	mux.HandleFunc("/admin/api/cookies/google-key", h.AdminUnsupported)
	mux.HandleFunc("/admin/api/cookies/auto-login", h.AdminUnsupported)
	mux.HandleFunc("/admin/api/cookies/refresh", h.AdminUnsupported)
	mux.HandleFunc("/admin/api/cookies/delete", h.AdminUnsupported)
	mux.HandleFunc("/admin/api/cookies/pool-type", h.AdminUnsupported)
	mux.HandleFunc("/admin/api/copilot/pool-type", h.AdminUnsupported)
	mux.HandleFunc("/admin/api/copilot/status", h.CopilotStatus)
	mux.HandleFunc("/admin/api/copilot/add", h.CopilotImport)
	mux.HandleFunc("/admin/api/copilot/accounts", h.CopilotAccounts)
	mux.HandleFunc("/admin/api/copilot/token", h.CopilotSaveToken)
	mux.HandleFunc("/admin/api/copilot/auto-login", h.CopilotAutoLogin)
	mux.HandleFunc("/admin/api/copilot/auto-login/status", h.CopilotAutoLoginStatus)
	mux.HandleFunc("/admin/api/copilot/auto-login/stop", h.CopilotAutoLoginStop)
		mux.HandleFunc("/admin/api/copilot/refresh", h.AdminUnsupported)
		mux.HandleFunc("/admin/api/copilot/delete", h.CopilotDelete)
		mux.HandleFunc("/admin/api/deepseek/accounts", h.DeepSeekAccounts)
		mux.HandleFunc("/admin/api/deepseek/auto-login", h.DeepSeekAutoLogin)
		mux.HandleFunc("/admin/api/deepseek/auto-login/status", h.DeepSeekAutoLoginStatus)
		mux.HandleFunc("/admin/api/deepseek/auto-login/stop", h.DeepSeekAutoLoginStop)
		mux.HandleFunc("/admin/api/traffic", h.TrafficHistory)
	mux.HandleFunc("/internal/status", h.DashboardStatus)
	mux.HandleFunc("/internal/events", h.Events)
	if staticFS, err := fs.Sub(web.StaticFS, "."); err == nil {
		mux.Handle("/web/", http.StripPrefix("/web", http.FileServer(http.FS(staticFS))))
		mux.HandleFunc("/web", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/web/", http.StatusMovedPermanently)
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/web/", http.StatusFound) })
	}
	return withMiddleware(mux, h, apiKey, dbKeys, maxBodyBytes)
}

func withMiddleware(next http.Handler, h *Handler, apiKey string, dbKeys *admin.APIKeys, maxBodyBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		defer func() {
			if h != nil {
				duration := time.Since(startedAt)
				h.TotalDurationMs.Add(uint64(duration.Milliseconds()))
					if isConsumerTraffic(r.URL.Path) && h.Traffic != nil {
						go func(t time.Time, d time.Duration) {
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							if err := h.Traffic.Record(ctx, t, d); err != nil {
								log.Printf("[GATEWAY] traffic counter write failed: %v", err)
							}
						}(startedAt, duration)
					}
			}
		}()
		w.Header().Set("X-Request-ID", requestID())
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Session-ID, X-API-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		p := r.URL.Path
		if h != nil {
			h.Requests.Add(1)
		}

		// Telemetry protection: internal routes require loopback access, valid admin auth, or valid API key.
		if strings.HasPrefix(p, "/internal/") && !isLoopback(r.RemoteAddr) {
			adminAuth := (h != nil && h.AdminAuth != nil && h.AdminAuth.IsAuthenticated(r))
			keyAuth := false
			candidate := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if candidate == "" {
				candidate = strings.TrimSpace(r.Header.Get("X-API-Key"))
			}
			if candidate != "" {
				if apiKey != "" && candidate == apiKey {
					keyAuth = true
				} else if dbKeys != nil {
					_, err := dbKeys.AuthenticateWithScope(candidate, "internal")
					keyAuth = err == nil
				}
			}
			if !adminAuth && !keyAuth {
				writeError(w, http.StatusUnauthorized, "internal telemetry requires authentication", "authentication_error", "unauthorized")
				return
			}
		}

		// API Key authentication applies only to consumer API endpoints.
		// Admin, internal, health, static files, and inbound worker WebSocket use their own auth mechanisms.
		requiresAPIKey := !strings.HasPrefix(p, "/admin/") &&
			!strings.HasPrefix(p, "/internal/") &&
			!strings.HasPrefix(p, "/web/") &&
			!strings.HasPrefix(p, "/web") &&
			p != "/health" &&
			p != "/ready" &&
			p != "/" &&
			p != "/api/llm/ws/worker"

		if requiresAPIKey {
			candidate := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if candidate == "" {
				candidate = strings.TrimSpace(r.Header.Get("X-API-Key"))
			}

			if apiKey != "" && candidate == apiKey {
				// The configured legacy key remains unrestricted.
			} else if dbKeys != nil && candidate != "" {
				_, err := dbKeys.AuthenticateWithScope(candidate, scopeForPath(p))
				if err != nil {
					status, code := http.StatusUnauthorized, "invalid_api_key"
					switch {
					case errors.Is(err, admin.ErrExpiredAPIKey):
						code = "key_expired"
					case errors.Is(err, admin.ErrInsufficientScope):
						status, code = http.StatusForbidden, "insufficient_scope"
					case errors.Is(err, admin.ErrRateLimit):
						status, code = http.StatusTooManyRequests, "rate_limit_exceeded"
					case !errors.Is(err, admin.ErrInvalidAPIKey):
						status, code = http.StatusServiceUnavailable, "apikey_store_unavailable"
					}
					writeError(w, status, "API key access denied", "authentication_error", code)
					return
				}
			} else if apiKey != "" || (dbKeys != nil && dbKeys.HasAnyKeys()) {
				writeError(w, http.StatusUnauthorized, "invalid API key", "authentication_error", "invalid_api_key")
				return
			}
		}

		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("[GATEWAY] panic request_id=%s path=%s: %v", w.Header().Get("X-Request-ID"), r.URL.Path, recovered)
				if !responseStarted(w) {
					writeError(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func responseStarted(w http.ResponseWriter) bool {
	// net/http does not expose whether a ResponseWriter has written headers.
	// Recovery is therefore intentionally conservative: handlers should not panic after writing.
	return false
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func isConsumerTraffic(p string) bool {
	if p == "/v1/models" || p == "/api/tts/voices" || p == "/api/tts/tiktok/workers" || p == "/api/llm/ws/worker" {
		return false
	}
	return strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/api/tts/") || strings.HasPrefix(p, "/api/llm/")
}

func scopeForPath(p string) string {
	switch {
	case p == "/v1/models":
		return "models"
	case p == "/v1/chat/completions", p == "/v1/messages":
		return "chat"
	case p == "/v1/audio/speech", strings.HasPrefix(p, "/api/tts/"):
		return "audio_speech"
	case p == "/v1/images/generations":
		return "images"
	case strings.HasPrefix(p, "/v1/videos/") || strings.HasPrefix(p, "/v1/video/"):
		return "video"
	case strings.HasPrefix(p, "/api/llm/jobs"):
		return "jobs"
	default:
		return "all"
	}
}

func requestID() string            { return "req-" + strconvFormat(time.Now().UnixNano()) }
func strconvFormat(v int64) string { return fmt.Sprintf("%x", v) }
