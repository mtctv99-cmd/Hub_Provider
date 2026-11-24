package gateway

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"sync"
	"time"
)

// trafficFlushInterval is how often pending traffic counters hit SQLite.
// Metrics may lag this much in the dashboard; a crash loses at most one window.
const trafficFlushInterval = 5 * time.Second

type TrafficStore struct {
	db *sql.DB

	mu          sync.Mutex
	pendingHour map[int64][4]int64 // hour_start -> {requests, duration_ms, tokens, audio_sec}
	pendingMin  map[int64]int64
	closed      bool
	stop        chan struct{}
	done        chan struct{}
}

func NewTrafficStore(db *sql.DB) *TrafficStore {
	s := &TrafficStore{
		db:          db,
		pendingHour: make(map[int64][4]int64),
		pendingMin:  make(map[int64]int64),
	}
	if db != nil {
		_, _ = db.Exec(`ALTER TABLE request_traffic_hourly ADD COLUMN tokens INTEGER NOT NULL DEFAULT 0`)
		_, _ = db.Exec(`ALTER TABLE request_traffic_hourly ADD COLUMN audio_sec INTEGER NOT NULL DEFAULT 0`)
		_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS request_traffic_minutely (
			minute_start INTEGER PRIMARY KEY,
			request_count INTEGER NOT NULL DEFAULT 0
		)`)
		s.stop = make(chan struct{})
		s.done = make(chan struct{})
		go s.flushLoop()
	}
	return s
}

func (s *TrafficStore) flushLoop() {
	defer close(s.done)
	ticker := time.NewTicker(trafficFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = s.Flush(context.Background())
		case <-s.stop:
			return
		}
	}
}

// Close stops the flush goroutine and persists what is still pending.
func (s *TrafficStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	alreadyClosed := s.closed
	s.closed = true
	s.mu.Unlock()
	if alreadyClosed {
		return
	}
	if s.stop != nil {
		close(s.stop)
		<-s.done
	}
	_ = s.Flush(context.Background())
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
	// Fold pending counters in first so callers never read stale numbers.
	_ = s.Flush(ctx)
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

func (s *TrafficStore) Record(_ context.Context, at time.Time, duration time.Duration) error {
	s.recordHourly(at, 1, duration, 0, 0)
	return nil
}

func (s *TrafficStore) RecordMetrics(_ context.Context, at time.Time, duration time.Duration, tokens int64, audioSec int64) error {
	reqCount := int64(0)
	if duration > 0 {
		reqCount = 1
	}
	s.recordHourly(at, reqCount, duration, tokens, audioSec)
	return nil
}

// recordHourly buffers counters in memory; Flush writes them in one transaction.
func (s *TrafficStore) recordHourly(at time.Time, requestCount int64, duration time.Duration, tokens int64, audioSec int64) {
	if s == nil || s.db == nil {
		return
	}
	hour := at.UTC().Truncate(time.Hour).Unix()
	minute := at.UTC().Truncate(time.Minute).Unix()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		// Requests in flight during shutdown: the final flush cannot see new maps,
		// so write straight through instead of dropping the sample.
		s.writeDirect(hour, minute, requestCount, duration, tokens, audioSec)
		return
	}
	h := s.pendingHour[hour]
	h[0] += requestCount
	h[1] += duration.Milliseconds()
	h[2] += tokens
	h[3] += audioSec
	s.pendingHour[hour] = h
	s.pendingMin[minute] += requestCount
	s.mu.Unlock()
}

// Flush writes all buffered counters in a single transaction. A failed write
// merges the batch back so the next tick retries instead of losing metrics.
func (s *TrafficStore) Flush(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	hours, mins := s.pendingHour, s.pendingMin
	s.pendingHour = make(map[int64][4]int64)
	s.pendingMin = make(map[int64]int64)
	s.mu.Unlock()
	if len(hours) == 0 && len(mins) == 0 {
		return nil
	}
	// Independent of the caller: a disconnected client must not cancel the write
	// after the batch was already taken out of the pending maps.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.writeBatch(ctx, hours, mins); err != nil {
		s.mergeBack(hours, mins)
		log.Printf("[traffic] flush failed, %d hourly/%d minutely buckets requeued: %v", len(hours), len(mins), err)
		return err
	}
	return nil
}

func (s *TrafficStore) mergeBack(hours map[int64][4]int64, mins map[int64]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hour, v := range hours {
		h := s.pendingHour[hour]
		h[0] += v[0]
		h[1] += v[1]
		h[2] += v[2]
		h[3] += v[3]
		s.pendingHour[hour] = h
	}
	for minute, count := range mins {
		s.pendingMin[minute] += count
	}
}

func (s *TrafficStore) writeBatch(ctx context.Context, hours map[int64][4]int64, mins map[int64]int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for hour, v := range hours {
		if _, err := tx.ExecContext(ctx, `INSERT INTO request_traffic_hourly(hour_start, request_count, duration_ms, tokens, audio_sec)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT(hour_start) DO UPDATE SET
				request_count=request_count+excluded.request_count,
				duration_ms=duration_ms+excluded.duration_ms,
				tokens=tokens+excluded.tokens,
				audio_sec=audio_sec+excluded.audio_sec`,
			hour, v[0], v[1], v[2], v[3]); err != nil {
			return err
		}
	}
	for minute, count := range mins {
		if _, err := tx.ExecContext(ctx, `INSERT INTO request_traffic_minutely(minute_start, request_count)
			VALUES (?, ?) ON CONFLICT(minute_start) DO UPDATE SET request_count=request_count+excluded.request_count`,
			minute, count); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// writeDirect is the post-Close fallback: one immediate transaction, no buffering.
func (s *TrafficStore) writeDirect(hour, minute int64, requestCount int64, duration time.Duration, tokens int64, audioSec int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.writeBatch(ctx,
		map[int64][4]int64{hour: {requestCount, duration.Milliseconds(), tokens, audioSec}},
		map[int64]int64{minute: requestCount}); err != nil {
		log.Printf("[traffic] post-close write failed: %v", err)
	}
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
	// Persist buffered counters before MIN/SUM so the dashboard sees fresh data.
	_ = h.Traffic.Flush(r.Context())
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
