package copilotrefresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"sever_provider/internal/browser"
	"sever_provider/internal/identity"
	"sever_provider/internal/provider/copilot"
)

// Refresh re-captures Chathub tokens for Copilot credentials whose
// token expires within lead, using their stored signed-in Chrome profile.
func Refresh(ctx context.Context, manager *identity.Manager, profilesDir string, lead time.Duration) (int, error) {
	details, err := manager.ListIdentityDetails(ctx, "copilot")
	if err != nil {
		return 0, err
	}
	log.Printf("[CopilotRefresh] scan: %d identities, lead=%s", len(details), lead)
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
			secret, err := manager.ReadSecret(ctx, cred.ID)
			if err != nil {
				failures = append(failures, fmt.Errorf("profile %s: credential secret unavailable", profileID))
				continue
			}
			var stored struct {
				TokenURL string `json:"token_url"`
			}
			_ = json.Unmarshal(secret, &stored)
			// lead < 0 forces a re-capture (manual admin refresh).
			storedValid := false
			if stored.TokenURL != "" {
				if _, validErr := copilot.ValidateTokenURL(stored.TokenURL, int64(lead.Seconds())); validErr == nil {
					if lead >= 0 {
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
				failures = append(failures, fmt.Errorf("profile %s: %w", profileID, err))
				continue
			}
			// Never move a token across accounts: the profile must be signed in
			// as the identity that owns it.
			if !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(detail.Identity.Email)) {
				failures = append(failures, fmt.Errorf("profile %s: captured account %s does not match identity", profileID, email))
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
				failures = append(failures, fmt.Errorf("profile %s: store refreshed token: %w", profileID, err))
				continue
			}
			_ = manager.SetStatus(ctx, cred.ID, identity.StatusHealthy, nil, false)
			log.Printf("[CopilotRefresh] profile=%s token refreshed, expires_at=%s", profileID, expiresAt.Format(time.RFC3339))
			refreshed++
		}
	}
	return refreshed, errors.Join(failures...)
}
