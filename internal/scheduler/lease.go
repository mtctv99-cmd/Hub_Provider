package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"sever_provider/internal/identity"
)

type Lease struct {
	ID           string
	CredentialID string
	Domain       string
	RequestID    string
	AcquiredAt   time.Time
	ExpiresAt    time.Time
}

func (l *Lease) Expired(now time.Time) bool { return !now.Before(l.ExpiresAt) }

type leaseCapacity struct {
	identity.RuntimeCapacity
}

type LeaseManager struct {
	mu         sync.Mutex
	capacities map[string]*leaseCapacity
	leases     map[string]Lease
	now        func() time.Time
}

func NewLeaseManager() *LeaseManager {
	return &LeaseManager{capacities: make(map[string]*leaseCapacity), leases: make(map[string]Lease), now: time.Now}
}

func (m *LeaseManager) Register(c identity.RuntimeCapacity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.MaxConcurrency < 1 {
		c.MaxConcurrency = 1
	}
	if existing, ok := m.capacities[c.CredentialID]; ok {
		existing.MaxConcurrency = c.MaxConcurrency
		existing.Domain = c.Domain
		existing.Status = c.Status
		existing.CooldownUntil = c.CooldownUntil
		return
	}
	m.capacities[c.CredentialID] = &leaseCapacity{RuntimeCapacity: c}
}

func (m *LeaseManager) SetStatus(id string, status identity.CredentialStatus, until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.capacities[id]; ok {
		c.Status, c.CooldownUntil = status, until
	}
}

func (m *LeaseManager) Unregister(credentialID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.capacities, credentialID)
}

func (m *LeaseManager) Acquire(ctx context.Context, domain, requestID string, ttl time.Duration) (Lease, error) {
	if ttl <= 0 {
		ttl = 3 * time.Minute
	}
	for {
		m.mu.Lock()
		if lease, ok := m.tryAcquireLocked(domain, requestID, ttl); ok {
			m.mu.Unlock()
			return lease, nil
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return Lease{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// AcquireCredential acquires a lease specifically for the given credentialID.
func (m *LeaseManager) AcquireCredential(ctx context.Context, credentialID, requestID string, ttl time.Duration) (Lease, error) {
	if ttl <= 0 {
		ttl = 3 * time.Minute
	}
	for {
		m.mu.Lock()
		if lease, ok := m.tryAcquireCredentialLocked(credentialID, requestID, ttl); ok {
			m.mu.Unlock()
			return lease, nil
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return Lease{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (m *LeaseManager) tryAcquireCredentialLocked(credentialID, requestID string, ttl time.Duration) (Lease, bool) {
	now := m.now()
	c, ok := m.capacities[credentialID]
	if !ok {
		return Lease{}, false
	}
	if (c.Status != identity.StatusHealthy && c.Status != identity.StatusDegraded) || now.Before(c.CooldownUntil) {
		return Lease{}, false
	}
	if atomic.LoadInt32(&c.ActiveLeases) >= int32(c.MaxConcurrency) {
		return Lease{}, false
	}
	atomic.AddInt32(&c.ActiveLeases, 1)
	lease := Lease{ID: randomID("lease"), CredentialID: c.CredentialID, Domain: c.Domain, RequestID: requestID, AcquiredAt: now, ExpiresAt: now.Add(ttl)}
	m.leases[lease.ID] = lease
	return lease, true
}

func (m *LeaseManager) tryAcquireLocked(domain, requestID string, ttl time.Duration) (Lease, bool) {
	now := m.now()
	for _, c := range m.capacities {
		if c.Domain != domain || (c.Status != identity.StatusHealthy && c.Status != identity.StatusDegraded) || now.Before(c.CooldownUntil) {
			continue
		}
		if atomic.LoadInt32(&c.ActiveLeases) >= int32(c.MaxConcurrency) {
			continue
		}
		atomic.AddInt32(&c.ActiveLeases, 1)
		lease := Lease{ID: randomID("lease"), CredentialID: c.CredentialID, Domain: c.Domain, RequestID: requestID, AcquiredAt: now, ExpiresAt: now.Add(ttl)}
		m.leases[lease.ID] = lease
		return lease, true
	}
	return Lease{}, false
}

func (m *LeaseManager) Release(leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[leaseID]
	if !ok {
		return errors.New("lease not found")
	}
	if c, ok := m.capacities[lease.CredentialID]; ok && atomic.LoadInt32(&c.ActiveLeases) > 0 {
		atomic.AddInt32(&c.ActiveLeases, -1)
	}
	delete(m.leases, leaseID)
	return nil
}

func (m *LeaseManager) ReclaimExpired() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	count := 0
	for id, lease := range m.leases {
		if !lease.Expired(now) {
			continue
		}
		if c, ok := m.capacities[lease.CredentialID]; ok && atomic.LoadInt32(&c.ActiveLeases) > 0 {
			atomic.AddInt32(&c.ActiveLeases, -1)
		}
		delete(m.leases, id)
		count++
	}
	return count
}

func (m *LeaseManager) Snapshot() []Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Lease, 0, len(m.leases))
	for _, lease := range m.leases {
		out = append(out, lease)
	}
	return out
}

func (m *LeaseManager) ActiveForCredential(id string) int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.capacities[id]; ok {
		return atomic.LoadInt32(&c.ActiveLeases)
	}
	return 0
}

func (m *LeaseManager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.leases)
}

func (m *LeaseManager) ActiveLeasesFor(credentialID string) int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.capacities[credentialID]; ok {
		return atomic.LoadInt32(&c.ActiveLeases)
	}
	return 0
}

func randomID(prefix string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf)
}
