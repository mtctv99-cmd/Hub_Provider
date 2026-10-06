// Package workerhub manages inbound WebSocket sessions from Colab workers.
package workerhub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"sever_provider/internal/provider"
)

const maxWorkerSlots = 32

var (
	ErrHubClosed           = errors.New("worker hub is closed")
	ErrNoWorker            = errors.New("no worker is available")
	ErrWorkerDisconnected  = errors.New("worker disconnected")
	ErrRequestCanceled     = errors.New("worker request canceled")
	ErrInvalidRegistration = errors.New("invalid worker registration")
	ErrUnauthorized        = errors.New("unauthorized worker token")
	ErrWorkerBusy          = errors.New("worker has active requests")
)

// Config controls a Hub. Zero values use conservative defaults.
type Config struct {
	// Upgrader is used by ServeHTTP. If nil, a permissive upgrader is used.
	Upgrader *websocket.Upgrader
	// HeartbeatInterval is the interval between server WebSocket ping frames.
	HeartbeatInterval time.Duration
	// HeartbeatTimeout is the time allowed for a pong frame.
	HeartbeatTimeout time.Duration
	// WriteTimeout bounds an individual WebSocket write.
	WriteTimeout time.Duration
	// AuthToken is required for inbound WebSocket workers. An empty token disables registration.
	AuthToken string
}

// Registration describes the capabilities announced by a worker.
type Registration struct {
	WorkerID         string         `json:"worker_id"`
	Model            string         `json:"model"`
	Models           []string       `json:"models,omitempty"`
	ModelConcurrency map[string]int `json:"model_concurrency,omitempty"`
	Capabilities     []string       `json:"capabilities,omitempty"`
	MaxConcurrency   int            `json:"max_concurrency"`
	Token            string         `json:"token,omitempty"`
}

// WorkerSnapshot is a point-in-time view of a connected worker.
type WorkerSnapshot struct {
	WorkerID         string         `json:"worker_id"`
	Model            string         `json:"model"`
	Models           []string       `json:"models,omitempty"`
	ModelConcurrency map[string]int `json:"model_concurrency,omitempty"`
	Capabilities     []string       `json:"capabilities,omitempty"`
	MaxConcurrency   int            `json:"max_concurrency"`
	Active           int            `json:"active"`
	ConnectedAt      time.Time      `json:"connected_at"`
	LastSeen         time.Time      `json:"last_seen"`
}

// Event is an event received from a worker for a request. Raw contains the
// original JSON object and is useful for provider-specific fields.
type Event struct {
	Type        string          `json:"type"`
	RequestID   string          `json:"request_id,omitempty"`
	Delta       string          `json:"delta,omitempty"`
	Text        string          `json:"text,omitempty"`
	Error       string          `json:"error,omitempty"`
	AudioBase64 string          `json:"audio_base64,omitempty"`
	MIME        string          `json:"mime,omitempty"`
	Done        bool            `json:"done,omitempty"`
	Raw         json.RawMessage `json:"-"`
}

// Stream carries a request's token and terminal events. Events is closed after
// a terminal event. Err receives transport, cancellation, and worker errors.
type Stream struct {
	Events <-chan Event
	Err    <-chan error
	Done   <-chan struct{}

	events chan Event
	errs   chan error
	done   chan struct{}
	once   sync.Once
}

func newStream() *Stream {
	events := make(chan Event, 32)
	errs := make(chan error, 1)
	done := make(chan struct{})
	return &Stream{Events: events, Err: errs, Done: done, events: events, errs: errs, done: done}
}

func (s *Stream) finish(err error) {
	s.once.Do(func() {
		if err != nil {
			s.errs <- err
		}
		close(s.events)
		close(s.errs)
		close(s.done)
	})
}

// OutboundRequest is an optional lower-level request form for Dispatch.
type OutboundRequest struct {
	Model   string
	Type    string
	Payload any
}

type pending struct {
	stream *Stream
	model  string
}

type worker struct {
	hub  *Hub
	conn *websocket.Conn
	info WorkerSnapshot

	mu           sync.Mutex
	closed       bool
	pending      map[string]*pending
	activeModels map[string]int
	writeMu      sync.Mutex
	done         chan struct{}
	once         sync.Once
}

// Session represents one registered worker connection.
type Session struct{ w *worker }

func (s *Session) Info() WorkerSnapshot {
	if s == nil || s.w == nil {
		return WorkerSnapshot{}
	}
	return s.w.snapshot()
}

func (s *Session) Done() <-chan struct{} {
	if s == nil || s.w == nil {
		return closedChannel()
	}
	return s.w.done
}

