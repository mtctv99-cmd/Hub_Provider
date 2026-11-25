// Package tiktokhealth revalidates TikTok TTS credentials that the scheduler
// marked DEGRADED. The scheduler orders HEALTHY credentials first and only
// retries a degraded account once healthy capacity is exhausted, so a
// transient upstream failure can strand a working session outside rotation
// indefinitely (observed 2026-11-25: 3 valid accounts idle for 8 days).
package tiktokhealth

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"sever_provider/internal/identity"
)

// CredentialStore is the subset of identity.Manager the revalidator needs.
type CredentialStore interface {
	List(ctx context.Context, domain string) ([]identity.Credential, error)
	ReadSecret(ctx context.Context, id string) ([]byte, error)
	MarkSuccess(ctx context.Context, id string) error
}

// Prober verifies a TikTok session against the upstream speech endpoint.
type Prober interface {
	Probe(ctx context.Context, sessionID string) error
}

// Config controls the revalidation loop. Zero values select defaults.
type Config struct {
	Interval     time.Duration
	StartupDelay time.Duration
	Timeout      time.Duration
	MaxPerCycle  int
	Spacing      time.Duration
	Logf         func(format string, args ...any)
}

const (
	defaultInterval     = 15 * time.Minute
	defaultStartupDelay = 30 * time.Second
	defaultTimeout      = 10 * time.Second
	defaultMaxPerCycle  = 3
	defaultSpacing      = time.Second
)

// Run revalidates DEGRADED credentials once after StartupDelay and then every
// Interval until ctx is canceled. Successful probes call MarkSuccess, the same
// path a normal request uses to clear DEGRADED.
func Run(ctx context.Context, store CredentialStore, prober Prober, cfg Config) {
	if store == nil || prober == nil {
		return
	}
	cfg = withDefaults(cfg)
	if !sleep(ctx, cfg.StartupDelay) {
		return
	}
	for {
		checked, reset := revalidate(ctx, store, prober, cfg)
		if checked > 0 {
			cfg.Logf("[TikTokHealth] checked=%d reset=%d", checked, reset)
		}
		if !sleep(ctx, cfg.Interval) {
			return
		}
	}
}

// revalidate probes up to MaxPerCycle DEGRADED credentials. The store orders
// credentials by last use, so the accounts most likely to have been idle
// longest are rechecked first.
func revalidate(ctx context.Context, store CredentialStore, prober Prober, cfg Config) (checked, reset int) {
	creds, err := store.List(ctx, "tiktok_api")
	if err != nil {
		cfg.Logf("[TikTokHealth] list credentials failed: %v", err)
		return 0, 0
	}
	for _, cred := range creds {
		if cred.Status != identity.StatusDegraded {
			continue
		}
		if checked >= cfg.MaxPerCycle || ctx.Err() != nil {
			break
		}
		if checked > 0 && !sleep(ctx, cfg.Spacing) {
			break
		}
		secretBytes, err := store.ReadSecret(ctx, cred.ID)
		if err != nil {
			cfg.Logf("[TikTokHealth] credential=%s secret unavailable: %v", cred.ID, err)
			continue
		}
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(secretBytes, &payload); err != nil || strings.TrimSpace(payload.SessionID) == "" {
			cfg.Logf("[TikTokHealth] credential=%s has no usable session_id", cred.ID)
			continue
		}
		checked++
		attemptCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		probeErr := prober.Probe(attemptCtx, payload.SessionID)
		cancel()
		if probeErr != nil {
			cfg.Logf("[TikTokHealth] credential=%s probe failed: %v", cred.ID, probeErr)
			continue
		}
		if err := store.MarkSuccess(ctx, cred.ID); err != nil {
			cfg.Logf("[TikTokHealth] credential=%s probe ok but status update failed: %v", cred.ID, err)
			continue
		}
		reset++
		cfg.Logf("[TikTokHealth] credential=%s probe ok; reset to HEALTHY", cred.ID)
	}
	return checked, reset
}

func withDefaults(cfg Config) Config {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.StartupDelay <= 0 {
		cfg.StartupDelay = defaultStartupDelay
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxPerCycle <= 0 {
		cfg.MaxPerCycle = defaultMaxPerCycle
	}
	if cfg.Spacing <= 0 {
		cfg.Spacing = defaultSpacing
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return cfg
}

// sleep waits for d, returning false when ctx is canceled first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
