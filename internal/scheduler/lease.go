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
	"sever_provider/internal/provider"
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
	// waiters is the bounded FIFO admission queue for Acquire (P1-2).
	// ponytail: turn check is per-domain so one hot domain never blocks others;
	// upgrade to cond-broadcast only if the 25ms poll shows up in profiles.
	waiters    []leaseWaiter
	nextTicket uint64
	maxQueue   int
}

type leaseWaiter struct {
	ticket uint64
	domain string
}

// StreamLeaseTTL separates stream lease lifetime from request TimeoutSec (P0-3).
// Streams Renew it per chunk, so the sweep only reaps stalled streams.
// ponytail: fixed value; upgrade to per-model config when long-tail models need it.
const StreamLeaseTTL = 30 * time.Minute

// ErrLeaseNotFound reports a release-after-sweep race.
// ponytail: kept as non-nil error (not silent nil) to preserve the
// lease_test.go contract; dispatch ignores it, so no log noise.
var ErrLeaseNotFound = errors.New("lease not found")

func NewLeaseManager() *LeaseManager {
	return &LeaseManager{capacities: make(map[string]*leaseCapacity), leases: make(map[string]Lease), now: time.Now, maxQueue: 128}
}

// SetMaxQueue bounds the Acquire FIFO; overflow fails fast with ErrNoCapacity.
func (m *LeaseManager) SetMaxQueue(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxQueue = n
}

func (m *LeaseManager) maxQueueLocked() int {
	if m.maxQueue <= 0 {
		return 128
	}
	return m.maxQueue
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
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	// P1-2: bounded FIFO admission. Full queue fails fast with ErrNoCapacity
	// instead of piling unbounded waiters behind a hot credential.
	m.mu.Lock()
	if len(m.waiters) >= m.maxQueueLocked() {
		m.mu.Unlock()
		return Lease{}, provider.ErrNoCapacity
	}
	ticket := m.nextTicket
	m.nextTicket++
	m.waiters = append(m.waiters, leaseWaiter{ticket: ticket, domain: domain})
	m.mu.Unlock()
	defer m.dequeueWaiter(ticket)
	for {
		m.mu.Lock()
		// Only the head waiter per domain attempts; other domains never block it.
		if m.turnLocked(ticket, domain) {
			if lease, ok := m.tryAcquireLocked(domain, requestID, ttl); ok {
				m.removeWaiterLocked(ticket)
				m.mu.Unlock()
				return lease, nil
			}
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return Lease{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// AcquireCredential is a single non-blocking try for one credential (P0-2, P1-3).
// On a full slot it returns ErrNoCapacity immediately so the dispatcher moves
// to the next candidate instead of polling here until ctx Done.
func (m *LeaseManager) AcquireCredential(ctx context.Context, credentialID, requestID string, ttl time.Duration) (Lease, error) {
	if ttl <= 0 {
		ttl = 3 * time.Minute
	}
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease, ok, _ := m.tryAcquireCredentialLocked(credentialID, requestID, ttl); ok {
		return lease, nil
	}
	return Lease{}, provider.ErrNoCapacity
}

// Renew extends a live lease; false means it was already swept (P0-3).
// Streams call it per chunk so the sweep only reaps stalled streams.
func (m *LeaseManager) Renew(leaseID string, ttl time.Duration) bool {
	if ttl <= 0 {
		ttl = 3 * time.Minute
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[leaseID]
	if !ok {
		return false
	}
	lease.ExpiresAt = m.now().Add(ttl)
	m.leases[leaseID] = lease
	return true
}

// IsRegistered reports whether the credential is known to the lease manager.
// Refresh syncs the externally tracked load (P1-3) before a non-blocking try.
func (m *LeaseManager) IsRegistered(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.capacities[id]
	return ok
}

func (m *LeaseManager) Refresh(id string, active int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.capacities[id]; ok && active >= 0 {
		atomic.StoreInt32(&c.ActiveLeases, active)
	}
}

func (m *LeaseManager) turnLocked(ticket uint64, domain string) bool {
	for _, w := range m.waiters {
		if w.domain == domain {
			return w.ticket == ticket
		}
	}
	return false
}

func (m *LeaseManager) removeWaiterLocked(ticket uint64) {
	for i, w := range m.waiters {
		if w.ticket == ticket {
			m.waiters = append(m.waiters[:i], m.waiters[i+1:]...)
			return
		}
	}
}

func (m *LeaseManager) dequeueWaiter(ticket uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeWaiterLocked(ticket)
}

func (m *LeaseManager) tryAcquireCredentialLocked(credentialID, requestID string, ttl time.Duration) (Lease, bool, bool) {
	now := m.now()
	c, ok := m.capacities[credentialID]
	if !ok {
		return Lease{}, false, false
	}
	if (c.Status != identity.StatusHealthy && c.Status != identity.StatusDegraded) || now.Before(c.CooldownUntil) {
		return Lease{}, false, false
	}
	if atomic.LoadInt32(&c.ActiveLeases) >= int32(c.MaxConcurrency) {
		return Lease{}, false, true
	}
	atomic.AddInt32(&c.ActiveLeases, 1)
	lease := Lease{ID: randomID("lease"), CredentialID: c.CredentialID, Domain: c.Domain, RequestID: requestID, AcquiredAt: now, ExpiresAt: now.Add(ttl)}
	m.leases[lease.ID] = lease
	return lease, true, false
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
	if leaseID == "" {
		return ErrLeaseNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[leaseID]
	if !ok {
		// Release-after-sweep race (P0-3): idempotent on the counter side,
		// sentinel error so callers can distinguish it from real failures.
		return ErrLeaseNotFound
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