func (s *Session) Close() error {
	if s == nil || s.w == nil {
		return nil
	}
	s.w.close(ErrWorkerDisconnected)
	return nil
}

// Hub owns all currently connected worker sessions.
type Hub struct {
	cfg Config

	mu      sync.RWMutex
	workers map[string]*worker
	closed  bool
}

// New creates a worker hub. An optional Config keeps both New() and New(cfg)
// convenient for callers.
func New(config ...Config) *Hub {
	cfg := Config{}
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 15 * time.Second
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = 45 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	return &Hub{cfg: cfg, workers: make(map[string]*worker)}
}

// NewHub is an explicit alias for New.
func NewHub(config ...Config) *Hub { return New(config...) }

// ServeHTTP upgrades an inbound connection, reads its registration message,
// acknowledges it, and starts the session. HTTP routing remains the caller's
// responsibility.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.ServeWebSocket(w, r); err != nil {
		// Upgrade errors are still ordinary HTTP errors. After a successful
		// upgrade the connection is closed and no second response is written.
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// ServeWebSocket is the error-returning form of ServeHTTP.
func (h *Hub) ServeWebSocket(w http.ResponseWriter, r *http.Request) error {
	if h.isClosed() {
		return ErrHubClosed
	}
	if strings.TrimSpace(h.cfg.AuthToken) == "" {
		return ErrUnauthorized
	}
	upgrader := h.cfg.Upgrader
	if upgrader == nil {
		upgrader = &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(h.cfg.HeartbeatTimeout))
	var reg Registration
	if err := conn.ReadJSON(&reg); err != nil {
		_ = conn.Close()
		return fmt.Errorf("read worker registration: %w", err)
	}
	if !h.authorized(r, reg.Token) {
		_ = conn.Close()
		return ErrUnauthorized
	}
	if err := validateRegistration(&reg); err != nil {
		_ = conn.Close()
		return err
	}
	if _, err := h.register(conn, reg); err != nil {
		_ = conn.Close()
		return err
	}
	return nil
}

// Register attaches an already-upgraded WebSocket connection. Ownership of
// conn transfers to the returned session.
func (h *Hub) Register(conn *websocket.Conn, reg Registration) (*Session, error) {
	if strings.TrimSpace(h.cfg.AuthToken) == "" || subtle.ConstantTimeCompare([]byte(reg.Token), []byte(strings.TrimSpace(h.cfg.AuthToken))) != 1 {
		return nil, ErrUnauthorized
	}
	if err := validateRegistration(&reg); err != nil {
		return nil, err
	}
	return h.register(conn, reg)
}

// RegisterWorker is an alias for Register.
func (h *Hub) RegisterWorker(conn *websocket.Conn, reg Registration) (*Session, error) {
	return h.Register(conn, reg)
}

func (h *Hub) register(conn *websocket.Conn, reg Registration) (*Session, error) {
	if conn == nil {
		return nil, errors.New("worker connection is nil")
	}
	if h.isClosed() {
		return nil, ErrHubClosed
	}
	reg.MaxConcurrency = clampSlots(reg.MaxConcurrency)
	reg.Models = normalizeModels(reg.Model, reg.Models)
	modelConcurrency := make(map[string]int, len(reg.ModelConcurrency))
	for model, slots := range reg.ModelConcurrency {
		model = strings.TrimSpace(model)
		if model != "" && slots > 0 && containsModel(reg.Models, model) {
			modelConcurrency[model] = min(clampSlots(slots), reg.MaxConcurrency)
		}
	}
	if reg.Model == "" && len(reg.Models) > 0 {
		reg.Model = reg.Models[0]
	}
	now := time.Now()
	w := &worker{hub: h, conn: conn, info: WorkerSnapshot{WorkerID: reg.WorkerID, Model: reg.Model, Models: append([]string(nil), reg.Models...), ModelConcurrency: modelConcurrency, Capabilities: append([]string(nil), reg.Capabilities...), MaxConcurrency: reg.MaxConcurrency, ConnectedAt: now, LastSeen: now}, pending: make(map[string]*pending), activeModels: make(map[string]int), done: make(chan struct{})}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrHubClosed
	}
	targetID := reg.WorkerID
	if existing, ok := h.workers[targetID]; ok && existing != nil && !existing.closed {
		targetID = fmt.Sprintf("%s-%04x", targetID, time.Now().UnixNano()&0xffff)
	}
	reg.WorkerID = targetID
	w.info.WorkerID = targetID

	old := h.workers[targetID]
	h.workers[targetID] = w
	h.mu.Unlock()
	if old != nil && old != w {
		old.close(fmt.Errorf("worker %q replaced", targetID))
	}
	if err := w.send(map[string]any{"type": "registered", "worker_id": reg.WorkerID, "model": reg.Model, "max_concurrency": reg.MaxConcurrency, "model_concurrency": modelConcurrency}); err != nil {
		w.close(err)
		return nil, err
	}
	go w.readLoop()
	go w.heartbeatLoop()
	return &Session{w: w}, nil
}

