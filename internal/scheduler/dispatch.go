package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sever_provider/internal/identity"
	"sever_provider/internal/pool"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/session"
	"sever_provider/internal/settings"
	"sever_provider/internal/workerhub"
	"strings"
)

type CredentialSource interface {
	List(ctx context.Context, domain string) ([]identity.Credential, error)
	ReadSecret(ctx context.Context, credentialID string) ([]byte, error)
}

type CredentialStatusUpdater interface {
	MarkSuccess(ctx context.Context, id string) error
	SetStatus(ctx context.Context, id string, status identity.CredentialStatus, cooldownUntil *time.Time, incrementError bool) error
}

type AdapterRegistry interface {
	Get(name string) (provider.Adapter, bool)
}

type Dispatcher struct {
	Models      *ModelRegistry
	Credentials CredentialSource
	Adapters    AdapterRegistry
	Leases      *LeaseManager
	Sessions    *session.Manager
	Pools       *pool.Allocator
	Breakers    *BreakerRegistry
	Workers     *workerhub.Hub
	Settings    *settings.Store
}

func (d *Dispatcher) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	return d.chatWithFailover(ctx, req, make(map[string]bool))
}

func (d *Dispatcher) chatWithFailover(ctx context.Context, req provider.ChatRequest, visited map[string]bool) (provider.ChatResponse, error) {
	if visited[req.Model] {
		return provider.ChatResponse{}, fmt.Errorf("model failover cycle detected at %q", req.Model)
	}
	visited[req.Model] = true
	resp, err := d.chatOnce(ctx, req)
	if err == nil {
		return resp, nil
	}
	spec, ok := d.Models.Get(req.Model)
	if !ok || len(spec.FailoverTo) == 0 || !failoverEligible(err) {
		return provider.ChatResponse{}, err
	}
	for _, fallback := range spec.FailoverTo {
		fallback = strings.TrimSpace(fallback)
		if fallback == "" || visited[fallback] {
			continue
		}
		candidate := req
		candidate.Model = fallback
		if response, fallbackErr := d.chatWithFailover(ctx, candidate, visited); fallbackErr == nil {
			return response, nil
		}
	}
	return provider.ChatResponse{}, err
}

