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
	Priority   int
}

type Allocator struct {
	mu          sync.Mutex
	borrowLimit int
	priorities  map[string]int
}

func New() *Allocator { return &Allocator{borrowLimit: 2, priorities: make(map[string]int)} }

func (a *Allocator) SetPriority(credentialID string, priority int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.priorities == nil {
		a.priorities = make(map[string]int)
	}
	a.priorities[credentialID] = priority
}

func (a *Allocator) SetBorrowLimit(limit int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.borrowLimit = limit
}

// targetSessionCount calculates 40% of total credentials for the session pool.
func targetSessionCount(total int) int {
	if total <= 1 {
		return 0
	}
	target := (total*4 + 5) / 10
	if target < 1 {
		target = 1
	}
	if target >= total {
		target = total - 1
	}
	return target
}

// isExhausted reports whether every candidate in the pool is at max capacity.
func isExhausted(candidates []Candidate) bool {
	if len(candidates) == 0 {
		return true
	}
	for _, c := range candidates {
		limit := int32(c.Credential.MaxConcurrency)
		if limit < 1 {
			limit = 1
		}
		if c.Active < limit {
			return false
		}
	}
	return true
}

// Allocate applies W2A's 40/60 session/worker split while respecting explicit roles.
// When regular worker pool is exhausted, idle session accounts (Active == 0) step in
// to help handle worker traffic ("nếu rảnh thì qua chạy thường phụ").
func (a *Allocator) Allocate(creds []identity.Credential, session bool) []Candidate {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(creds) == 0 {
		return nil
	}

	targetRole := RoleWorker
	if session {
		targetRole = RoleSession
	}

	// Single credential: adapt to requested role.
	if len(creds) == 1 {
		c := creds[0]
		p := 0
		if a.priorities != nil {
			p = a.priorities[c.ID]
		}
		role := Role(c.PoolRole)
		if role == "" {
			role = RoleAuto
		}
		return []Candidate{{Credential: c, Configured: role, Effective: targetRole, Active: c.ActiveLeases, Priority: p}}
	}

	// 1. Separate explicit roles from AUTO roles.
	var explicitSession, explicitWorker []Candidate
	var autoCandidates []Candidate

	for _, c := range creds {
		role := Role(c.PoolRole)
		if role == "" {
			role = RoleAuto
		}
		p := 0
		if a.priorities != nil {
			p = a.priorities[c.ID]
		}
		cand := Candidate{Credential: c, Configured: role, Active: c.ActiveLeases, Priority: p}
		switch role {
		case RoleSession:
			cand.Effective = RoleSession
			explicitSession = append(explicitSession, cand)
		case RoleWorker:
			cand.Effective = RoleWorker
			explicitWorker = append(explicitWorker, cand)
		default:
			autoCandidates = append(autoCandidates, cand)
		}
	}

	// 2. Allocate 40% of accounts to Session, 60% to Worker.
	targetSessions := targetSessionCount(len(creds))
	neededSessions := targetSessions - len(explicitSession)
	if neededSessions < 0 {
		neededSessions = 0
	}
	if neededSessions > len(autoCandidates) {
		neededSessions = len(autoCandidates)
	}

	// Sort AUTO candidates by Priority desc so higher priority accounts serve sessions.
	sort.Slice(autoCandidates, func(i, j int) bool {
		return autoCandidates[i].Priority > autoCandidates[j].Priority
	})

	var sessionPool, workerPool []Candidate
	sessionPool = append(sessionPool, explicitSession...)
	workerPool = append(workerPool, explicitWorker...)

	for i, c := range autoCandidates {
		if i < neededSessions {
			c.Effective = RoleSession
			sessionPool = append(sessionPool, c)
		} else {
			c.Effective = RoleWorker
			workerPool = append(workerPool, c)
		}
	}

	// 3. Determine preferred and other pools based on targetRole.
	var preferred, other []Candidate
	if targetRole == RoleSession {
		preferred, other = sessionPool, workerPool
	} else {
		preferred, other = workerPool, sessionPool
	}

	// Sort candidates: least active leases first, then highest priority.
	sortCandidates := func(cs []Candidate) {
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].Active != cs[j].Active {
				return cs[i].Active < cs[j].Active
			}
			return cs[i].Priority > cs[j].Priority
		})
	}
	sortCandidates(preferred)
	sortCandidates(other)

	// 4. "nếu rảnh thì qua chạy thường phụ":
	// When preferred pool is exhausted, borrow idle accounts (Active == 0) from the other pool.
	if isExhausted(preferred) {
		borrowLimit := a.borrowLimit
		if borrowLimit <= 0 {
			borrowLimit = 2
		}
		borrowed := make([]Candidate, 0, borrowLimit)
		for _, c := range other {
			if c.Active == 0 {
				c.Effective = targetRole
				borrowed = append(borrowed, c)
				if len(borrowed) >= borrowLimit {
					break
				}
			}
		}
		if len(preferred) == 0 {
			return borrowed
		}
		return append(preferred, borrowed...)
	}

	return preferred
}

func (a *Allocator) AllocateCredentials(creds []identity.Credential, session bool) []identity.Credential {
	candidates := a.Allocate(creds, session)
	out := make([]identity.Credential, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.Credential)
	}
	return out
}
