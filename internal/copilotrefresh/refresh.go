package copilotrefresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"sever_provider/internal/browser"
	"sever_provider/internal/identity"
	"sever_provider/internal/provider/copilot"
)

// Terminal profiles (AUTH_REQUIRED) used to launch headless Chrome on every
// 5-minute tick and log once per tick (M2). Retry them with exponential
// backoff instead, and report a given profile at most once per day.
const (
	refreshBaseBackoff = 30 * time.Minute
	refreshMaxBackoff  = 6 * time.Hour
	refreshLogInterval = 24 * time.Hour
)

type refreshBackoff struct {
	failures int
	next     time.Time
	lastLog  time.Time
}

var refreshBackoffs = struct {
	sync.Mutex
	entries map[string]*refreshBackoff
}{entries: make(map[string]*refreshBackoff)}

func refreshBackoffDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := refreshBaseBackoff
	for i := 1; i < failures && delay < refreshMaxBackoff; i++ {
		delay *= 2
	}
	if delay > refreshMaxBackoff {
		delay = refreshMaxBackoff
	}
	return delay
}

// refreshDue reports whether a credential may be re-captured now, the time of
// the next allowed attempt, and whether the skip should be logged (once/day).
// force bypasses backoff entirely (manual admin refresh, lead < 0).
func refreshDue(credentialID string, status identity.CredentialStatus, force bool, now time.Time) (bool, time.Time, bool) {
	if force {
		return true, time.Time{}, false
	}
	refreshBackoffs.Lock()
	defer refreshBackoffs.Unlock()
	state, ok := refreshBackoffs.entries[credentialID]
	if !ok {
		state = &refreshBackoff{}
		refreshBackoffs.entries[credentialID] = state
	}
	if state.failures == 0 && status != identity.StatusAuthRequired {
		return true, time.Time{}, false
	}
	if state.failures == 0 {
		// First terminal observation: schedule a probe instead of launching
		// Chrome on this tick.
		state.failures = 1
		state.next = now.Add(refreshBackoffDelay(1))
	}
	if now.Before(state.next) {
		logSkip := now.Sub(state.lastLog) >= refreshLogInterval
		if logSkip {
			state.lastLog = now
		}
		return false, state.next, logSkip
	}
	return true, state.next, false
}

// refreshFailed records a failed attempt and reports whether it should be
// surfaced (first failure after success, or once per day per credential).
func refreshFailed(credentialID string, now time.Time) bool {
	refreshBackoffs.Lock()
	defer refreshBackoffs.Unlock()
	state, ok := refreshBackoffs.entries[credentialID]
	if !ok {
		state = &refreshBackoff{}
		refreshBackoffs.entries[credentialID] = state
	}
	state.failures++
	state.next = now.Add(refreshBackoffDelay(state.failures))
	report := state.failures == 1 || now.Sub(state.lastLog) >= refreshLogInterval
	if report {
		state.lastLog = now
	}
	return report
}

// clearRefreshBackoff forgets a profile's failure history after a successful
// capture or when the stored token is still valid.
func clearRefreshBackoff(credentialID string) {
	refreshBackoffs.Lock()
	delete(refreshBackoffs.entries, credentialID)
	refreshBackoffs.Unlock()
}

// resetRefreshBackoffs exists for tests that exercise the backoff state.
func resetRefreshBackoffs() {
	refreshBackoffs.Lock()
	refreshBackoffs.entries = make(map[string]*refreshBackoff)
	refreshBackoffs.Unlock()
}

