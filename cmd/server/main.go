package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sever_provider/internal/admin"
	"sever_provider/internal/browser"
	"sever_provider/internal/colabsupervisor"
	"sever_provider/internal/config"
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
	"sever_provider/internal/provider/gemini"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/provider/tiktok"
	"sever_provider/internal/scheduler"
	"sever_provider/internal/secret"
	"sever_provider/internal/session"
	"sever_provider/internal/settings"
	"sever_provider/internal/workerhub"
)

func main() {
	configDir := flag.String("config", "config", "config directory")
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
		_ = models.Register(scheduler.ModelSpec{ID: id, Provider: m.Provider, Capabilities: caps, FailoverTo: append([]string(nil), m.FailoverTo...), TimeoutSec: m.TimeoutSec, GuestAllowed: m.Guest})
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

	secretStore, err := secret.NewStore(os.Getenv("MASTER_KEY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "secret store init failed: %v\n", err)
		return
	}
	database, err := db.Open(context.Background(), cfg.Server.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database init failed: %v\n", err)
		return
	}
	defer database.Close()
	identityManager := identity.NewManager(database.DB, secretStore)
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
	sessions := session.New(session.DefaultMaxTurns, session.DefaultIdleTTL)
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
		fmt.Fprintf(os.Stderr, "admin auth init failed: %v\n", err)
		return
	}
	handler.SetAdminAuth(adminAuth)
	handler.SetAPIKeys(admin.NewAPIKeys(database.DB))
	handler.SetAudit(admin.NewAuditLogger(database.DB))
	handler.Traffic = gateway.NewTrafficStore(database.DB)
	colabTokenDir := os.Getenv("COLAB_TOKEN_DIR")
	if colabTokenDir == "" {
		colabTokenDir = os.Getenv("HOME") + "/.config/colab-cli"
	}
	colabSupervisor := colabsupervisor.New(database.DB, colabsupervisor.Config{TokenDir: colabTokenDir, Command: os.Getenv("COLAB_SUPERVISOR_CMD")})
	handler.SetColabSupervisor(colabSupervisor)
	browserMgr := browser.NewManager("data/profiles", func(accountID, email, cookieStr, profileDir string) error {
		secret, _ := json.Marshal(map[string]string{"cookie": cookieStr})
		specs := []identity.GoogleCredentialSpec{{
			Domain:         "gemini_web",
			MaxConcurrency: 4,
			PoolRole:       identity.PoolAuto,
			SourceRef:      "browser_profile:" + accountID,
			Secret:         secret,
		}}
		_, err := identityManager.UpsertGoogleAccount(context.Background(), email, 1, specs)
		return err
	})
	handler.SetBrowserManager(browserMgr)
	colabSupervisor.Start(reclaimCtx)
	handler.SetSessions(sessions)
	tiktokConcurrency := 4
	if value, convErr := strconv.Atoi(os.Getenv("TIKTOK_WORKER_COUNT")); convErr == nil && value > 0 {
		tiktokConcurrency = value
	}
	handler.TikTokAsyncSlots = tiktokConcurrency
	runner := &jobs.Runner{Jobs: jobStore, Dispatcher: dispatcher, Workers: workers, Files: mediaStore, TikTokConcurrency: tiktokConcurrency, Settings: runtimeSettings}
	runner.Start(reclaimCtx)
	supervisor := &jobs.Supervisor{Jobs: jobStore, Files: mediaStore, Sessions: sessions}
	supervisor.Start(reclaimCtx)
	router := gateway.NewRouterWithLimits(handler, cfg.Server.APIKey, cfg.Server.MaxRequestBodyBytes)
		server := &http.Server{Addr: fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port), Handler: router, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 5 * time.Minute}
		var secondaryServers []*http.Server
		if os.Getenv("DISABLE_LEGACY_PORTS") != "true" {
			legacyPorts := []int{8081, 8090, 8095}
			for _, port := range legacyPorts {
				if port == cfg.Server.Port {
					continue
				}
				secServer := &http.Server{Addr: fmt.Sprintf("%s:%d", cfg.Server.Host, port), Handler: router, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 5 * time.Minute}
				secondaryServers = append(secondaryServers, secServer)
				go func(s *http.Server, p int) {
					if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						fmt.Fprintf(os.Stderr, "legacy port %d note: %v\n", p, err)
					}
				}(secServer, port)
			}
		}
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
			return
		case <-stop:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Server.ShutdownGraceSec)*time.Second)
		defer cancel()
		for _, s := range secondaryServers {
			_ = s.Shutdown(ctx)
		}
		_ = server.Shutdown(ctx)
}
