package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"sever_provider/internal/identity"
	"sever_provider/internal/pool"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/piper"
	"sever_provider/internal/session"
	"sever_provider/internal/settings"
	"sever_provider/internal/workerhub"
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

func providerModelRequest(req provider.ChatRequest, spec ModelSpec) provider.ChatRequest {
	if spec.BaseModel != "" {
		req.Model = spec.BaseModel
		req.ReasoningEffort = spec.ReasoningEffort
	}
	return req
}

func (d *Dispatcher) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	ctx, cancel, unlock, err := d.chatContext(ctx, req)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	defer cancel()
	defer unlock()
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
		// An image request must not fail over to a text-only model: that would
		// turn a generation failure into a misleading 400 from the fallback.
		if req.ImageGeneration {
			fallbackSpec, ok := d.Models.Get(fallback)
			if !ok || !contains(fallbackSpec.Capabilities, provider.CapabilityImage) {
				continue
			}
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
	// P1-5: namespace first so every session lookup/commit below is isolated per API key.
	req.SessionID = namespaceSession(ctx, req.SessionID)
	if d.Sessions != nil {
		if b, ok := d.Sessions.Get(req.SessionID); ok && b.Provider != spec.Provider {
			return provider.ChatResponse{}, fmt.Errorf("existing chat session provider mismatch")
		}
	}
	if len(creds) == 0 && spec.GuestAllowed {
		return d.chatGuest(ctx, req, spec)
	}
	if upstream, ok := d.sessionUpstream(req.SessionID); ok {
		req.Upstream = upstream
	}
	creds, err = d.chatCredentials(creds, req.SessionID, spec.Provider)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	isBoundSession := false
	if d.Sessions != nil && strings.TrimSpace(req.SessionID) != "" {
		if b, ok := d.Sessions.Get(req.SessionID); ok && b.Provider == spec.Provider {
			isBoundSession = true
		}
	}
	now := time.Now()
	lastErr := error(ErrNoAvailableCredential)
	skippedStatus, skippedBreaker := 0, 0
	for _, cred := range creds {
		cred = d.recoverCooldownIfExpired(ctx, cred, now)
		if ctx.Err() != nil {
			return provider.ChatResponse{}, ctx.Err()
		}
		if (cred.Status != identity.StatusHealthy && cred.Status != identity.StatusDegraded) || (cred.CooldownUntil != nil && now.Before(*cred.CooldownUntil)) {
			skippedStatus++
			continue
		}
		if d.Breakers != nil {
			if !d.Breakers.Get(spec.Provider).Allow(now) {
				skippedBreaker++
				continue
			}
			if !d.Breakers.GetAccount(cred.ID).Allow(now) {
				skippedBreaker++
				continue
			}
		}
		maxConc := cred.MaxConcurrency
		if cred.Domain == "deepseek_web" {
			maxConc = 1
		}
		if d.Leases != nil {
			capacity := identity.RuntimeCapacity{CredentialID: cred.ID, Domain: cred.Domain, Status: cred.Status, MaxConcurrency: maxConc}
			if cred.CooldownUntil != nil {
				capacity.CooldownUntil = *cred.CooldownUntil
			}
			d.Leases.Register(capacity)
		}
		// When a request is bound to a specific account, queue and wait for its slot
		// so concurrent requests on the same account serialize instead of failing fast.
		lease, err := d.acquireCredential(ctx, cred.ID, cred.Domain, req.Model, spec.TimeoutSec, isBoundSession)
		if err != nil {
			lastErr = err
			continue
		}
		secret, secretErr := d.Credentials.ReadSecret(ctx, cred.ID)
		if secretErr != nil {
			lastErr = fmt.Errorf("credential secret unavailable: %w", secretErr)
			d.release(lease)
			d.recordAttempt(ctx, spec.Provider, cred.ID, fmt.Errorf("credential secret: %w", secretErr))
			continue
		}
		// P2-3: defer release immediately after acquire succeeds so panics or early returns never leak the slot.
		// ponytail: closure wraps call + defer; upgrade to explicit tracker when span metrics added.
		resp, callErr := func() (provider.ChatResponse, error) {
			defer d.release(lease)
			return adapter.Chat(ctx, providerModelRequest(req, spec), secret)
		}()
		// Block empty content before recording success or committing the
		// session: an empty turn must not advance upstream continuity.
		if callErr == nil && strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0 && len(resp.ImageData) == 0 {
			callErr = provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "chat returned empty content with no tool calls")
		}
		if callErr != nil {
			lastErr = callErr
		}
		d.recordAttempt(ctx, spec.Provider, cred.ID, callErr)
		if callErr == nil {
			if err := d.bindSession(req.SessionID, cred.ID, spec.Provider, resp.Upstream); err != nil {
				return provider.ChatResponse{}, err
			}
			return resp, nil
		}
	}
	return provider.ChatResponse{}, fmt.Errorf("chat model=%s provider=%s credentials=%d skipped_status=%d skipped_breaker=%d: %w", req.Model, spec.Provider, len(creds), skippedStatus, skippedBreaker, lastErr)
}