// Refresh re-captures Chathub tokens for Copilot credentials whose
// token expires within lead, using their stored signed-in Chrome profile.
func Refresh(ctx context.Context, manager *identity.Manager, profilesDir string, lead time.Duration) (int, error) {
	details, err := manager.ListIdentityDetails(ctx, "copilot")
	if err != nil {
		return 0, err
	}
	const prefix = "browser_profile:"
	refreshed := 0
	var failures []error
	for _, detail := range details {
		for _, cred := range detail.Credentials {
			if cred == nil || cred.Domain != "copilot_ws" || !strings.HasPrefix(cred.SourceRef, prefix) {
				continue
			}
			profileID := strings.TrimPrefix(cred.SourceRef, prefix)
			if profileID == "" {
				continue
			}
			// lead < 0 forces a re-capture (manual admin refresh).
			force := lead < 0
			now := time.Now()
			if due, next, logSkip := refreshDue(cred.ID, cred.Status, force, now); !due {
				if logSkip {
					log.Printf("[CopilotRefresh] profile=%s status=%s backoff_until=%s", profileID, cred.Status, next.UTC().Format(time.RFC3339))
				}
				continue
			}
			secret, err := manager.ReadSecret(ctx, cred.ID)
			if err != nil {
				report := refreshFailed(cred.ID, now)
				if force || report {
					failures = append(failures, fmt.Errorf("profile %s: credential secret unavailable", profileID))
				}
				continue
			}
			var stored struct {
				TokenURL string `json:"token_url"`
			}
			_ = json.Unmarshal(secret, &stored)
			storedValid := false
			if stored.TokenURL != "" {
				if _, validErr := copilot.ValidateTokenURL(stored.TokenURL, int64(lead.Seconds())); validErr == nil {
					if lead >= 0 {
						clearRefreshBackoff(cred.ID)
						continue
					}
					storedValid = true
				}
			}
			captureCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			token, email, expiresAt, err := browser.CaptureCopilotTokenFromProfile(captureCtx, profilesDir, profileID)
			cancel()
			if err != nil {
				// A signed-out profile must not demote a credential whose stored
				// token is still valid; only a dead token makes it unusable.
				switch {
				case errors.Is(err, browser.ErrProfileAuthRequired) && !storedValid:
					_ = manager.SetStatus(ctx, cred.ID, identity.StatusAuthRequired, nil, false)
				case storedValid:
					// Still-valid token wins over a signed-out profile.
					_ = manager.SetStatus(ctx, cred.ID, identity.StatusHealthy, nil, false)
				}
				report := refreshFailed(cred.ID, time.Now())
				if force || report {
					failures = append(failures, fmt.Errorf("profile %s: %w", profileID, err))
				}
				continue
			}
			// Never move a token across accounts: the profile must be signed in
			// as the identity that owns it.
			if !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(detail.Identity.Email)) {
				report := refreshFailed(cred.ID, time.Now())
				if force || report {
					failures = append(failures, fmt.Errorf("profile %s: captured account %s does not match identity", profileID, email))
				}
				continue
			}
			secretBytes, _ := json.Marshal(map[string]string{"token_url": token})
			priority := detail.Identity.Priority
			if priority <= 0 {
				priority = 1
			}
			if _, err := manager.UpsertCopilotAccount(ctx, identity.CopilotCredentialSpec{
				IdentityID:     detail.Identity.ID,
				CredentialID:   cred.ID,
				Email:          detail.Identity.Email,
				Priority:       priority,
				MaxConcurrency: cred.MaxConcurrency,
				PoolRole:       cred.PoolRole,
				SourceRef:      cred.SourceRef,
				Secret:         secretBytes,
			}); err != nil {
				report := refreshFailed(cred.ID, time.Now())
				if force || report {
					failures = append(failures, fmt.Errorf("profile %s: store refreshed token: %w", profileID, err))
				}
				continue
			}
			_ = manager.SetStatus(ctx, cred.ID, identity.StatusHealthy, nil, false)
			clearRefreshBackoff(cred.ID)
			log.Printf("[CopilotRefresh] profile=%s token refreshed, expires_at=%s", profileID, expiresAt.Format(time.RFC3339))
			refreshed++
		}
	}
	return refreshed, errors.Join(failures...)
}
