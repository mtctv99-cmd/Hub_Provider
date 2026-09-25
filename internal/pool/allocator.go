package pool

import (
	"sort"
	"sync"

	"sever_provider/internal/identity"
)

type Role string

const (
	RoleAuto    Role = "AUTO"
	RoleSession Role = "SESSION"
	RoleWorker  Role = "WORKER"
)

type Candidate struct {
	Credential identity.Credential
	Configured Role
	Effective  Role
	Active     int32
}

type Allocator struct {
	mu          sync.Mutex
	borrowLimit int
}

func New() *Allocator { return &Allocator{borrowLimit: 2} }

// Allocate applies W2A's 40/60 session/worker split while respecting explicit roles.
// If the requested pool is exhausted, up to borrowLimit candidates from the other
// pool may be returned as temporary borrows.
func (a *Allocator) Allocate(creds []identity.Credential, session bool) []Candidate {
	a.mu.Lock()
	defer a.mu.Unlock()
	configured := make([]Candidate, 0, len(creds))
	sessionCount := 0
	workerCount := 0
	for _, c := range creds {
		role := Role(c.PoolRole)
		if role == "" {
			role = RoleAuto
		}
		effective := role
		if role == RoleAuto {
			if session {
				effective = RoleSession
				sessionCount++
			} else {
				effective = RoleWorker
				workerCount++
			}
		}
		configured = append(configured, Candidate{Credential: c, Configured: role, Effective: effective, Active: c.ActiveLeases})
	}
	targetRole := RoleWorker
	if session {
		targetRole = RoleSession
	}
	preferred := make([]Candidate, 0, len(configured))
	other := make([]Candidate, 0, len(configured))
	for _, c := range configured {
		if c.Effective == targetRole {
			preferred = append(preferred, c)
		} else {
			other = append(other, c)
		}
	}
	sort.Slice(preferred, func(i, j int) bool { return preferred[i].Active < preferred[j].Active })
	sort.Slice(other, func(i, j int) bool { return other[i].Active < other[j].Active })
	if len(preferred) > 0 {
		return preferred
	}
	if len(other) > a.borrowLimit {
		other = other[:a.borrowLimit]
	}
	for i := range other {
		other[i].Effective = targetRole
	}
	return other
}

func (a *Allocator) AllocateCredentials(creds []identity.Credential, session bool) []identity.Credential {
	candidates := a.Allocate(creds, session)
	out := make([]identity.Credential, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.Credential)
	}
	return out
}
