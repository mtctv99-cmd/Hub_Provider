package scheduler

import (
	"context"
	"sever_provider/internal/identity"
	"sync"
)

type MemoryCredentials struct {
	mu      sync.RWMutex
	items   map[string][]identity.Credential
	secrets map[string][]byte
}

func NewMemoryCredentials() *MemoryCredentials {
	return &MemoryCredentials{items: make(map[string][]identity.Credential), secrets: make(map[string][]byte)}
}
func (m *MemoryCredentials) Add(domain string, c identity.Credential, secret []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[domain] = append(m.items[domain], c)
	m.secrets[c.ID] = append([]byte(nil), secret...)
}
func (m *MemoryCredentials) List(_ context.Context, domain string) ([]identity.Credential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]identity.Credential(nil), m.items[domain]...), nil
}
func (m *MemoryCredentials) ReadSecret(_ context.Context, id string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]byte(nil), m.secrets[id]...), nil
}
