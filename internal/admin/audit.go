package admin

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// AuditEvent captures a redacted operational event. Secret material must never be placed in Detail.
type AuditEvent struct {
	RequestID string
	EventType string
	Provider  string
	Model     string
	AccountID string
	Latency   time.Duration
	ErrorKind string
	Detail    string
}

type AuditLogger struct {
	db *sql.DB
}

func NewAuditLogger(db *sql.DB) *AuditLogger { return &AuditLogger{db: db} }

func (l *AuditLogger) Record(ctx context.Context, event AuditEvent) error {
	if l == nil || l.db == nil {
		return fmt.Errorf("audit logger unavailable")
	}
	detail := strings.TrimSpace(event.Detail)
	if len(detail) > 1000 {
		detail = detail[:1000]
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO observability_events
        (request_id, event_type, provider, model, account_id, latency_ms, error_kind, detail)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(event.RequestID), strings.TrimSpace(event.EventType),
		strings.TrimSpace(event.Provider), strings.TrimSpace(event.Model),
		strings.TrimSpace(event.AccountID), event.Latency.Milliseconds(),
		strings.TrimSpace(event.ErrorKind), detail)
	return err
}

type AuditLine struct {
	ID        int64     `json:"id"`
	EventType string    `json:"event_type"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	AccountID string    `json:"account_id,omitempty"`
	ErrorKind string    `json:"error_kind,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (l *AuditLogger) Recent(ctx context.Context, limit int) ([]AuditLine, error) {
	if l == nil || l.db == nil {
		return nil, fmt.Errorf("audit logger unavailable")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := l.db.QueryContext(ctx, `SELECT id, event_type, provider, model, account_id, error_kind, detail, created_at
        FROM observability_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AuditLine, 0)
	for rows.Next() {
		var line AuditLine
		if err := rows.Scan(&line.ID, &line.EventType, &line.Provider, &line.Model, &line.AccountID, &line.ErrorKind, &line.Detail, &line.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, line)
	}
	return result, rows.Err()
}

func (l *AuditLogger) Clear(ctx context.Context) error {
	if l == nil || l.db == nil {
		return fmt.Errorf("audit logger unavailable")
	}
	_, err := l.db.ExecContext(ctx, `DELETE FROM observability_events`)
	return err
}
