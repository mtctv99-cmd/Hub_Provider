package main

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
	"sever_provider/internal/provider/gemini"
)

func refreshSelectedGoogleProfiles(ctx context.Context, manager *identity.Manager, adapter *gemini.Adapter, selection string) error {
	wanted := make(map[string]bool)
	for _, id := range strings.Split(selection, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			wanted[id] = false
		}
	}
	if len(wanted) == 0 {
		return errors.New("no profile selected")
	}
	details, err := manager.ListIdentityDetails(ctx, "google")
	if err != nil {
		return err
	}
	browsers := browser.NewManager("data/profiles", nil)
	var failures []error
	for _, detail := range details {
		for _, cred := range detail.Credentials {
			if cred == nil || cred.Domain != "gemini_web" {
				continue
			}
			id := strings.TrimPrefix(cred.SourceRef, "browser_profile:")
			if _, ok := wanted[id]; !ok {
				continue
			}
			wanted[id] = true
			cookie, err := refreshGoogleCredential(ctx, browsers, manager, adapter, cred, id)
			if err != nil {
				failures = append(failures, fmt.Errorf("profile %s: %w", id, err))
				continue
			}
			secret, _ := json.Marshal(map[string]string{"cookie": cookie})
			specs := []identity.GoogleCredentialSpec{{Domain: cred.Domain, ID: cred.ID, Status: identity.StatusHealthy, MaxConcurrency: cred.MaxConcurrency, PoolRole: cred.PoolRole, SourceRef: cred.SourceRef, Fingerprint: cred.Fingerprint, Secret: secret}}
			if _, err := manager.UpsertGoogleAccount(ctx, detail.Identity.Email, detail.Identity.Priority, specs); err != nil {
				failures = append(failures, fmt.Errorf("profile %s credential update failed", id))
				continue
			}
			log.Printf("[AutoRefresh] profile=%s captured, Google validation passed, encrypted credential saved", id)
		}
	}
	for id, found := range wanted {
		if !found {
			failures = append(failures, fmt.Errorf("profile %s has no linked Google credential", id))
		}
	}
	return errors.Join(failures...)
}

// Restore only the encrypted cookie belonging to this credential, after Google
// validates it. Failed capture or validation never replaces the stored secret.
func refreshGoogleCredential(ctx context.Context, browsers *browser.Manager, credentials *identity.Manager, adapter *gemini.Adapter, cred *identity.Credential, profileID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cookie, err := browsers.RefreshAccountFromProfileContext(ctx, profileID, "")
	if errors.Is(err, browser.ErrProfileAuthRequired) {
		secret, readErr := credentials.ReadSecret(ctx, cred.ID)
		if readErr != nil {
			return "", errors.New("stored credential unavailable for profile recovery")
		}
		var stored struct {
			Cookie string `json:"cookie"`
		}
		if json.Unmarshal(secret, &stored) != nil || stored.Cookie == "" {
			return "", errors.New("stored credential has no cookie for profile recovery")
		}
		if err := adapter.ValidateCookie(ctx, stored.Cookie); err != nil {
			return "", fmt.Errorf("stored cookie validation failed: %w", err)
		}
		cookie, err = browsers.RefreshAccountFromProfileContext(ctx, profileID, stored.Cookie)
	}
	if err != nil {
		return "", err
	}
	if err := adapter.ValidateCookie(ctx, cookie); err != nil {
		return "", fmt.Errorf("captured cookie validation failed: %w", err)
	}
	return cookie, nil
}