// Submit sends a request to the least-loaded worker that advertises model.
// payload is merged into the outbound JSON object; request_id, model, and type
// are always supplied by the hub.
func (h *Hub) Submit(ctx context.Context, model string, payload any) (*Stream, error) {
	return h.Dispatch(ctx, OutboundRequest{Model: model, Payload: payload})
}

// Request is an alias for Submit.
func (h *Hub) Request(ctx context.Context, model string, payload any) (*Stream, error) {
	return h.Submit(ctx, model, payload)
}

// Dispatch sends a request using a custom message type when needed.
func (h *Hub) Dispatch(ctx context.Context, req OutboundRequest) (*Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h.isClosed() {
		return nil, ErrHubClosed
	}
	payload, err := objectPayload(req.Payload)
	if err != nil {
		return nil, err
	}
	if req.Type == "" {
		req.Type = "chat_request"
	}
	id := newRequestID()
	payload["type"] = req.Type
	payload["request_id"] = id
	if req.Model != "" {
		payload["model"] = req.Model
	}

	waitDeadline := time.Now().Add(60 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(waitDeadline) {
		waitDeadline = d
	}
	for {
		w, stream, hasWorker := h.reserve(req.Model, id)
		if stream != nil {
			if err := w.send(payload); err != nil {
				w.failRequest(id, err)
				return nil, err
			}
			go watchCancellation(ctx, w, id, stream)
			return stream, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !hasWorker {
			return nil, ErrNoWorker
		}
		if !time.Now().Before(waitDeadline) {
			return nil, provider.ErrNoCapacity
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(15 * time.Millisecond):
		}
	}

}

// reserve makes worker selection and slot ownership one atomic operation.
func (h *Hub) reserve(model, id string) (*worker, *Stream, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, false
	}
	w := h.chooseLocked(model)
	if w != nil {
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.closed && w.hasCapacity(model) {
			stream := newStream()
			w.pending[id] = &pending{stream: stream, model: model}
			w.info.Active++
			w.activeModels[model]++
			return w, stream, true
		}
	}
	for _, item := range h.workers {
		item.mu.Lock()
		available := !item.closed && item.canServe(model)
		item.mu.Unlock()
		if available {
			return nil, nil, true
		}
	}
	return nil, nil, false
}

func (h *Hub) chooseLocked(model string) *worker {
	var chosen *worker
	chosenActive, chosenID := 0, ""
	for _, w := range h.workers {
		w.mu.Lock()
		available := !w.closed && w.canServe(model) && w.hasCapacity(model)
		active, id := w.info.Active, w.info.WorkerID
		w.mu.Unlock()
		if !available {
			continue
		}
		if chosen == nil || active < chosenActive || (active == chosenActive && id < chosenID) {
			chosen, chosenActive, chosenID = w, active, id
		}
	}
	return chosen
}

// Snapshot returns connected workers sorted by worker ID.
func (h *Hub) Snapshot() []WorkerSnapshot {
	h.mu.RLock()
	out := make([]WorkerSnapshot, 0, len(h.workers))
	for _, w := range h.workers {
		out = append(out, w.snapshot())
	}
	h.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].WorkerID < out[j].WorkerID })
	return out
}

// List is an alias for Snapshot.
func (h *Hub) List() []WorkerSnapshot { return h.Snapshot() }

// DisconnectIdleWorker disconnects only if the worker still has no active requests.
func (h *Hub) DisconnectIdleWorker(workerID string) error {
	h.mu.RLock()
	w := h.workers[workerID]
	if w == nil {
		h.mu.RUnlock()
		return ErrNoWorker
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		h.mu.RUnlock()
		return ErrNoWorker
	}
	if len(w.pending) != 0 {
		w.mu.Unlock()
		h.mu.RUnlock()
		return ErrWorkerBusy
	}
	w.closed = true
	w.mu.Unlock()
	h.mu.RUnlock()
	w.close(ErrWorkerDisconnected)
	return nil
}

