package scheduler

import (
	"sync"
	"time"
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "CLOSED"
	CircuitOpen     CircuitState = "OPEN"
	CircuitHalfOpen CircuitState = "HALF_OPEN"
)

type AccountBreaker struct {
	mu        sync.Mutex
	state     CircuitState
	failures  int
	openedAt  time.Time
	openFor   time.Duration
	threshold int
}

func NewAccountBreaker() *AccountBreaker {
	return &AccountBreaker{state: CircuitClosed, threshold: 3, openFor: time.Minute}
}
func (b *AccountBreaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == CircuitClosed {
		return true
	}
	if b.state == CircuitOpen && now.Sub(b.openedAt) >= b.openFor {
		b.state = CircuitHalfOpen
		return true
	}
	return b.state == CircuitHalfOpen
}
func (b *AccountBreaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.failures = CircuitClosed, 0
}
func (b *AccountBreaker) Failure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.threshold {
		b.state, b.openedAt = CircuitOpen, now
	}
}
func (b *AccountBreaker) State() CircuitState { b.mu.Lock(); defer b.mu.Unlock(); return b.state }

type ProviderBreaker struct {
	mu        sync.Mutex
	state     CircuitState
	failures  []time.Time
	window    time.Duration
	threshold int
	openFor   time.Duration
	openedAt  time.Time
}

func NewProviderBreaker() *ProviderBreaker {
	return &ProviderBreaker{state: CircuitClosed, window: time.Minute, threshold: 5, openFor: time.Minute}
}
func (b *ProviderBreaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(now)
	if b.state == CircuitOpen && now.Sub(b.openedAt) >= b.openFor {
		b.state = CircuitHalfOpen
		return true
	}
	return b.state == CircuitClosed || b.state == CircuitHalfOpen
}
func (b *ProviderBreaker) Failure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(now)
	b.failures = append(b.failures, now)
	if len(b.failures) >= b.threshold {
		b.state, b.openedAt = CircuitOpen, now
	}
}
func (b *ProviderBreaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.failures = CircuitClosed, nil
}
func (b *ProviderBreaker) State() CircuitState { b.mu.Lock(); defer b.mu.Unlock(); return b.state }
func (b *ProviderBreaker) prune(now time.Time) {
	cutoff := now.Add(-b.window)
	i := 0
	for i < len(b.failures) && b.failures[i].Before(cutoff) {
		i++
	}
	b.failures = b.failures[i:]
}