func (d *Dispatcher) chatOnce(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	spec, ok := d.Models.Get(req.Model)
	if !ok {
		return provider.ChatResponse{}, fmt.Errorf("model %q is not configured", req.Model)
	}
	if !contains(spec.Capabilities, provider.CapabilityChat) {
		return provider.ChatResponse{}, provider.ErrCapabilityUnsupported
	}
	adapter, ok := d.Adapters.Get(spec.Provider)
	if !ok {
		return provider.ChatResponse{}, fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	creds, err := d.Credentials.List(ctx, spec.ProviderDomain())
	if err != nil {
		return provider.ChatResponse{}, err
	}
	if len(creds) == 0 && spec.GuestAllowed {
		return d.chatGuest(ctx, req, spec)
	}
	if upstream, ok := d.sessionUpstream(req.SessionID); ok {
		req.Upstream = upstream
	}
	creds = d.orderedCredentials(creds, req.SessionID)
	now := time.Now()
	for _, cred := range creds {
		if (cred.Status != identity.StatusHealthy && cred.Status != identity.StatusDegraded) || (cred.CooldownUntil != nil && now.Before(*cred.CooldownUntil)) {
			continue
		}
		if d.Breakers != nil {
			if !d.Breakers.Get(spec.Provider).Allow(now) {
				continue
			}
			if !d.Breakers.GetAccount(cred.ID).Allow(now) {
				continue
			}
		}
		if d.Leases != nil {
			capacity := identity.RuntimeCapacity{CredentialID: cred.ID, Domain: cred.Domain, Status: cred.Status, MaxConcurrency: cred.MaxConcurrency}
			if cred.CooldownUntil != nil {
				capacity.CooldownUntil = *cred.CooldownUntil
			}
			d.Leases.Register(capacity)
		}
		lease, err := d.acquireCredential(ctx, cred.ID, cred.Domain, req.Model, spec.TimeoutSec)
		if err != nil {
			continue
		}
		secret, secretErr := d.Credentials.ReadSecret(ctx, cred.ID)
		if secretErr != nil {
			d.release(lease)
			d.recordAttempt(ctx, spec.Provider, cred.ID, fmt.Errorf("credential secret: %w", secretErr))
			continue
		}
		resp, callErr := adapter.Chat(ctx, req, secret)
		d.release(lease)
		d.recordAttempt(ctx, spec.Provider, cred.ID, callErr)
		if callErr == nil {
			d.bindSession(req.SessionID, cred.ID, spec.Provider, resp.Upstream)
			return resp, nil
		}
	}
	return provider.ChatResponse{}, ErrNoAvailableCredential
}

// chatGuest runs a credential-less model (Gemini flash-lite guest mode).
// No lease, breaker, or credential status is involved; upstream continuity
// is still tracked via X-Session-ID when present.
func (d *Dispatcher) chatGuest(ctx context.Context, req provider.ChatRequest, spec ModelSpec) (provider.ChatResponse, error) {
	adapter, ok := d.Adapters.Get(spec.Provider)
	if !ok {
		return provider.ChatResponse{}, fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	if upstream, ok := d.sessionUpstream(req.SessionID); ok {
		req.Upstream = upstream
	}
	resp, err := adapter.Chat(ctx, req, nil)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	if d.Sessions != nil && strings.TrimSpace(req.SessionID) != "" && !resp.Upstream.Empty() {
		d.Sessions.SaveUpstreamState(req.SessionID, resp.Upstream)
	}
	return resp, nil
}

func (d *Dispatcher) sessionUpstream(sessionID string) (provider.UpstreamState, bool) {
	if d.Sessions == nil || strings.TrimSpace(sessionID) == "" {
		return provider.UpstreamState{}, false
	}
	binding, ok := d.Sessions.Get(sessionID)
	if !ok {
		return provider.UpstreamState{}, false
	}
	upstream := provider.UpstreamState{ConversationID: binding.UpstreamConversationID, ResponseID: binding.UpstreamResponseID, ChoiceID: binding.UpstreamChoiceID, ChatSessionID: binding.UpstreamChatSessionID, InvocationID: binding.UpstreamInvocationID}
	if upstream.Empty() {
		return provider.UpstreamState{}, false
	}
	return upstream, true
}

func (d *Dispatcher) bindSession(sessionID, credentialID, providerName string, upstream provider.UpstreamState) {
	if d.Sessions == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	_, _ = d.Sessions.Bind(sessionID, credentialID, providerName)
	if !upstream.Empty() {
		d.Sessions.SaveUpstreamState(sessionID, upstream)
	}
}

func (d *Dispatcher) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	spec, ok := d.Models.Get(req.Model)
	if !ok {
		return nil, fmt.Errorf("model %q is not configured", req.Model)
	}
	if !contains(spec.Capabilities, provider.CapabilityStreaming) {
		return nil, provider.ErrCapabilityUnsupported
	}
	adapter, ok := d.Adapters.Get(spec.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	creds, err := d.Credentials.List(ctx, spec.ProviderDomain())
	if err != nil {
		return nil, err
	}
	if len(creds) == 0 && spec.GuestAllowed {
		return d.streamGuest(ctx, req, spec)
	}
	if upstream, ok := d.sessionUpstream(req.SessionID); ok {
		req.Upstream = upstream
	}
	creds = d.orderedCredentials(creds, req.SessionID)
	now := time.Now()
	for _, cred := range creds {
		if (cred.Status != identity.StatusHealthy && cred.Status != identity.StatusDegraded) || (cred.CooldownUntil != nil && now.Before(*cred.CooldownUntil)) {
			continue
		}
		if d.Breakers != nil {
			if !d.Breakers.Get(spec.Provider).Allow(now) {
				continue
			}
			if !d.Breakers.GetAccount(cred.ID).Allow(now) {
				continue
			}
		}
		if d.Leases != nil {
			capacity := identity.RuntimeCapacity{CredentialID: cred.ID, Domain: cred.Domain, Status: cred.Status, MaxConcurrency: cred.MaxConcurrency}
			if cred.CooldownUntil != nil {
				capacity.CooldownUntil = *cred.CooldownUntil
			}
			d.Leases.Register(capacity)
		}
		lease, err := d.acquireCredential(ctx, cred.ID, cred.Domain, req.Model, spec.TimeoutSec)
		if err != nil {
			continue
		}
		secret, secretErr := d.Credentials.ReadSecret(ctx, cred.ID)
		if secretErr != nil {
			d.release(lease)
			d.recordAttempt(ctx, spec.Provider, cred.ID, fmt.Errorf("credential secret: %w", secretErr))
			continue
		}
		stream, callErr := adapter.StreamChat(ctx, req, secret)
		if callErr != nil {
			d.release(lease)
			d.recordAttempt(ctx, spec.Provider, cred.ID, callErr)
			continue
		}
		credID := cred.ID
		provName := spec.Provider
		sessionID := req.SessionID
		return wrapRelease(stream, d.Leases, lease.ID, ctx, func(streamErr error, upstream provider.UpstreamState) {
			d.recordAttempt(ctx, provName, credID, streamErr)
			if streamErr == nil {
				d.bindSession(sessionID, credID, provName, upstream)
			}
		}), nil
	}
	return nil, ErrNoAvailableCredential
}

func (d *Dispatcher) streamGuest(ctx context.Context, req provider.ChatRequest, spec ModelSpec) (<-chan provider.StreamChunk, error) {
	adapter, ok := d.Adapters.Get(spec.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	if upstream, ok := d.sessionUpstream(req.SessionID); ok {
		req.Upstream = upstream
	}
	stream, err := adapter.StreamChat(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	sessionID := req.SessionID
	return wrapRelease(stream, d.Leases, "", ctx, func(streamErr error, upstream provider.UpstreamState) {
		if streamErr == nil && d.Sessions != nil && strings.TrimSpace(sessionID) != "" && !upstream.Empty() {
			d.Sessions.SaveUpstreamState(sessionID, upstream)
		}
	}), nil
}

func wrapRelease(in <-chan provider.StreamChunk, leases *LeaseManager, id string, ctx context.Context, onFinish func(error, provider.UpstreamState)) <-chan provider.StreamChunk {
	out := make(chan provider.StreamChunk)
	go func() {
		defer close(out)
		if leases != nil && id != "" {
			defer leases.Release(id)
		}
		var finalErr error
		var finalUpstream provider.UpstreamState
		defer func() {
			if onFinish != nil {
				onFinish(finalErr, finalUpstream)
			}
		}()
		for {
			select {
			case <-ctx.Done():
				finalErr = ctx.Err()
				return
			case item, ok := <-in:
				if !ok {
					return
				}
				if !item.Upstream.Empty() {
					finalUpstream = item.Upstream
				}
				select {
				case out <- item:
				case <-ctx.Done():
					return
				}
				if item.Err != nil {
					finalErr = item.Err
					return
				}
				if item.Done {
					return
				}
			}
		}
	}()
	return out
}

func (d *Dispatcher) release(lease Lease) {
	if d.Leases != nil {
		_ = d.Leases.Release(lease.ID)
	}
}

func (d *Dispatcher) Image(ctx context.Context, req provider.ImageRequest) (provider.ImageResponse, error) {
	spec, ok := d.Models.Get(req.Model)
	if !ok {
		return provider.ImageResponse{}, fmt.Errorf("model %q is not configured", req.Model)
	}
	if !contains(spec.Capabilities, provider.CapabilityImage) {
		return provider.ImageResponse{}, provider.ErrCapabilityUnsupported
	}
	adapter, ok := d.Adapters.Get(spec.Provider)
	if !ok {
		return provider.ImageResponse{}, fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	creds, err := d.Credentials.List(ctx, spec.ProviderDomain())
	if err != nil {
		return provider.ImageResponse{}, err
	}
	now := time.Now()
	for _, cred := range creds {
		if (cred.Status != identity.StatusHealthy && cred.Status != identity.StatusDegraded) || (cred.CooldownUntil != nil && now.Before(*cred.CooldownUntil)) {
			continue
		}
		if d.Breakers != nil {
			if !d.Breakers.Get(spec.Provider).Allow(now) {
				continue
			}
			if !d.Breakers.GetAccount(cred.ID).Allow(now) {
				continue
			}
		}
		if d.Leases != nil {
			d.Leases.Register(identity.RuntimeCapacity{CredentialID: cred.ID, Domain: cred.Domain, Status: cred.Status, MaxConcurrency: maxInt(cred.MaxConcurrency, 1)})
		}
		lease, leaseErr := d.acquireCredential(ctx, cred.ID, cred.Domain, req.Model, spec.TimeoutSec)
		if leaseErr != nil {
			continue
		}
		secret, secretErr := d.Credentials.ReadSecret(ctx, cred.ID)
		if secretErr != nil {
			d.release(lease)
			d.recordAttempt(ctx, spec.Provider, cred.ID, fmt.Errorf("credential secret: %w", secretErr))
			continue
		}
		result, callErr := adapter.Image(ctx, req, secret)
		d.release(lease)
		d.recordAttempt(ctx, spec.Provider, cred.ID, callErr)
		if callErr == nil {
			return result, nil
		}
	}
	return provider.ImageResponse{}, ErrNoAvailableCredential
}

func (d *Dispatcher) Speech(ctx context.Context, req provider.SpeechRequest) ([]byte, string, error) {
	voice := req.Voice
	if voice == "" && strings.HasPrefix(req.Model, "piper:") {
		voice = strings.TrimPrefix(req.Model, "piper:")
	}
	if voice == "" && strings.HasPrefix(req.Model, "tiktok:") {
		voice = strings.TrimPrefix(req.Model, "tiktok:")
	}
	remoteMode := strings.HasPrefix(req.Model, "piper:") && d.Settings != nil && d.Settings.Get(ctx, "tts.piper_mode", "auto") == "remote"
	localMode := strings.HasPrefix(req.Model, "piper:") && d.Settings != nil && d.Settings.Get(ctx, "tts.piper_mode", "auto") == "local"

	workerModel := req.Model
	if workerModel == "" && voice != "" {
		workerModel = "piper:" + voice
	}
	workerTarget := ""
	if !localMode && d.Workers != nil {
		if workerModel != "" && d.Workers.Available(workerModel) {
			workerTarget = workerModel
		} else if strings.HasPrefix(req.Model, "piper:") && voice != "" && d.Workers.Available("piper:"+voice) {
			workerTarget = "piper:" + voice
		} else if voice != "" && d.Workers.Available("piper:"+voice) {
			workerTarget = "piper:" + voice
		} else if req.Model != "" && d.Workers.Available(req.Model) {
			workerTarget = req.Model
		}
	}
	if remoteMode && workerTarget == "" {
		return nil, "", fmt.Errorf("remote piper voice %q is not available", voice)
	}
		if workerTarget != "" {
			wReq := req
			wReq.Model = workerTarget
			wReq.Voice = voice
			data, mime, err := d.Workers.Speech(ctx, wReq)
			if err == nil {
				return data, mime, nil
			}
			return nil, "", err
		}

	spec, configured := d.Models.Get(req.Model)
	if !configured {
		return nil, "", fmt.Errorf("model %q is not configured", req.Model)
	}
	if spec.Provider == "piper_local" && d.Settings != nil && !d.Settings.GetBool(ctx, "tts.voice."+voice+".enabled", true) {
		return nil, "", fmt.Errorf("piper voice %q is disabled", voice)
	}

	adapter, ok := d.Adapters.Get(spec.Provider)

	if !ok {
		return nil, "", fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	if spec.Provider == "piper_local" {
		if !d.hasLocalPiperVoice(voice) {
			return nil, "", fmt.Errorf("piper voice %q is not available locally", voice)
		}
		req.Voice = voice
	}
	var creds []identity.Credential
	if d.Credentials != nil {
		var err error
		creds, err = d.Credentials.List(ctx, spec.ProviderDomain())
		if err != nil {
			return nil, "", err
		}
	}
	if len(creds) == 0 {
		creds = []identity.Credential{{ID: "local-" + spec.Provider, Domain: spec.Provider, Status: identity.StatusHealthy, MaxConcurrency: 1}}
	}
	now := time.Now()
	for _, cred := range creds {
		if (cred.Status != identity.StatusHealthy && cred.Status != identity.StatusDegraded) || (cred.CooldownUntil != nil && now.Before(*cred.CooldownUntil)) {
			continue
		}
		if d.Breakers != nil {
			if !d.Breakers.Get(spec.Provider).Allow(now) {
				continue
			}
			if !d.Breakers.GetAccount(cred.ID).Allow(now) {
				continue
			}
		}
		if d.Leases != nil {
			d.Leases.Register(identity.RuntimeCapacity{CredentialID: cred.ID, Domain: cred.Domain, Status: identity.StatusHealthy, MaxConcurrency: maxInt(cred.MaxConcurrency, 1)})
		}
		lease, leaseErr := d.acquireCredential(ctx, cred.ID, cred.Domain, req.Model, spec.TimeoutSec)
		if leaseErr != nil {
			continue
		}
		var secret []byte
		if d.Credentials != nil && !strings.HasPrefix(cred.ID, "local-") {
			var secretErr error
			secret, secretErr = d.Credentials.ReadSecret(ctx, cred.ID)
			if secretErr != nil {
				d.release(lease)
				d.recordAttempt(ctx, spec.Provider, cred.ID, fmt.Errorf("credential secret: %w", secretErr))
				continue
			}
		}
		audio, mime, callErr := adapter.Speech(ctx, req, secret)

		d.release(lease)
		d.recordAttempt(ctx, spec.Provider, cred.ID, callErr)
		if callErr == nil {
			return audio, mime, nil
		}
	}
	return nil, "", ErrNoAvailableCredential
}

func (d *Dispatcher) hasLocalPiperVoice(voice string) bool {
	if d == nil || d.Adapters == nil || voice == "" {
		return false
	}
	adapter, ok := d.Adapters.Get("piper_local")
	if !ok {
		return false
	}
	voices, ok := adapter.(interface{ Voices() []piper.Voice })
	if !ok {
		return false
	}
	for _, item := range voices.Voices() {
		if item.ID == voice && item.ModelPath != "" {
			return true
		}
	}
	return false
}

func (d *Dispatcher) orderedCredentials(creds []identity.Credential, sessionID string) []identity.Credential {
	hasSession := strings.TrimSpace(sessionID) != ""
	if d.Sessions != nil && hasSession {
		if binding, ok := d.Sessions.Get(sessionID); ok {
			var bound identity.Credential
			found := false
			rest := make([]identity.Credential, 0, len(creds))
			for _, c := range creds {
				if c.ID == binding.CredentialID {
					bound = c
					found = true
				} else {
					rest = append(rest, c)
				}
			}
			if found {
				if d.Pools != nil {
					rest = d.Pools.AllocateCredentials(rest, true)
				}
				return append([]identity.Credential{bound}, rest...)
			}
		}
	}
	if d.Pools != nil {
		return d.Pools.AllocateCredentials(creds, hasSession)
	}
	return creds
}

func (d *Dispatcher) recordAttempt(ctx context.Context, providerName, credentialID string, err error) {
	// Client-initiated cancellations are not provider failures.
	if errors.Is(err, context.Canceled) {
		return
	}

	now := time.Now()

	// Update breaker state.
	if d.Breakers != nil {
		provBreaker := d.Breakers.Get(providerName)
		accBreaker := d.Breakers.GetAccount(credentialID)
		if err == nil {
			provBreaker.Success()
			accBreaker.Success()
		} else {
			provBreaker.Failure(now)
			accBreaker.Failure(now)
		}
	}

	// Persist credential status.
	updater, ok := d.Credentials.(CredentialStatusUpdater)
	if !ok {
		return
	}
	if err == nil {
		_ = updater.MarkSuccess(ctx, credentialID)
		return
	}
	status, cooldown := classifyProviderError(err, now)
	_ = updater.SetStatus(ctx, credentialID, status, cooldown, true)
}

func failoverEligible(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNoAvailableCredential) || errors.Is(err, provider.ErrProviderUnavailable) || errors.Is(err, provider.ErrNoCapacity) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not registered") || strings.Contains(msg, "provider unavailable") || strings.Contains(msg, "no available credential")
}

func classifyProviderError(err error, now time.Time) (identity.CredentialStatus, *time.Time) {
	if err == nil {
		return identity.StatusHealthy, nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "429") || strings.Contains(msg, "rate") || strings.Contains(msg, "quota"):
		cooldown := now.Add(60 * time.Second)
		return identity.StatusRateLimited, &cooldown
	case strings.Contains(msg, "cooldown"):
		cooldown := now.Add(60 * time.Second)
		return identity.StatusCooldown, &cooldown
	case strings.Contains(msg, "401") || strings.Contains(msg, "403") || strings.Contains(msg, "auth") ||
		strings.Contains(msg, "expired") || strings.Contains(msg, "xsrf") || strings.Contains(msg, "cookie"):
		return identity.StatusAuthRequired, nil
	default:
		return identity.StatusDegraded, nil
	}
}

func (d *Dispatcher) acquireCredential(ctx context.Context, credentialID, domain, model string, timeout int) (Lease, error) {
	if d.Leases == nil {
		return Lease{}, fmt.Errorf("lease manager is not configured")
	}
	ttl := time.Duration(timeout) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return d.Leases.AcquireCredential(ctx, credentialID, model, ttl)
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func contains(items []provider.Capability, want provider.Capability) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

var ErrNoAvailableCredential = fmt.Errorf("no available credential")

// ProviderDomain maps provider name to credential domain. It remains explicit so identity state is never inferred from model names.
func (s ModelSpec) ProviderDomain() string {
	switch s.Provider {
	case "gemini_web":
		return "gemini_web"
	case "google_colab":
		return "google_colab"
	case "copilot_ws":
		return "copilot_ws"
	case "piper_local":
		return "piper_local"
	case "tiktok_api":
		return "tiktok_api"
	default:
		return s.Provider
	}
}