// CloseWorker disconnects a worker by ID and fails its in-flight requests.
func (h *Hub) CloseWorker(workerID string) error {
	h.mu.RLock()
	w := h.workers[workerID]
	h.mu.RUnlock()
	if w == nil {
		return ErrNoWorker
	}
	w.close(ErrWorkerDisconnected)
	return nil
}

// Close disconnects all workers and fails their in-flight requests.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	workers := make([]*worker, 0, len(h.workers))
	for _, w := range h.workers {
		workers = append(workers, w)
	}
	h.workers = make(map[string]*worker)
	h.mu.Unlock()
	for _, w := range workers {
		w.close(ErrHubClosed)
	}
	return nil
}

func (h *Hub) isClosed() bool { h.mu.RLock(); defer h.mu.RUnlock(); return h.closed }

func (w *worker) canServe(model string) bool {
	if model == "" {
		return true
	}
	for _, advertised := range w.info.Models {
		if advertised == model {
			return true
		}
	}
	return w.info.Model == model
}

func (w *worker) hasCapacity(model string) bool {
	if w.info.Active >= w.info.MaxConcurrency {
		return false
	}
	if limit, ok := w.info.ModelConcurrency[model]; ok && w.activeModels[model] >= limit {
		return false
	}
	return true
}

func (w *worker) releaseModel(model string) {
	if w.activeModels[model] > 1 {
		w.activeModels[model]--
	} else {
		delete(w.activeModels, model)
	}
}

func (w *worker) snapshot() WorkerSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	copy := w.info
	copy.Models = append([]string(nil), w.info.Models...)
	copy.ModelConcurrency = make(map[string]int, len(w.info.ModelConcurrency))
	for model, slots := range w.info.ModelConcurrency {
		copy.ModelConcurrency[model] = slots
	}
	copy.Capabilities = append([]string(nil), w.info.Capabilities...)
	return copy
}

func (w *worker) send(message any) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return ErrWorkerDisconnected
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(w.hub.cfg.WriteTimeout))
	return w.conn.WriteJSON(message)
}

func (w *worker) readLoop() {
	w.conn.SetPongHandler(func(string) error {
		w.mu.Lock()
		w.info.LastSeen = time.Now()
		w.mu.Unlock()
		return w.conn.SetReadDeadline(time.Now().Add(w.hub.cfg.HeartbeatTimeout))
	})
	for {
		_, data, err := w.conn.ReadMessage()
		if err != nil {
			w.close(fmt.Errorf("%w: %v", ErrWorkerDisconnected, err))
			return
		}
		w.mu.Lock()
		w.info.LastSeen = time.Now()
		w.mu.Unlock()
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}
		typ := rawString(envelope["type"])
		switch typ {
		case "ping":
			_ = w.send(map[string]string{"type": "pong"})
		case "pong", "":
			// Control pongs are handled by Gorilla's pong handler. Empty or
			// unknown heartbeat payloads do not belong to a request.
		default:
			w.handleEvent(envelope, typ)
		}
	}
}

func (w *worker) heartbeatLoop() {
	ticker := time.NewTicker(w.hub.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.writeMu.Lock()
			w.mu.Lock()
			closed := w.closed
			w.mu.Unlock()
			if !closed {
				_ = w.conn.SetWriteDeadline(time.Now().Add(w.hub.cfg.WriteTimeout))
				if err := w.conn.WriteControl(websocket.PingMessage, []byte("workerhub"), time.Now().Add(w.hub.cfg.WriteTimeout)); err != nil {
					w.writeMu.Unlock()
					w.close(fmt.Errorf("%w: heartbeat: %v", ErrWorkerDisconnected, err))
					return
				}
			}
			w.writeMu.Unlock()
		case <-w.done:
			return
		}
	}
}

func (w *worker) handleEvent(envelope map[string]json.RawMessage, typ string) {
	id := rawString(envelope["request_id"])
	if id == "" {
		return
	}
	w.mu.Lock()
	p := w.pending[id]
	if p == nil {
		w.mu.Unlock()
		return
	}
	terminal := typ == "token_end" || typ == "job_result" || typ == "audio_result" || typ == "error" || rawBool(envelope["done"])
	if terminal {
		delete(w.pending, id)
		w.releaseModel(p.model)
		if w.info.Active > 0 {
			w.info.Active--
		}
	}

	event := Event{Type: typ, RequestID: id, Delta: rawString(envelope["delta"]), Text: rawString(envelope["text"]), Error: rawString(envelope["error"]), AudioBase64: rawString(envelope["audio_base64"]), MIME: rawString(envelope["mime"]), Done: rawBool(envelope["done"]), Raw: append(json.RawMessage(nil), mustJSON(envelope)...)}
	if typ == "error" && event.Error == "" {
		event.Error = "worker returned an error"
	}
	// Keep enqueue/finish under the worker lock so cancellation cannot close the
	// stream between lookup and delivery. Never block the reader on a slow client.
	select {
	case p.stream.events <- event:
	default:
		if !terminal {
			delete(w.pending, id)
			w.releaseModel(p.model)
			w.info.Active--
		}
		p.stream.finish(errors.New("worker response buffer full"))
		w.mu.Unlock()
		return
	}
	if typ == "error" {
		p.stream.finish(errors.New(event.Error))
	} else if terminal {
		p.stream.finish(nil)
	}
	w.mu.Unlock()
}

