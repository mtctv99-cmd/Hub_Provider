package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sever_provider/internal/provider"
	"time"
)

type gate struct {
	token chan struct{}
	refs  int
}

// Acquire serializes all turns of a conversation, including streamed turns.
func (m *Manager) Acquire(ctx context.Context, id string) (func(), error) {
	if id == "" {
		return func() {}, nil
	}
	m.mu.Lock()
	if m.gates == nil {
		m.gates = make(map[string]*gate)
	}
	g := m.gates[id]
	if g == nil {
		g = &gate{token: make(chan struct{}, 1)}
		g.token <- struct{}{}
		m.gates[id] = g
	}
	g.refs++
	m.mu.Unlock()
	drop := func() {
		m.mu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(m.gates, id)
		}
		m.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	case <-g.token:
	}
	return func() { g.token <- struct{}{}; drop() }, nil
}
func NewPersistent(database *sql.DB) (*Manager, error) {
	if database == nil {
		return nil, fmt.Errorf("session database missing")
	}
	_, err := database.Exec(`CREATE TABLE IF NOT EXISTS chat_session_bindings (id TEXT PRIMARY KEY, binding TEXT NOT NULL)`)
	if err != nil {
		return nil, err
	}
	m := New(DefaultMaxTurns, DefaultIdleTTL)
	m.db = database
	rows, err := database.Query(`SELECT binding FROM chat_session_bindings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var b Binding
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &b); err != nil {
			return nil, fmt.Errorf("invalid stored chat binding: %w", err)
		}
		m.sessions[b.ID] = b
	}
	return m, rows.Err()
}

// Commit persists continuity before success is reported to the caller.
func (m *Manager) Commit(id, credentialID, providerName string, upstream provider.UpstreamState) error {
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.sessions[id]
	if ok && (b.CredentialID != credentialID || b.Provider != providerName) {
		return fmt.Errorf("session owner mismatch")
	}
	now := m.now()
	if !ok {
		b = Binding{ID: id, CredentialID: credentialID, Provider: providerName, CreatedAt: now}
	}
	b.Turns++
	b.LastActiveAt = now
	if upstream.ConversationID != "" {
		b.UpstreamConversationID = upstream.ConversationID
	}
	if upstream.ResponseID != "" {
		b.UpstreamResponseID = upstream.ResponseID
	}
	if upstream.ChoiceID != "" {
		b.UpstreamChoiceID = upstream.ChoiceID
	}
	if upstream.ChatSessionID != "" {
		b.UpstreamChatSessionID = upstream.ChatSessionID
	}
	if upstream.InvocationID != 0 {
		b.UpstreamInvocationID = upstream.InvocationID
	}
	if upstream.MessageID != 0 {
		b.UpstreamMessageID = upstream.MessageID
	}
	if upstream.ConversationTransferToken != "" {
		b.UpstreamConversationTransferToken = upstream.ConversationTransferToken
	}
	if upstream.TurnState != "" {
		b.UpstreamTurnState = upstream.TurnState
	}
	if m.db != nil {
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err = m.db.ExecContext(ctx, `INSERT INTO chat_session_bindings(id,binding) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET binding=excluded.binding`, id, string(raw)); err != nil {
			return fmt.Errorf("persist chat session: %w", err)
		}
	}
	m.sessions[id] = b
	return nil
}
