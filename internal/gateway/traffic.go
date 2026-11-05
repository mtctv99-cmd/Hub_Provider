package gateway

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

type TrafficStore struct {
	db *sql.DB
}

func NewTrafficStore(db *sql.DB) *TrafficStore {
	s := &TrafficStore{db: db}
	if db != nil {
		_, _ = db.Exec(`ALTER TABLE request_traffic_hourly ADD COLUMN tokens INTEGER NOT NULL DEFAULT 0`)
		_, _ = db.Exec(`ALTER TABLE request_traffic_hourly ADD COLUMN audio_sec INTEGER NOT NULL DEFAULT 0`)
		_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS request_traffic_minutely (
			minute_start INTEGER PRIMARY KEY,
			request_count INTEGER NOT NULL DEFAULT 0
		)`)
	}
	return s
}

type trafficSummary struct {
	Requests           int64
	DurationMs         int64
	Tokens             int64
	AudioSec           int64
	RequestsThisMinute int64
}

func (s *TrafficStore) Summary(ctx context.Context, now time.Time) (trafficSummary, error) {
	var summary trafficSummary
	if s == nil || s.db == nil {
		return summary, sql.ErrConnDone
	}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(request_count),0), COALESCE(SUM(duration_ms),0),
		COALESCE(SUM(tokens),0), COALESCE(SUM(audio_sec),0) FROM request_traffic_hourly`).Scan(
		&summary.Requests, &summary.DurationMs, &summary.Tokens, &summary.AudioSec)
	if err != nil {
		return trafficSummary{}, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(request_count),0) FROM request_traffic_minutely WHERE minute_start=?`,
		now.UTC().Truncate(time.Minute).Unix()).Scan(&summary.RequestsThisMinute)
	return summary, err
}

func (s *TrafficStore) Record(ctx context.Context, at time.Time, duration time.Duration) error {
	return s.recordHourly(ctx, at, 1, duration, 0, 0)
}

func (s *TrafficStore) RecordMetrics(ctx context.Context, at time.Time, duration time.Duration, tokens int64, audioSec int64) error {
	reqCount := int64(0)
	if duration > 0 {
		reqCount = 1
	}
	return s.recordHourly(ctx, at, reqCount, duration, tokens, audioSec)
}

func (s *TrafficStore) recordHourly(ctx context.Context, at time.Time, requestCount int64, duration time.Duration, tokens int64, audioSec int64) error {
	if s == nil || s.db == nil {
		return nil
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `INSERT INTO request_traffic_hourly(hour_start, request_count, duration_ms, tokens, audio_sec)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(hour_start) DO UPDATE SET
			request_count=request_count+excluded.request_count,
			duration_ms=duration_ms+excluded.duration_ms,
			tokens=tokens+excluded.tokens,
			audio_sec=audio_sec+excluded.audio_sec`,
		at.UTC().Truncate(time.Hour).Unix(), requestCount, duration.Milliseconds(), tokens, audioSec); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO request_traffic_minutely(minute_start, request_count)
		VALUES (?, ?) ON CONFLICT(minute_start) DO UPDATE SET request_count=request_count+excluded.request_count`,
		at.UTC().Truncate(time.Minute).Unix(), requestCount); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

type trafficBucket struct {
	Start      int64 `json:"start"`
	Requests   int64 `json:"requests"`
	DurationMs int64 `json:"duration_ms"`
	Tokens     int64 `json:"tokens"`
	AudioSec   int64 `json:"audio_sec"`
}

func (h *Handler) TrafficHistory(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if h.Traffic == nil || h.Traffic.db == nil {
		writeError(w, http.StatusServiceUnavailable, "traffic history is unavailable", "server_error", "traffic_unavailable")
		return
	}
	period := r.URL.Query().Get("period")
	var since int64
	var interval int64
	now := time.Now().UTC()
	switch period {
	case "24h":
		since, interval = now.Add(-24*time.Hour).Unix(), 3600
	case "7d":
		since, interval = now.AddDate(0, 0, -7).Unix(), 86400
	case "1m":
		since, interval = now.AddDate(0, -1, 0).Unix(), 86400
	case "all":
		interval = 86400
	default:
		writeError(w, http.StatusBadRequest, "invalid traffic period", "invalid_request_error", "invalid_period")
		return
	}
	if since == 0 {
		if err := h.Traffic.db.QueryRowContext(r.Context(), `SELECT COALESCE(MIN(hour_start), 0) FROM request_traffic_hourly`).Scan(&since); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load traffic history", "server_error", "traffic_query_failed")
			return
		}
	}
	rows, err := h.Traffic.db.QueryContext(r.Context(), `SELECT (hour_start / ?) * ? AS bucket, SUM(request_count), SUM(duration_ms), SUM(COALESCE(tokens, 0)), SUM(COALESCE(audio_sec, 0))
		FROM request_traffic_hourly WHERE hour_start >= ? GROUP BY bucket ORDER BY bucket`, interval, interval, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load traffic history", "server_error", "traffic_query_failed")
		return
	}
	defer rows.Close()
	buckets := make([]trafficBucket, 0)
	var total int64
	for rows.Next() {
		var bucket trafficBucket
		if err := rows.Scan(&bucket.Start, &bucket.Requests, &bucket.DurationMs, &bucket.Tokens, &bucket.AudioSec); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load traffic history", "server_error", "traffic_query_failed")
			return
		}
		total += bucket.Requests
		buckets = append(buckets, bucket)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "failed to load traffic history", "server_error", "traffic_query_failed")
		return
	}
	from := (since / interval) * interval
	to := (now.Unix() / interval) * interval
	writeJSON(w, http.StatusOK, map[string]any{"period": period, "interval_seconds": interval, "requests": total, "buckets": buckets, "from": from, "to": to})
}