// chatGuest runs a credential-less model (Gemini flash-lite guest mode).
// No lease, breaker, or credential status is involved; upstream continuity
// is still tracked via X-Session-ID when present.
func (d *Dispatcher) chatGuest(ctx context.Context, req provider.ChatRequest, spec ModelSpec) (provider.ChatResponse, error) {
	if d.Sessions != nil {
		if b, ok := d.Sessions.Get(req.SessionID); ok && (b.CredentialID != "guest" || b.Provider != spec.Provider) {
			return provider.ChatResponse{}, fmt.Errorf("existing chat session owner unavailable")
		}
	}
	adapter, ok := d.Adapters.Get(spec.Provider)
	if !ok {
		return provider.ChatResponse{}, fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	if upstream, ok := d.sessionUpstream(req.SessionID); ok {
		req.Upstream = upstream
	}
	resp, err := adapter.Chat(ctx, providerModelRequest(req, spec), nil)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	if strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0 && len(resp.ImageData) == 0 {
		return provider.ChatResponse{}, provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "guest model returned empty content with no tool calls")
	}
	if d.Sessions != nil && strings.TrimSpace(req.SessionID) != "" && !resp.Upstream.Empty() {
		if err := d.bindSession(req.SessionID, "guest", spec.Provider, resp.Upstream); err != nil {
			return provider.ChatResponse{}, err
		}
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
	upstream := provider.UpstreamState{ConversationID: binding.UpstreamConversationID, ResponseID: binding.UpstreamResponseID, ChoiceID: binding.UpstreamChoiceID, ChatSessionID: binding.UpstreamChatSessionID, InvocationID: binding.UpstreamInvocationID, MessageID: binding.UpstreamMessageID, ConversationTransferToken: binding.UpstreamConversationTransferToken, TurnState: binding.UpstreamTurnState}
	if upstream.Empty() {
		return provider.UpstreamState{}, false
	}
	return upstream, true
}

func (d *Dispatcher) bindSession(sessionID, credentialID, providerName string, upstream provider.UpstreamState) error {
	if d.Sessions == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	return d.Sessions.Commit(sessionID, credentialID, providerName, upstream)
}
func (d *Dispatcher) chatContext(ctx context.Context, req provider.ChatRequest) (context.Context, context.CancelFunc, func(), error) {
	timeout := 120 * time.Second
	if spec, ok := d.Models.Get(req.Model); ok && spec.TimeoutSec > 0 {
		timeout = time.Duration(spec.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	unlock := func() {}
	if d.Sessions != nil {
		var err error
		unlock, err = d.Sessions.Acquire(ctx, namespaceSession(ctx, req.SessionID))
		if err != nil {
			cancel()
			return ctx, cancel, func() {}, err
		}
	}
	return ctx, cancel, unlock, nil
}

// sessionNSKey is the context key carrying the caller's API key for session
// namespacing (P1-5). Set it in the gateway before entering the scheduler:
// ctx = context.WithValue(ctx, scheduler.SessionNSKey, rawAPIKey).
type sessionNSKey struct{}

// SessionNSKey advertises the context key without exporting the type.
var SessionNSKey = sessionNSKey{}

// namespaceSession prefixes the raw session ID with a hash of the caller API
// key, so two keys sharing one ID never map to the same upstream conversation.
// No key in context means no namespace (legacy single-key deployments).
// Idempotent: an already-namespaced ID is returned unchanged, so chatContext
// and chatOnce/streamChat can each apply it safely.
// ponytail: 8-byte sha256 prefix only; upgrade to full key id when the gateway
// passes one — raw keys never leave ctx, only the hash is stored.
func namespaceSession(ctx context.Context, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return sessionID
	}
	if i := strings.Index(sessionID, ":"); i == 16 && isHexPrefix(sessionID[:16]) {
		return sessionID
	}
	key, _ := ctx.Value(SessionNSKey).(string)
	key = strings.TrimSpace(key)
	if key == "" {
		return sessionID
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8]) + ":" + sessionID
}

func isHexPrefix(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return len(s) == 16
}
func (d *Dispatcher) chatCredentials(creds []identity.Credential, id, providerName string) ([]identity.Credential, error) {
	if d.Sessions != nil {
		if b, ok := d.Sessions.Get(id); ok {
			if b.Provider != providerName {
				return nil, fmt.Errorf("existing chat session belongs to provider %s", b.Provider)
			}
			for _, c := range creds {
				if c.ID == b.CredentialID {
					return []identity.Credential{c}, nil
				}
			}
			return nil, fmt.Errorf("existing chat session owner unavailable")
		}
	}
	return d.orderedCredentials(creds, id), nil
}

func (d *Dispatcher) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	ctx, cancel, unlock, err := d.chatContext(ctx, req)
	if err != nil {
		return nil, err
	}
	stream, err := d.streamChat(ctx, req)
	if err != nil {
		cancel()
		unlock()
		return nil, err
	}
	out := make(chan provider.StreamChunk)
	go func() {
		defer close(out)
		defer cancel()
		defer unlock()
		for {
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					select {
					case out <- provider.StreamChunk{Err: ctx.Err()}:
					case <-time.After(time.Second):
					}
				}
				return
			case chunk, ok := <-stream:
				if !ok {
					return
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}
func (d *Dispatcher) streamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
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
	// P1-5: namespace first so every session lookup/commit below is isolated per API key.
	req.SessionID = namespaceSession(ctx, req.SessionID)
	if d.Sessions != nil {
		if b, ok := d.Sessions.Get(req.SessionID); ok && b.Provider != spec.Provider {
			return nil, fmt.Errorf("existing chat session provider mismatch")
		}
	}
	if len(creds) == 0 && spec.GuestAllowed {
		return d.streamGuest(ctx, req, spec)
	}
	if upstream, ok := d.sessionUpstream(req.SessionID); ok {
		req.Upstream = upstream
	}
	creds, err = d.chatCredentials(creds, req.SessionID, spec.Provider)
	if err != nil {
		return nil, err
	}
	isBoundSession := false
	if d.Sessions != nil && strings.TrimSpace(req.SessionID) != "" {
		if b, ok := d.Sessions.Get(req.SessionID); ok && b.Provider == spec.Provider {
			isBoundSession = true
		}
	}
	now := time.Now()
	lastErr := error(ErrNoAvailableCredential)
	skippedStatus, skippedBreaker := 0, 0
	for _, cred := range creds {
		cred = d.recoverCooldownIfExpired(ctx, cred, now)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if (cred.Status != identity.StatusHealthy && cred.Status != identity.StatusDegraded) || (cred.CooldownUntil != nil && now.Before(*cred.CooldownUntil)) {
			skippedStatus++
			continue
		}
		if d.Breakers != nil {
			if !d.Breakers.Get(spec.Provider).Allow(now) {
				skippedBreaker++
				continue
			}
			if !d.Breakers.GetAccount(cred.ID).Allow(now) {
				skippedBreaker++
				continue
			}
		}
		maxConc := cred.MaxConcurrency
		if cred.Domain == "deepseek_web" {
			maxConc = 1
		}
		if d.Leases != nil {
			capacity := identity.RuntimeCapacity{CredentialID: cred.ID, Domain: cred.Domain, Status: cred.Status, MaxConcurrency: maxConc}
			if cred.CooldownUntil != nil {
				capacity.CooldownUntil = *cred.CooldownUntil
			}
			d.Leases.Register(capacity)
		}
		// P0-3: stream lease TTL tách khỏi TimeoutSec; wrapRelease Renew mỗi chunk
		// nên sweep chỉ thu hồi stream treo. Release idempotent khi đã sweep.
		// When session-bound, wait for this specific credential's slot.
		lease, err := d.acquireStreamLease(ctx, cred.ID, req.Model, isBoundSession)
		if err != nil {
			lastErr = err
			continue
		}
		secret, secretErr := d.Credentials.ReadSecret(ctx, cred.ID)
		if secretErr != nil {
			lastErr = fmt.Errorf("credential secret unavailable: %w", secretErr)
			d.release(lease)
			d.recordAttempt(ctx, spec.Provider, cred.ID, fmt.Errorf("credential secret: %w", secretErr))
			continue
		}
		stream, callErr := adapter.StreamChat(ctx, providerModelRequest(req, spec), secret)
		if callErr != nil {
			lastErr = callErr
			d.release(lease)
			d.recordAttempt(ctx, spec.Provider, cred.ID, callErr)
			continue
		}
		credID := cred.ID
		provName := spec.Provider
		sessionID := req.SessionID
		return wrapRelease(stream, d.Leases, lease.ID, ctx, func(streamErr error, upstream provider.UpstreamState) error {
			d.recordAttempt(ctx, provName, credID, streamErr)
			if streamErr == nil {
				return d.bindSession(sessionID, credID, provName, upstream)
			}
			return nil
		}), nil
	}
	return nil, fmt.Errorf("stream model=%s provider=%s credentials=%d skipped_status=%d skipped_breaker=%d: %w", req.Model, spec.Provider, len(creds), skippedStatus, skippedBreaker, lastErr)
}

func (d *Dispatcher) streamGuest(ctx context.Context, req provider.ChatRequest, spec ModelSpec) (<-chan provider.StreamChunk, error) {
	if d.Sessions != nil {
		if b, ok := d.Sessions.Get(req.SessionID); ok && (b.CredentialID != "guest" || b.Provider != spec.Provider) {
			return nil, fmt.Errorf("existing chat session owner unavailable")
		}
	}
	adapter, ok := d.Adapters.Get(spec.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	if upstream, ok := d.sessionUpstream(req.SessionID); ok {
		req.Upstream = upstream
	}
	stream, err := adapter.StreamChat(ctx, providerModelRequest(req, spec), nil)
	if err != nil {
		return nil, err
	}
	sessionID := req.SessionID
	return wrapRelease(stream, d.Leases, "", ctx, func(streamErr error, upstream provider.UpstreamState) error {
		if streamErr == nil && d.Sessions != nil && strings.TrimSpace(sessionID) != "" && !upstream.Empty() {
			return d.bindSession(sessionID, "guest", spec.Provider, upstream)
		}
		return nil
	}), nil
}

func wrapRelease(in <-chan provider.StreamChunk, leases *LeaseManager, id string, ctx context.Context, onFinish func(error, provider.UpstreamState) error) <-chan provider.StreamChunk {
	out := make(chan provider.StreamChunk)
	go func() {
		defer close(out)
		if leases != nil && id != "" {
			// P0-3: release idempotent (ignores release-after-sweep noise);
			// per-chunk Renew keeps the sweep from reaping a live stream.
			defer leases.Release(id)
		}
		var upstream provider.UpstreamState
		var emittedText strings.Builder
		emittedToolCalls := false
		finished := false
		finish := func(err error) error {
			if finished {
				return err
			}
			finished = true
			if onFinish != nil {
				if e := onFinish(err, upstream); e != nil {
					return e
				}
			}
			return err
		}
		defer func() {
			if !finished {
				finish(ctx.Err())
			}
		}()
		for {
			select {
			case <-ctx.Done():
				finish(ctx.Err())
				return
			case chunk, ok := <-in:
				if !ok {
					err := finish(fmt.Errorf("upstream stream closed without completion"))
					select {
					case out <- provider.StreamChunk{Err: err}:
					case <-ctx.Done():
					}
					return
				}
				if !chunk.Upstream.Empty() {
					upstream = chunk.Upstream
				}
				if leases != nil && id != "" {
					leases.Renew(id, StreamLeaseTTL)
				}
				emittedText.WriteString(chunk.Content)
				emittedText.WriteString(chunk.ReasoningContent)
				if len(chunk.ToolCalls) > 0 {
					emittedToolCalls = true
				}
				// Block empty streams before committing the session: a Done
				// turn with no text and no tool calls becomes an empty_response
				// error instead of a silent success.
				if chunk.Done && chunk.Err == nil && strings.TrimSpace(emittedText.String()) == "" && !emittedToolCalls {
					chunk.Err = provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "upstream stream completed without text content or tool calls")
					chunk.Done = false
				}
				if chunk.Done || chunk.Err != nil {
					if err := finish(chunk.Err); err != nil {
						chunk.Err = err
						chunk.Done = false
					}
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
				if chunk.Done || chunk.Err != nil {
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
	if spec.BaseModel != "" {
		// A public image-mode ID (gemini-image) renders through its base model.
		req.Model = spec.BaseModel
	}
	adapter, ok := d.Adapters.Get(spec.Provider)
	if !ok {
		return provider.ImageResponse{}, fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	creds, err := d.Credentials.List(ctx, spec.ProviderDomain())
	if err != nil {
		return provider.ImageResponse{}, err
	}
	creds = d.orderedCredentials(creds, req.SessionID)
	// Continue the session's Gemini conversation so repeated image prompts keep
	// the same style instead of starting a fresh one every time.
	if strings.TrimSpace(req.SessionID) != "" && d.Sessions != nil {
		if binding, ok := d.Sessions.Get(req.SessionID); ok && binding.Provider == spec.Provider {
			req.Upstream = provider.UpstreamState{
				ConversationID: binding.UpstreamConversationID,
				ResponseID:     binding.UpstreamResponseID,
				ChoiceID:       binding.UpstreamChoiceID,
			}
		}
	}
	now := time.Now()
	lastErr := error(nil)
	for _, cred := range creds {
		cred = credentialAfterCooldown(cred, now)
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
		lease, leaseErr := d.acquireCredential(ctx, cred.ID, cred.Domain, req.Model, spec.TimeoutSec, false)
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
			// Same affinity chat keeps: credential plus the upstream turn, so the
			// next image in this session continues the conversation.
			_ = d.bindSession(req.SessionID, cred.ID, spec.Provider, result.Upstream)
			return result, nil
		}
		if lastErr == nil {
			lastErr = callErr
		}
	}
	if lastErr != nil {
		return provider.ImageResponse{}, lastErr
	}
	return provider.ImageResponse{}, ErrNoAvailableCredential
}

func (d *Dispatcher) Video(ctx context.Context, req provider.VideoRequest) (provider.VideoResponse, error) {
	spec, ok := d.Models.Get(req.Model)
	if !ok {
		return provider.VideoResponse{}, fmt.Errorf("model %q is not configured", req.Model)
	}
	if !contains(spec.Capabilities, provider.CapabilityVideo) {
		return provider.VideoResponse{}, provider.ErrCapabilityUnsupported
	}
	adapter, ok := d.Adapters.Get(spec.Provider)
	if !ok {
		return provider.VideoResponse{}, fmt.Errorf("provider %q is not registered", spec.Provider)
	}
	creds, err := d.Credentials.List(ctx, spec.ProviderDomain())
	if err != nil {
		return provider.VideoResponse{}, err
	}
	creds = d.orderedCredentials(creds, req.SessionID)
	now := time.Now()
	lastErr := error(nil)
	for _, cred := range creds {
		cred = credentialAfterCooldown(cred, now)
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
		lease, leaseErr := d.acquireCredential(ctx, cred.ID, cred.Domain, req.Model, spec.TimeoutSec, false)
		if leaseErr != nil {
			continue
		}
		secret, secretErr := d.Credentials.ReadSecret(ctx, cred.ID)
		if secretErr != nil {
			d.release(lease)
			d.recordAttempt(ctx, spec.Provider, cred.ID, fmt.Errorf("credential secret: %w", secretErr))
			continue
		}
		result, callErr := adapter.Video(ctx, req, secret)
		d.release(lease)
		d.recordAttempt(ctx, spec.Provider, cred.ID, callErr)
		if callErr == nil {
			return result, nil
		}
		if lastErr == nil {
			lastErr = callErr
		}
	}
	if lastErr != nil {
		return provider.VideoResponse{}, lastErr
	}
	return provider.VideoResponse{}, ErrNoAvailableCredential
}

func (d *Dispatcher) Speech(ctx context.Context, req provider.SpeechRequest) ([]byte, string, error) {
	voice := strings.TrimSpace(req.Voice)
	voice = strings.TrimPrefix(voice, "piper:")
	voice = strings.TrimPrefix(voice, "tiktok:")
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
		if strings.HasPrefix(req.Model, "piper:") {
			alt := "tiktok:" + strings.TrimPrefix(req.Model, "piper:")
			if s, c := d.Models.Get(alt); c {
				spec, configured = s, c
				req.Model = alt
			}
		} else if strings.HasPrefix(req.Model, "tiktok:") {
			alt := "piper:" + strings.TrimPrefix(req.Model, "tiktok:")
			if s, c := d.Models.Get(alt); c {
				spec, configured = s, c
				req.Model = alt
			}
		} else {
			if s, c := d.Models.Get("piper:" + req.Model); c {
				spec, configured = s, c
				req.Model = "piper:" + req.Model
			} else if s, c := d.Models.Get("tiktok:" + req.Model); c {
				spec, configured = s, c
				req.Model = "tiktok:" + req.Model
			}
		}
	}
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
	lastErr := ErrNoAvailableCredential
	for _, cred := range creds {
		cred = credentialAfterCooldown(cred, now)
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
		lease, leaseErr := d.acquireCredential(ctx, cred.ID, cred.Domain, req.Model, spec.TimeoutSec, false)
		if leaseErr != nil {
			lastErr = leaseErr
			continue
		}
		var secret []byte
		if d.Credentials != nil && !strings.HasPrefix(cred.ID, "local-") {
			var secretErr error
			secret, secretErr = d.Credentials.ReadSecret(ctx, cred.ID)
			if secretErr != nil {
				d.release(lease)
				d.recordAttempt(ctx, spec.Provider, cred.ID, fmt.Errorf("credential secret: %w", secretErr))
				lastErr = fmt.Errorf("credential secret: %w", secretErr)
				continue
			}
		}
		audio, mime, callErr := adapter.Speech(ctx, req, secret)

		d.release(lease)
		d.recordAttempt(ctx, spec.Provider, cred.ID, callErr)
		if callErr == nil {
			return audio, mime, nil
		}
		lastErr = callErr
	}
	return nil, "", lastErr
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
	// P1-3: refresh ActiveLeases from the live lease manager before sorting,
	// so order reflects current load instead of the stale List() snapshot.
	// ponytail: in-place refresh only; upgrade to re-List when multi-instance.
	if d.Leases != nil {
		for i := range creds {
			if d.Leases.IsRegistered(creds[i].ID) {
				creds[i].ActiveLeases = d.Leases.ActiveLeasesFor(creds[i].ID)
			}
		}
	}
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
	// Copilot image sanitization rejects the caller's attachment, not the
	// account; charging it would open account breakers and stall text chat.
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "imagesanitization") {
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

	// Persist credential status with a bounded context independent of client cancellation.
	statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Persist credential status.
	updater, ok := d.Credentials.(CredentialStatusUpdater)
	if !ok {
		return
	}
	if err == nil {
		_ = updater.MarkSuccess(statusCtx, credentialID)
		return
	}
	status, cooldown := classifyProviderError(err, now)
	_ = updater.SetStatus(statusCtx, credentialID, status, cooldown, true)
}

func failoverEligible(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNoAvailableCredential) || errors.Is(err, provider.ErrProviderUnavailable) || errors.Is(err, provider.ErrNoCapacity) || errors.Is(err, provider.ErrEmptyResponse) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not registered") || strings.Contains(msg, "provider unavailable") || strings.Contains(msg, "no available credential") || strings.Contains(msg, "empty_response") || strings.Contains(msg, "empty upstream response")
}

func classifyProviderError(err error, now time.Time) (identity.CredentialStatus, *time.Time) {
	if err == nil {
		return identity.StatusHealthy, nil
	}
	var retry interface{ RetryAfter() time.Time }
	if errors.As(err, &retry) {
		until := retry.RetryAfter()
		if until.Before(now) {
			until = now.Add(time.Minute)
		}
		return identity.StatusRateLimited, &until
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

func (d *Dispatcher) acquireCredential(ctx context.Context, credentialID, domain, model string, timeout int, wait bool) (Lease, error) {
	if d.Leases == nil {
		return Lease{}, fmt.Errorf("lease manager is not configured")
	}
	ttl := time.Duration(timeout) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	if wait {
		return d.Leases.AcquireCredentialWait(ctx, credentialID, model, ttl)
	}
	// P0-2/P1-3: non-blocking try per candidate; full slot fails fast with
	// ErrNoCapacity so the loop moves to the next ordered credential instead
	// of polling here until ctx Done.
	if lease, ok := d.tryAcquireFresh(ctx, credentialID, model, ttl); ok {
		return lease, nil
	}
	return Lease{}, provider.ErrNoCapacity
}

// acquireStreamLease separates stream lease TTL from request TimeoutSec (P0-3).
func (d *Dispatcher) acquireStreamLease(ctx context.Context, credentialID, model string, wait bool) (Lease, error) {
	if d.Leases == nil {
		return Lease{}, fmt.Errorf("lease manager is not configured")
	}
	if wait {
		return d.Leases.AcquireCredentialWait(ctx, credentialID, model, StreamLeaseTTL)
	}
	if lease, ok := d.tryAcquireFresh(ctx, credentialID, model, StreamLeaseTTL); ok {
		return lease, nil
	}
	return Lease{}, provider.ErrNoCapacity
}

// tryAcquireFresh does one non-blocking try (P1-3); freshness of ActiveLeases
// is handled by orderedCredentials before the loop.
func (d *Dispatcher) tryAcquireFresh(ctx context.Context, credentialID, model string, ttl time.Duration) (Lease, bool) {
	if d.Leases == nil {
		return Lease{}, false
	}
	lease, err := d.Leases.AcquireCredential(ctx, credentialID, model, ttl)
	if err != nil {
		return Lease{}, false
	}
	return lease, true
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
	case "deepseek_web":
		return "deepseek_web"
	default:
		return s.Provider
	}
}

func (d *Dispatcher) recoverCooldownIfExpired(ctx context.Context, c identity.Credential, now time.Time) identity.Credential {
	if (c.Status == identity.StatusRateLimited || c.Status == identity.StatusCooldown) && c.CooldownUntil != nil && !now.Before(*c.CooldownUntil) {
		// Cooldown has expired: restore to Healthy, clear cooldown timestamp,
		// and persist status to DB so UI and subsequent queries see the unlocked state.
		c.Status = identity.StatusHealthy
		c.CooldownUntil = nil
		c.LastErrorKind = ""
		if updater, ok := d.Credentials.(CredentialStatusUpdater); ok {
			statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = updater.SetStatus(statusCtx, c.ID, identity.StatusHealthy, nil, false)
		}
		if d.Breakers != nil {
			d.Breakers.GetAccount(c.ID).Success()
		}
	}
	return c
}

func credentialAfterCooldown(c identity.Credential, now time.Time) identity.Credential {
	if (c.Status == identity.StatusRateLimited || c.Status == identity.StatusCooldown) && c.CooldownUntil != nil && !now.Before(*c.CooldownUntil) {
		c.Status = identity.StatusHealthy
		c.CooldownUntil = nil
	}
	return c
}
