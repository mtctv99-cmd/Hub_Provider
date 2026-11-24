package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/browser"
	"sever_provider/internal/buildinfo"
	"sever_provider/internal/colabsupervisor"
	"sever_provider/internal/config"
	"sever_provider/internal/copilotrefresh"
	"sever_provider/internal/db"
	"sever_provider/internal/filestore"
	"sever_provider/internal/gateway"
	"sever_provider/internal/identity"
	"sever_provider/internal/jobs"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/pool"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/colab"
	"sever_provider/internal/provider/copilot"
	"sever_provider/internal/provider/deepseek"
	"sever_provider/internal/provider/gemini"
	"sever_provider/internal/provider/meta"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/provider/tiktok"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/secret"
	"sever_provider/internal/session"
	"sever_provider/internal/settings"
	"sever_provider/internal/workerhub"
)

func main() {
	log.Printf("Sever Provider version=%s commit=%s starting", buildinfo.Version, buildinfo.Commit)
	configDir := flag.String("config", "config", "config directory")
	refreshProfiles := flag.String("refresh-google-profiles", "", "refresh only the comma-separated Google profile IDs, then exit")
	flag.Parse()
	cfg, err := config.Load(*configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		return
	}
	if len(cfg.Models) == 0 {
		fmt.Fprintf(os.Stderr, "warning: no models loaded from %s/models.yaml\n", *configDir)
	}
	models := scheduler.NewModelRegistry()
	for id, m := range cfg.Models {
		caps := make([]provider.Capability, 0, len(m.Capabilities))
		for _, name := range m.Capabilities {
			caps = append(caps, provider.Capability(name))
		}
		spec := scheduler.ModelSpec{ID: id, Provider: m.Provider, Capabilities: caps, FailoverTo: append([]string(nil), m.FailoverTo...), TimeoutSec: m.TimeoutSec, GuestAllowed: m.Guest, BaseModel: m.BaseModel, ImageMode: m.ImageMode}
		if spec.ImageMode {
			// Effort aliases would point at this ID and defeat BaseModel.
			_ = models.Register(spec)
			continue
		}
		_ = models.RegisterWithReasoningEffortAliases(spec)
	}
	registry := provider.NewRegistry()
	piperAdapter := piper.New(os.Getenv("PIPER_BIN"), os.Getenv("PIPER_MODELS_DIR"))
	registry.Register(piperAdapter)
	tiktokPool := tiktok.NewPool(nil)
	registry.Register(tiktok.New(nil, tiktokPool))
	geminiAdapter := gemini.New(nil)
	registry.Register(geminiAdapter)
	registry.Register(copilot.New(nil))
	registry.Register(colab.New(colab.Config{URL: os.Getenv("COLAB_WS_URL"), WorkerID: os.Getenv("COLAB_WORKER_ID"), APIKey: os.Getenv("COLAB_API_KEY")}))
	registry.Register(deepseek.New(nil))
	registry.Register(meta.New(nil))

	masterKey := os.Getenv("MASTER_KEY")
	if masterKey == "" {
		masterKey = cfg.Server.MasterKey
	}
	masterKey = strings.TrimSpace(masterKey)
	if masterKey == "" {
		// P0-1 fail-fast: no master key = ephemeral encryption. Refuse to start
		// instead of scrambling the DB with un-decryptable secrets across restarts.
		fmt.Fprintf(os.Stderr, "MASTER_KEY is required (env MASTER_KEY or server.master_key)\n")
		os.Exit(2)
	}
	secretStore, err := secret.NewStore(masterKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "secret store init failed: %v\n", err)
		return
	}
	log.Printf("[startup] MASTER_KEY configured, persistent secret store initialized (key_len=%d)", len(masterKey))
	database, err := db.Open(context.Background(), cfg.Server.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database init failed: %v\n", err)
		return
	}
	defer database.Close()
	// H7 fail-fast: binding a public interface with no static key and an empty
	// api_keys table would leave every consumer endpoint unauthenticated.
	// Existing deployments with DB-managed keys keep working unchanged.
	apiKeys := admin.NewAPIKeys(database.DB)
	if strings.TrimSpace(cfg.Server.APIKey) == "" {
		keyCount, countErr := countAPIKeys(context.Background(), database.DB)
		if countErr != nil {
			fmt.Fprintf(os.Stderr, "refusing to start: server.api_key is empty and api_keys could not be verified: %v\n", countErr)
			os.Exit(1)
		}
		if keyCount == 0 {
			fmt.Fprintln(os.Stderr, "refusing to start: server.api_key is empty and the api_keys table has no rows; set server.api_key or create an API key so consumer endpoints are not unauthenticated")
			os.Exit(1)
		}
		log.Printf("[startup] server.api_key is empty; consumer auth relies on %d key(s) in the api_keys table", keyCount)
	}
	identityManager := identity.NewManager(database.DB, secretStore)
	if err := warnUndecryptableGoogleSecrets(context.Background(), identityManager); err != nil {
		log.Printf("[startup] google secret self-check failed: %v", err)
	}
	if *refreshProfiles != "" {
		if err := refreshSelectedGoogleProfiles(context.Background(), identityManager, geminiAdapter, *refreshProfiles); err != nil {
			log.Printf("profile refresh failed: %v", err)
			_ = database.Close()
			os.Exit(1)
		}
		return
	}
	runtimeSettings := settings.New(database.DB)
	if creds, listErr := identityManager.List(context.Background(), "tiktok_api"); listErr == nil {
		for _, cred := range creds {
			if secretBytes, secretErr := identityManager.ReadSecret(context.Background(), cred.ID); secretErr == nil {
				var payload struct {
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal(secretBytes, &payload) == nil && strings.TrimSpace(payload.SessionID) != "" {
					tiktokPool.Add(payload.SessionID)
				}
			}
		}
	}
	leases := scheduler.NewLeaseManager()
	reclaimCtx, stopReclaim := context.WithCancel(context.Background())
	defer stopReclaim()
	interval := time.Duration(cfg.Server.LeaseReclaimInterval) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-reclaimCtx.Done():
				return
			case <-ticker.C:
				leases.ReclaimExpired()
			}
		}
	}()
	breakers := scheduler.NewBreakerRegistry()
	sessions, err := session.NewPersistent(database.DB)
	if err != nil {
		log.Fatalf("initialize persistent chat sessions: %v", err)
	}
	pools := pool.New()
	dispatcher := &scheduler.Dispatcher{Models: models, Credentials: identityManager, Adapters: registry, Leases: leases, Sessions: sessions, Pools: pools, Breakers: breakers, Settings: runtimeSettings}
	handler := gateway.NewHandler(dispatcher, models)
	handler.SetGeminiValidator(geminiAdapter)
	handler.SetCredentials(identityManager)
	handler.SetTikTokPool(tiktokPool)
	handler.SetIdentityDetails(identityManager)
	handler.SetLeases(leases)
	handler.SetBreakers(breakers)
	handler.SetSettings(runtimeSettings)
	jobStore := jobstore.New(database.DB)
	_ = jobStore.EnsureSchema(context.Background())
	handler.SetJobs(jobStore)
	var mediaStore *filestore.Store
	if media, mediaErr := filestore.New("data/files"); mediaErr == nil {
		mediaStore = media
		handler.SetFiles(media)
	}
	workerToken := os.Getenv("COLAB_WORKER_KEY")
	if workerToken == "" {
		workerToken = os.Getenv("WORKER_AUTH_TOKEN")
	}
	workers := workerhub.New(workerhub.Config{AuthToken: workerToken})
	defer workers.Close()
	handler.SetWorkers(workers)
	adminAuth, err := admin.NewFromEnvWithDB(database.DB)
	if err != nil {
		if errors.Is(err, admin.ErrNotConfigured) {
			fmt.Fprintln(os.Stderr, "admin auth is not configured: set ADMIN_PASSWORD to a strong password and restart")
		} else {
			fmt.Fprintf(os.Stderr, "admin auth init failed: %v\n", err)
		}
		os.Exit(2)
	}
	if adminAuth.UsesDefaultPassword() {
		// Never serve a deployment that still accepts the shipped default password.
		envPassword := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))
		if envPassword == "" {
			fmt.Fprintln(os.Stderr, "admin auth still uses the default password: set ADMIN_PASSWORD and restart")
			os.Exit(2)
		}
		if err := adminAuth.ApplyEnvPassword(envPassword); err != nil {
			fmt.Fprintf(os.Stderr, "admin password bootstrap failed: %v\n", err)
			os.Exit(2)
		}
		log.Printf("[startup] replaced default admin password from ADMIN_PASSWORD; change it at the next login")
	}
	handler.SetAdminAuth(adminAuth)
	handler.SetAPIKeys(apiKeys)
	handler.SetAudit(admin.NewAuditLogger(database.DB))
	handler.Traffic = gateway.NewTrafficStore(database.DB)
	// Deferred after database.Close (LIFO), so the final traffic flush still sees an open DB.
	defer handler.Traffic.Close()
	colabTokenDir := os.Getenv("COLAB_TOKEN_DIR")
	if colabTokenDir == "" {
		colabTokenDir = os.Getenv("HOME") + "/.config/colab-cli"
	}
	colabSupervisor := colabsupervisor.New(database.DB, colabsupervisor.Config{
		TokenDir: colabTokenDir,
		Command:  os.Getenv("COLAB_SUPERVISOR_CMD"),
	})
	handler.SetColabSupervisor(colabSupervisor)
	browserMgr := browser.NewManager("data/profiles", func(accountID, email, cookieStr, profileDir string) error {
		validateCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := geminiAdapter.ValidateCookie(validateCtx, cookieStr); err != nil {
			log.Printf("[AutoLogin] captured cookie for %s (%s) failed upstream validation: %v", email, accountID, err)
			return fmt.Errorf("upstream cookie validation failed: %w", err)
		}
		secretBytes, err := json.Marshal(map[string]string{"cookie": cookieStr})
		if err != nil {
			return fmt.Errorf("marshal cookie secret: %w", err)
		}
		specs := []identity.GoogleCredentialSpec{{
			Domain:         "gemini_web",
			Status:         identity.StatusHealthy,
			MaxConcurrency: 4,
			PoolRole:       identity.PoolAuto,
			SourceRef:      "browser_profile:" + accountID,
			Secret:         secretBytes,
		}}
		if _, err := identityManager.UpsertGoogleAccount(context.Background(), email, 1, specs); err != nil {
			return fmt.Errorf("upsert google account: %w", err)
		}
		_ = identityManager.LinkSharedSourceRef(context.Background(), email, "browser_profile:"+accountID)
		log.Printf("[AutoLogin] captured, validated upstream and marked HEALTHY for %s (profile=%s)", email, accountID)
		return nil
	})
	handler.SetBrowserManager(browserMgr)
	// ponytail: sequential profile refresh. Upgrade to worker pool if profile count > 20.
	go func() {
		retryAfter := make(map[string]time.Time)
		failures := make(map[string]int)
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-reclaimCtx.Done():
				return
			case <-ticker.C:
				details, err := identityManager.ListIdentityDetails(context.Background(), "google")
				if err != nil {
					log.Printf("[AutoRefresh] list google identities failed: %v", err)
					continue
				}
				for _, detail := range details {
					for _, cred := range detail.Credentials {
						if cred == nil || cred.Domain != "gemini_web" {
							continue
						}
						const prefix = "browser_profile:"
						if !strings.HasPrefix(cred.SourceRef, prefix) {
							continue
						}
						profileID := strings.TrimPrefix(cred.SourceRef, prefix)
						if profileID == "" {
							continue
						}
						if time.Now().Before(retryAfter[profileID]) {
							continue
						}
						cookieStr, err := refreshGoogleCredential(reclaimCtx, browserMgr, identityManager, geminiAdapter, cred, profileID)
						if err != nil {
							failures[profileID]++
							delay := time.Hour << min(failures[profileID]-1, 3)
							retryAfter[profileID] = time.Now().Add(delay)
							if errors.Is(err, gemini.ErrCookieInvalidOrExpired) {
								_ = identityManager.SetStatus(reclaimCtx, cred.ID, identity.StatusAuthRequired, nil, false)
							}
							log.Printf("[AutoRefresh] profile=%s refresh failed; retry_after=%s: %v", profileID, retryAfter[profileID].Format(time.RFC3339), err)
							continue
						}
						delete(failures, profileID)
						delete(retryAfter, profileID)
						secretBytes, err := json.Marshal(map[string]string{"cookie": cookieStr})
						if err != nil {
							log.Printf("[AutoRefresh] marshal secret for %s failed: %v", profileID, err)
							continue
						}
						priority := detail.Identity.Priority
						if priority <= 0 {
							priority = 1
						}
						maxConcurrency := cred.MaxConcurrency
						if maxConcurrency <= 0 {
							maxConcurrency = 4
						}
						specs := []identity.GoogleCredentialSpec{{
							Domain:         "gemini_web",
							ID:             cred.ID,
							MaxConcurrency: maxConcurrency,
							PoolRole:       cred.PoolRole,
							SourceRef:      cred.SourceRef,
							Fingerprint:    cred.Fingerprint,
							Secret:         secretBytes,
						}}
						if _, err := identityManager.UpsertGoogleAccount(context.Background(), detail.Identity.Email, priority, specs); err != nil {
							log.Printf("[AutoRefresh] upsert google account %s failed: %v", detail.Identity.Email, err)
						}
					}
				}
			}
		}
	}()
	// Copilot Chathub tokens live ~1h; re-capture from the signed-in profile
	// before they expire so an idle pool never flips to AUTH_REQUIRED.
	// ponytail: sequential capture (one headless Chrome at a time).
	go func() {
		run := func() {
			n, err := copilotrefresh.Refresh(reclaimCtx, identityManager, "data/profiles", 20*time.Minute)
			if n > 0 || err != nil {
				log.Printf("[CopilotRefresh] done: refreshed=%d err=%v", n, err)
			}
		}
		run() // a restart usually means some stored tokens are already stale
		// Copilot only re-mints a Chathub token once the old one expires, so a
		// tight tick keeps the post-expiry gap to a few minutes.
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-reclaimCtx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	colabSupervisor.Start(reclaimCtx)
	handler.SetSessions(sessions)
	tiktokConcurrency := 4
	if value, convErr := strconv.Atoi(os.Getenv("TIKTOK_WORKER_COUNT")); convErr == nil && value > 0 {
		tiktokConcurrency = value
	}
	handler.TikTokAsyncSlots = tiktokConcurrency
	runner := &jobs.Runner{Jobs: jobStore, Dispatcher: dispatcher, Workers: workers, Files: mediaStore, TikTokConcurrency: tiktokConcurrency, Settings: runtimeSettings}
	runner.Start(reclaimCtx)
	supervisor := &jobs.Supervisor{Jobs: jobStore, Files: mediaStore, Sessions: sessions, VideoTimeout: runner.VideoTimeout}
	supervisor.Start(reclaimCtx)
	router := gateway.NewRouterWithLimits(handler, cfg.Server.APIKey, cfg.Server.MaxRequestBodyBytes)
	// WriteTimeout intentionally stays disabled (zero): it is an absolute
	// connection deadline, not per-request, and Gemini streams can run 900s
	// while large downloads take even longer. Upstream stream clients keep
	// their own deadlines.
	server := &http.Server{Addr: fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port), Handler: router, ReadHeaderTimeout: 10 * time.Second}
	server.RegisterOnShutdown(func() { _ = workers.Close() })
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErr:
		fmt.Fprintf(os.Stderr, "server listen failed: %v\n", err)
	case <-stop:
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Server.ShutdownGraceSec)*time.Second)
	defer cancel()
	// M11: stop the job claim loop before draining HTTP. In-flight jobs observe
	// the canceled parent and return to PENDING instead of FAILED.
	stopReclaim()
	if err := runner.Wait(ctx); err != nil {
		log.Printf("[shutdown] runner did not drain within grace period: %v", err)
	}
	// WebSockets are hijacked connections that http.Server.Shutdown does not
	// wait for. Close the hub explicitly after jobs have been requeued; the
	// registered callback keeps the same guarantee if Shutdown runs first.
	if err := workers.Close(); err != nil {
		log.Printf("[shutdown] close worker hub: %v", err)
	}
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[shutdown] server shutdown: %v", err)
	}
}

// countAPIKeys returns the number of rows in api_keys. A query failure is
// reported to the caller so startup can fail closed instead of serving an
// unauthenticated deployment.
func countAPIKeys(ctx context.Context, db *sql.DB) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count api_keys: %w", err)
	}
	return count, nil
}

// warnUndecryptableGoogleSecrets logs credential IDs whose stored secrets can't
// be decrypted with the current MASTER_KEY. No secret material logged.
func warnUndecryptableGoogleSecrets(ctx context.Context, manager *identity.Manager) error {
	details, err := manager.ListIdentityDetails(ctx, "google")
	if err != nil {
		return err
	}
	for _, detail := range details {
		for _, cred := range detail.Credentials {
			if cred == nil || cred.Domain != "gemini_web" {
				continue
			}
			if _, readErr := manager.ReadSecret(ctx, cred.ID); readErr != nil {
				log.Printf("[WARNING] google credential %s cannot be decrypted with current MASTER_KEY (%v); refusing auto-overwrite (P0-1); re-login via validated browser flow required", cred.ID, readErr)
			}
		}
	}
	return nil
}