func (w *worker) failRequest(id string, err error) {
	w.mu.Lock()
	p := w.pending[id]
	if p != nil {
		delete(w.pending, id)
		w.releaseModel(p.model)
		if w.info.Active > 0 {
			w.info.Active--
		}
	}
	w.mu.Unlock()
	if p != nil {
		p.stream.finish(err)
	}
}

func (w *worker) cancelRequest(id string) {
	w.mu.Lock()
	p := w.pending[id]
	if p != nil {
		delete(w.pending, id)
		w.releaseModel(p.model)
		if w.info.Active > 0 {
			w.info.Active--
		}
	}
	w.mu.Unlock()
	if p != nil {
		_ = w.send(map[string]string{"type": "cancel", "request_id": id})
		p.stream.finish(ErrRequestCanceled)
	}
}

func watchCancellation(ctx context.Context, w *worker, id string, stream *Stream) {
	select {
	case <-ctx.Done():
		w.cancelRequest(id)
	case <-stream.done:
	case <-w.done:
	}
}

func (w *worker) close(err error) {
	w.once.Do(func() {
		w.mu.Lock()
		w.closed = true
		pending := make([]*pending, 0, len(w.pending))
		for id, p := range w.pending {
			delete(w.pending, id)
			pending = append(pending, p)
		}
		w.activeModels = make(map[string]int)
		w.info.Active = 0
		close(w.done)
		w.mu.Unlock()
		_ = w.conn.Close()
		for _, p := range pending {
			p.stream.finish(err)
		}
		w.hub.mu.Lock()
		if w.hub.workers[w.info.WorkerID] == w {
			delete(w.hub.workers, w.info.WorkerID)
		}
		w.hub.mu.Unlock()
	})
}

func (h *Hub) authorized(r *http.Request, registrationToken string) bool {
	configured := strings.TrimSpace(h.cfg.AuthToken)
	if configured == "" {
		return false
	}
	candidates := []string{
		r.URL.Query().Get("token"),
		r.Header.Get("X-Worker-Token"),
		strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "),
		registrationToken,
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(configured)) == 1 {
			return true
		}
	}
	return false
}

func validateRegistration(reg *Registration) error {
	reg.WorkerID = strings.TrimSpace(reg.WorkerID)
	reg.Model = strings.TrimSpace(reg.Model)
	if reg.WorkerID == "" {
		reg.WorkerID = fmt.Sprintf("worker-%x", time.Now().UnixNano())
	}
	if reg.Model == "" && len(reg.Models) == 0 {
		return ErrInvalidRegistration
	}
	if reg.MaxConcurrency <= 0 {
		reg.MaxConcurrency = 1
	}
	return nil
}

func normalizeModels(model string, models []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(models)+1)
	for _, item := range append([]string{model}, models...) {
		item = strings.TrimSpace(item)
		if item != "" {
			if _, ok := seen[item]; !ok {
				seen[item] = struct{}{}
				out = append(out, item)
			}
		}
	}
	return out
}

func containsModel(models []string, target string) bool {
	for _, model := range models {
		if model == target {
			return true
		}
	}
	return false
}

func clampSlots(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxWorkerSlots {
		return maxWorkerSlots
	}
	return n
}

func objectPayload(payload any) (map[string]any, error) {
	if payload == nil {
		return make(map[string]any), nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil || out == nil {
		return nil, errors.New("worker request payload must be a JSON object")
	}
	return out, nil
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func rawBool(raw json.RawMessage) bool {
	var b bool
	_ = json.Unmarshal(raw, &b)
	return b
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

var requestIDCounter atomic.Uint64

func newRequestID() string {
	seq := requestIDCounter.Add(1)
	return fmt.Sprintf("req_%d_%x", time.Now().UnixNano(), seq)
}

func closedChannel() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
