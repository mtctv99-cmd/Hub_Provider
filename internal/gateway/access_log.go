package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// accessLogInfo is filled in by the middleware and optionally enriched by
// handlers once they have decoded the request. Only non-sensitive fields are
// kept: never request bodies, headers, query strings, or credentials.
type accessLogInfo struct {
	Model    string
	APIKeyID string
}

type accessLogContextKey struct{}

func accessLogInfoFrom(r *http.Request) *accessLogInfo {
	if r == nil {
		return nil
	}
	info, _ := r.Context().Value(accessLogContextKey{}).(*accessLogInfo)
	return info
}

// setAccessLogModel records the model name for the access log line.
func setAccessLogModel(r *http.Request, model string) {
	if info := accessLogInfoFrom(r); info != nil {
		info.Model = strings.TrimSpace(model)
	}
}

func withAccessLogInfo(ctx context.Context, info *accessLogInfo) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, accessLogContextKey{}, info)
}

type accessLogEntry struct {
	Timestamp  string `json:"ts"`
	RequestID  string `json:"request_id"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Bytes      int64  `json:"bytes"`
	Model      string `json:"model,omitempty"`
	APIKeyID   string `json:"api_key_id,omitempty"`
}

func writeAccessLog(responseWriter http.ResponseWriter, request *http.Request, startedAt time.Time, info *accessLogInfo, status int, bytes int64, hijacked bool) {
	if request == nil {
		return
	}
	if !shouldLogAccess(request) {
		return
	}
	requestID := ""
	if responseWriter != nil {
		requestID = responseWriter.Header().Get("X-Request-ID")
	}
	entry := accessLogEntry{
		Timestamp:  startedAt.UTC().Format(time.RFC3339Nano),
		RequestID:  requestID,
		Method:     request.Method,
		Path:       normalizeAccessPath(request.URL.Path),
		Status:     status,
		DurationMS: time.Since(startedAt).Milliseconds(),
		Bytes:      bytes,
	}
	if hijacked {
		entry.Status = http.StatusSwitchingProtocols
	}
	if status == 0 && !hijacked {
		entry.Status = http.StatusOK
	}
	if info != nil {
		entry.Model = info.Model
		entry.APIKeyID = info.APIKeyID
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	log.Printf("[ACCESS] %s", encoded)
}

func shouldLogAccess(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.Method == http.MethodOptions {
		return false
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/web/") || path == "/web" {
		return false
	}
	if strings.HasPrefix(path, "/models/") {
		return false
	}
	return true
}

// normalizeAccessPath replaces high-cardinality path segments with placeholders
// so access logs stay aggregated and do not leak job/file/credential IDs.
func normalizeAccessPath(p string) string {
	switch {
	case strings.HasPrefix(p, "/v1/videos/tasks/"):
		return "/v1/videos/tasks/:id"
	case strings.HasPrefix(p, "/v1/files/"):
		return "/v1/files/:id"
	case strings.HasPrefix(p, "/api/tts/jobs/"):
		if strings.HasSuffix(p, "/audio") {
			return "/api/tts/jobs/:id/audio"
		}
		return "/api/tts/jobs/:id"
	case strings.HasPrefix(p, "/api/llm/jobs/"):
		return "/api/llm/jobs/:id"
	case strings.HasPrefix(p, "/admin/api/apikeys/delete/"),
		strings.HasPrefix(p, "/admin/api/apikeys/revoke/"),
		strings.HasPrefix(p, "/admin/api/apikeys/activate/"):
		prefix := p[:strings.LastIndex(p, "/")+1]
		return prefix + ":id"
	case strings.HasPrefix(p, "/admin/api/google/accounts/"),
		strings.HasPrefix(p, "/admin/api/google/credentials/"):
		prefix := p[:strings.LastIndex(p, "/")+1]
		return prefix + ":id"
	case strings.HasPrefix(p, "/admin/api/workers/"):
		return "/admin/api/workers/:id"
	}
	return p
}

// accessResponseWriter records the status code and byte count while preserving
// Flusher/Hijacker/ReaderFrom/Pusher behaviour required by SSE and WebSocket
// handlers.
type accessResponseWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	hijacked bool
}

func (w *accessResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *accessResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *accessResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *accessResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("gateway: underlying ResponseWriter does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}

func (w *accessResponseWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *accessResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
