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
	TTFT      time.Duration
	Outcome   string
	ErrorKind string
	Detail    string
}

type AuditLogger struct {
	db *sql.DB
}

func NewAuditLogger(db *sql.DB) *AuditLogger { return &AuditLogger{db: db} }

// inferProvider fills the provider column for events recorded by handlers that
// only know the public model id. The dispatcher owns the authoritative provider;
// this is a best-effort attribution for dashboards and retention analysis.
func inferProvider(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "gemini"):
		return "gemini_web"
	case strings.Contains(m, "gpt"), strings.Contains(m, "copilot"):
		return "copilot_ws"
	case strings.Contains(m, "deepseek"):
		return "deepseek_web"
	case strings.Contains(m, "tiktok"), strings.HasPrefix(m, "bv0"):
		return "tiktok_api"
	case strings.HasPrefix(m, "piper:"):
		return "piper_local"
	case strings.Contains(m, "colab"), strings.Contains(m, "gemma"):
		return "google_colab"
	case strings.Contains(m, "muse"):
		return "meta_web"
	default:
		return ""
	}
}

func (l *AuditLogger) Record(ctx context.Context, event AuditEvent) error {
	if l == nil || l.db == nil {
		return fmt.Errorf("audit logger unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// P0-7: detach khỏi request context để timeout/cancel không nuốt audit.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	detail := strings.TrimSpace(event.Detail)
	if len(detail) > 1000 {
		detail = detail[:1000]
	}
	outcome := strings.TrimSpace(event.Outcome)
	if outcome == "" {
		if event.ErrorKind == "" {
			outcome = "success"
		} else {
			outcome = "error"
		}
	}
	provider := strings.TrimSpace(event.Provider)
	if provider == "" {
		provider = inferProvider(event.Model)
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO observability_events
        (request_id, event_type, provider, model, account_id, latency_ms, ttft_ms, outcome, error_kind, detail)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(event.RequestID), strings.TrimSpace(event.EventType),
		provider, strings.TrimSpace(event.Model),
		strings.TrimSpace(event.AccountID), event.Latency.Milliseconds(),
		event.TTFT.Milliseconds(), outcome,
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
