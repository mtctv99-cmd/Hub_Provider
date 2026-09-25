package scheduler

import "sync"

type BreakerRegistry struct {
	mu        sync.RWMutex
	providers map[string]*ProviderBreaker
	accounts  map[string]*AccountBreaker
}

func NewBreakerRegistry() *BreakerRegistry {
	return &BreakerRegistry{providers: make(map[string]*ProviderBreaker), accounts: make(map[string]*AccountBreaker)}
}
func (r *BreakerRegistry) Get(provider string) *ProviderBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.providers[provider]; ok {
		return b
	}
	b := NewProviderBreaker()
	r.providers[provider] = b
	return b
}
func (r *BreakerRegistry) GetAccount(credentialID string) *AccountBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.accounts[credentialID]; ok {
		return b
	}
	b := NewAccountBreaker()
	r.accounts[credentialID] = b
	return b
}
func (r *BreakerRegistry) Snapshot() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.providers))
	for name, b := range r.providers {
		out[name] = string(b.State())
	}
	for id, b := range r.accounts {
		if s := b.State(); s != CircuitClosed {
			out["account:"+id] = string(s)
		}
	}
	return out
}
