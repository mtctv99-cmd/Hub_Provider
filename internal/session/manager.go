package session

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"sever_provider/internal/provider"
)

const (
	DefaultMaxTurns = 25
	DefaultIdleTTL  = 30 * time.Minute
)

type Binding struct {
	ID           string    `json:"session_id"`
	CredentialID string    `json:"credential_id"`
	Provider     string    `json:"provider"`
	Turns        int       `json:"turns"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	// Upstream conversation continuity (Gemini: c_/r_/rc_ IDs).
	// Stored alongside credential affinity so a repeated X-Session-ID
	// continues the same upstream conversation.
	UpstreamConversationID string `json:"upstream_conversation_id,omitempty"`
	UpstreamResponseID     string `json:"upstream_response_id,omitempty"`
	UpstreamChoiceID       string `json:"upstream_choice_id,omitempty"`
	UpstreamChatSessionID  string `json:"upstream_chat_session_id,omitempty"`
	UpstreamInvocationID   int    `json:"upstream_invocation_id,omitempty"`
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]Binding
	maxTurns int
	idleTTL  time.Duration
	now      func() time.Time
}

func New(maxTurns int, idleTTL time.Duration) *Manager {
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	if idleTTL <= 0 {
		idleTTL = DefaultIdleTTL
	}
	return &Manager{sessions: make(map[string]Binding), maxTurns: maxTurns, idleTTL: idleTTL, now: time.Now}
}

func (m *Manager) Bind(id, credentialID, provider string) (Binding, bool) {
	if id == "" || credentialID == "" {
		return Binding{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if current, ok := m.sessions[id]; ok && now.Sub(current.LastActiveAt) <= m.idleTTL && current.Turns < m.maxTurns {
		current.Turns++
		current.LastActiveAt = now
		m.sessions[id] = current
		return current, true
	}
	binding := Binding{ID: id, CredentialID: credentialID, Provider: provider, Turns: 1, CreatedAt: now, LastActiveAt: now}
	m.sessions[id] = binding
	return binding, false
}

func (m *Manager) Get(id string) (Binding, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.sessions[id]
	if !ok || m.now().Sub(b.LastActiveAt) > m.idleTTL {
		if ok {
			delete(m.sessions, id)
		}
		return Binding{}, false
	}
	return b, true
}

// SaveUpstream persists provider-native conversation IDs for an existing
// session without bumping the turn counter. It is a no-op for empty IDs
// or unknown sessions so callers can invoke it unconditionally after a
// successful upstream call.
func (m *Manager) SaveUpstream(id, conversationID, responseID, choiceID string) {
	m.SaveUpstreamState(id, provider.UpstreamState{ConversationID: conversationID, ResponseID: responseID, ChoiceID: choiceID})
}

func (m *Manager) SaveUpstreamState(id string, upstream provider.UpstreamState) {
	if id == "" || upstream.Empty() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.sessions[id]
	if !ok || m.now().Sub(current.LastActiveAt) > m.idleTTL {
		return
	}
	if upstream.ConversationID != "" {
		current.UpstreamConversationID = upstream.ConversationID
	}
	if upstream.ResponseID != "" {
		current.UpstreamResponseID = upstream.ResponseID
	}
	if upstream.ChoiceID != "" {
		current.UpstreamChoiceID = upstream.ChoiceID
	}
	if upstream.ChatSessionID != "" {
		current.UpstreamChatSessionID = upstream.ChatSessionID
	}
	if upstream.InvocationID != 0 {
		current.UpstreamInvocationID = upstream.InvocationID
	}
	current.LastActiveAt = m.now()
	m.sessions[id] = current
}

func (m *Manager) Delete(id string) { m.mu.Lock(); delete(m.sessions, id); m.mu.Unlock() }
func (m *Manager) Sweep() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	now := m.now()
	for id, b := range m.sessions {
		if now.Sub(b.LastActiveAt) > m.idleTTL {
			delete(m.sessions, id)
			n++
		}
	}
	return n
}
func (m *Manager) List() []Binding {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Binding, 0, len(m.sessions))
	for _, b := range m.sessions {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "session-" + hex.EncodeToString([]byte(time.Now().String()))
	}
	return "session-" + hex.EncodeToString(b[:])
}
