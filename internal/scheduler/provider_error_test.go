package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sever_provider/internal/identity"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/copilot"
)

// H4 regression: "generated" contains "rate" but is not a rate limit. Image
// parser failures must degrade the attempt, not park the credential 60s.
func TestClassifyProviderErrorGeneratedIsNotRateLimited(t *testing.T) {
	now := time.Now()
	for _, msg := range []string{
		"gemini image response contains no supported generated image",
		"generated image bytes unavailable",
	} {
		status, cooldown := classifyProviderError(errors.New(msg), now)
		if status == identity.StatusRateLimited || cooldown != nil {
			t.Fatalf("%q classified as rate limited (cooldown=%v)", msg, cooldown)
		}
		if status != identity.StatusDegraded {
			t.Fatalf("%q status = %s, want DEGRADED", msg, status)
		}
	}
}

func TestClassifyProviderErrorRateLimitPhrases(t *testing.T) {
	now := time.Now()
	for _, msg := range []string{
		"HTTP 429 from upstream",
		"rate limit exceeded",
		"rate_limit_exceeded",
		"ratelimit",
		"too many requests",
		"quota exhausted",
	} {
		status, cooldown := classifyProviderError(errors.New(msg), now)
		if status != identity.StatusRateLimited || cooldown == nil {
			t.Fatalf("%q status=%s cooldown=%v, want RATE_LIMITED with cooldown", msg, status, cooldown)
		}
	}
}

// M1 regression: Copilot quota/throttle rejections carry RetryAfter and must
// become RATE_LIMITED with that cooldown instead of a generic DEGRADED 502.
func TestClassifyProviderErrorCopilotLimits(t *testing.T) {
	now := time.Now()
	until := now.Add(10 * time.Minute)
	status, cooldown := classifyProviderError(&copilot.LimitError{Kind: copilot.LimitKindOutOfCredits, Until: until}, now)
	if status != identity.StatusRateLimited || cooldown == nil || !cooldown.Equal(until) {
		t.Fatalf("LimitError status=%s cooldown=%v, want RATE_LIMITED until %v", status, cooldown, until)
	}
	for _, msg := range []string{
		"copilot upstream rejected request (OutOfCredits): You've used your daily chat limit",
		"copilot upstream throttled request (Throttled): slow down",
	} {
		status, cooldown := classifyProviderError(errors.New(msg), now)
		if status != identity.StatusRateLimited || cooldown == nil {
			t.Fatalf("%q status=%s cooldown=%v, want RATE_LIMITED", msg, status, cooldown)
		}
	}
}

// M16 regression: client-caused failures must not charge the provider/account
// breakers or persist a credential cooldown.
func TestClientCausedErrorsDoNotChargeBreakers(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"empty response", provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "chat returned empty content with no tool calls")},
		{"tool_choice ignored", errors.New("gemini: tool_choice requires a tool call but the model returned none")},
		{"context canceled", context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}}
			models := NewModelRegistry()
			_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})
			d := &Dispatcher{Models: models, Credentials: source, Adapters: singleRegistry{&countAdapter{callErr: tc.err}}, Leases: NewLeaseManager(), Breakers: NewBreakerRegistry()}

			for i := 0; i < 5; i++ {
				if _, err := d.Chat(context.Background(), provider.ChatRequest{Model: "test-model"}); err == nil {
					t.Fatal("expected chat failure")
				}
			}
			now := time.Now()
			if !d.Breakers.Get("test").Allow(now) || d.Breakers.Get("test").State() != CircuitClosed {
				t.Fatalf("provider breaker opened on client error: %s", d.Breakers.Get("test").State())
			}
			if !d.Breakers.GetAccount("c1").Allow(now) || d.Breakers.GetAccount("c1").State() != CircuitClosed {
				t.Fatalf("account breaker opened on client error: %s", d.Breakers.GetAccount("c1").State())
			}
			if len(source.statusSets) != 0 {
				t.Fatalf("client error persisted credential status: %+v", source.statusSets)
			}
		})
	}
}

// M3 regression: cancellation diagnostics must name the model/provider/
// credential/request while preserving the 499 classification.
func TestDispatchCanceledErrorCarriesContext(t *testing.T) {
	source := &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})
	d := &Dispatcher{Models: models, Credentials: source, Adapters: singleRegistry{&countAdapter{callErr: context.Canceled}}, Leases: NewLeaseManager(), Breakers: NewBreakerRegistry()}

	ctx := WithRequestID(context.Background(), "req-test-1")
	_, err := d.Chat(ctx, provider.ChatRequest{Model: "test-model", SessionID: "s1"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(context.Canceled) lost: %v", err)
	}
	for _, want := range []string{"model=test-model", "provider=test", "credential=c1", "request_id=req-test-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %q", want, err.Error())
		}
	}
}

func TestDispatchCanceledBeforeCredentialCarriesContext(t *testing.T) {
	source := &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat}, TimeoutSec: 10})
	d := &Dispatcher{Models: models, Credentials: source, Adapters: singleRegistry{&countAdapter{}}, Leases: NewLeaseManager(), Breakers: NewBreakerRegistry()}

	ctx, cancel := context.WithCancel(WithRequestID(context.Background(), "req-pre"))
	cancel()
	_, err := d.Chat(ctx, provider.ChatRequest{Model: "test-model"})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped cancellation, got %v", err)
	}
	for _, want := range []string{"model=test-model", "provider=test", "request_id=req-pre"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %q", want, err.Error())
		}
	}
}

func TestDispatchStreamCanceledErrorCarriesContext(t *testing.T) {
	source := &trackingSource{creds: []identity.Credential{{ID: "c1", Domain: "test", Status: identity.StatusHealthy, MaxConcurrency: 2}}}
	models := NewModelRegistry()
	_ = models.Register(ModelSpec{ID: "test-model", Provider: "test", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}, TimeoutSec: 10})
	d := &Dispatcher{Models: models, Credentials: source, Adapters: singleRegistry{&countAdapter{callErr: context.Canceled}}, Leases: NewLeaseManager(), Breakers: NewBreakerRegistry()}

	ctx := WithRequestID(context.Background(), "req-stream")
	_, err := d.StreamChat(ctx, provider.ChatRequest{Model: "test-model"})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped stream cancellation, got %v", err)
	}
	for _, want := range []string{"model=test-model", "provider=test", "credential=c1", "request_id=req-stream"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %q", want, err.Error())
		}
	}
}
