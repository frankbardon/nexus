package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// leaseState is the lifecycle phase of a lease within the gateway registry.
// Later stories (release, idle, crash, capacity) extend this enum and the
// transitions around it.
type leaseState string

const (
	// leaseStatePending means the lease has been created (e.g. by POST
	// /claim in E1-S4) but no instance has dialed back to register yet.
	leaseStatePending leaseState = "pending"

	// leaseStateRegistered means an instance has dialed back and bound its
	// connection to the lease, but no client is attached.
	leaseStateRegistered leaseState = "registered"

	// leaseStateActive means both a client and an instance connection are
	// attached and frames can flow in both directions.
	leaseStateActive leaseState = "active"

	// leaseStateClosed means the lease has been torn down. It is removed
	// from the registry on close; the state exists for clarity in logs.
	leaseStateClosed leaseState = "closed"
)

// wsConn wraps a single WebSocket peer (a client or an instance) with a
// buffered send queue and a single-fire close. The gateway runs a write pump
// that drains send and a read pump that forwards inbound frames to the peer.
type wsConn struct {
	conn   *websocket.Conn
	send   chan []byte
	closed chan struct{}
	once   sync.Once
}

// newWSConn wraps an accepted WebSocket connection.
func newWSConn(conn *websocket.Conn) *wsConn {
	return &wsConn{
		conn:   conn,
		send:   make(chan []byte, 256),
		closed: make(chan struct{}),
	}
}

// queue enqueues a frame for the write pump without blocking. It returns
// false if the send buffer is full (slow/dead peer) so the caller can drop
// the frame rather than stalling the whole gateway.
func (c *wsConn) queue(data []byte) bool {
	select {
	case <-c.closed:
		return false
	case c.send <- data:
		return true
	default:
		return false
	}
}

// shutdown closes the underlying WebSocket exactly once with the given status.
// A nil conn is tolerated so leases can be torn down in tests without a live
// socket.
func (c *wsConn) shutdown(status websocket.StatusCode, reason string) {
	c.once.Do(func() {
		close(c.closed)
		if c.conn != nil {
			_ = c.conn.Close(status, reason)
		}
	})
}

// lease holds the paired connections plus bookkeeping for a single claimed
// instance. Fields are guarded by Registry.mu — never read or mutate them
// outside a Registry method (with the documented exception of ready/readyOnce,
// which are signalled via MarkReady and observed through the channel returned
// by ReadyChan).
type lease struct {
	id        string
	state     leaseState
	createdAt time.Time
	instance  *wsConn
	client    *wsConn

	// ready is closed exactly once (via MarkReady) when the spawned instance
	// signals it has booted and can accept IO. POST /claim blocks on this
	// channel before responding so callers never connect before the engine
	// is live. The channel is created in NewLease so it is always non-nil.
	ready     chan struct{}
	readyOnce sync.Once

	// sessionID is the engine session id the instance reported via a
	// session-id-report frame. For a new session this is the engine's
	// generated id (returned to the caller so it can -recall later); for a
	// resume it echoes the requested id. sessionReported is closed exactly
	// once (via MarkSessionID) when the report arrives, so POST /claim can
	// wait briefly for it. Both are created in NewLease so they are non-nil.
	sessionID       string
	sessionReported chan struct{}
	sessionOnce     sync.Once

	// process is the broker's handle on the spawned instance process. It is
	// stored so later stories (release, crash, capacity) can manage the
	// process lifecycle; pid is cached for logging and inspection.
	process processHandle
	pid     int

	// exited is closed exactly once (via the single reaper goroutine started
	// in SetProcess) after the instance process has been wait()ed and reaped.
	// exitErr holds that wait()'s result, valid once exited is closed. Both
	// claim (waiting for an early exit / reaping after a kill) and release
	// (waiting out the graceful grace period) observe exited; routing every
	// wait() through one goroutine avoids calling wait() twice on a process.
	// exited is created in NewLease so it is always non-nil.
	exited   chan struct{}
	exitErr  error
	exitOnce sync.Once

	// releasing latches once a teardown (manual release, idle, crash) has
	// begun for this lease, so a concurrent release is a clean no-op rather
	// than a double shutdown/kill.
	releasing bool
}

// Registry is the gateway's shared, mutex-guarded map of lease id → lease.
// Its API is intentionally small and lockable so later stories (release,
// idle, crash, capacity, list) can extend it without reworking the gateway.
type Registry struct {
	logger *slog.Logger

	mu     sync.Mutex
	leases map[string]*lease
}

// NewRegistry constructs an empty lease registry.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		logger: logger,
		leases: make(map[string]*lease),
	}
}

// NewLease creates a fresh, pending lease with a randomly generated id and
// returns the id. E1-S4's POST /claim calls this before spawning the
// instance, then hands the id to the instance so its dial-back register
// frame matches. Use ClientWSPath(id) to learn where the client connects.
func (r *Registry) NewLease() (string, error) {
	id, err := newLeaseID()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.leases[id]; exists {
		return "", fmt.Errorf("lease id collision: %s", id)
	}
	r.leases[id] = &lease{
		id:              id,
		state:           leaseStatePending,
		createdAt:       time.Now(),
		ready:           make(chan struct{}),
		sessionReported: make(chan struct{}),
		exited:          make(chan struct{}),
	}
	return id, nil
}

// MarkReady closes the lease's readiness channel exactly once. The gateway
// calls it when an instance's dial-back connection delivers a ready signal,
// unblocking the POST /claim handler waiting in ReadyChan. It is a no-op for
// an unknown lease or a lease already marked ready.
func (r *Registry) MarkReady(id string) {
	r.mu.Lock()
	l, ok := r.leases[id]
	r.mu.Unlock()
	if !ok {
		return
	}
	l.readyOnce.Do(func() { close(l.ready) })
}

// ReadyChan returns the lease's readiness channel, closed once the instance
// signals ready. It returns nil for an unknown lease; a select on a nil
// channel blocks forever, so callers should also guard with a timeout.
func (r *Registry) ReadyChan(id string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return nil
	}
	return l.ready
}

// MarkSessionID records the engine session id an instance reported via a
// session-id-report frame and closes the lease's sessionReported channel
// exactly once, unblocking any POST /claim handler waiting in
// SessionReportedChan. It is a no-op for an unknown lease.
func (r *Registry) MarkSessionID(id, sessionID string) {
	r.mu.Lock()
	l, ok := r.leases[id]
	if ok {
		l.sessionID = sessionID
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	l.sessionOnce.Do(func() { close(l.sessionReported) })
}

// SessionReportedChan returns the lease's session-report channel, closed once
// the instance reports its session id. It returns nil for an unknown lease; a
// select on a nil channel blocks forever, so callers should also guard with a
// timeout.
func (r *Registry) SessionReportedChan(id string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return nil
	}
	return l.sessionReported
}

// SessionID returns the engine session id reported for a lease, or "" if the
// lease is unknown or has not reported yet.
func (r *Registry) SessionID(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return ""
	}
	return l.sessionID
}

// SetProcess records the spawned instance's process handle on a lease and
// starts the single reaper goroutine that wait()s the process exactly once,
// stores its exit error, and closes the lease's exited channel. Both the claim
// path and the release path observe that channel, so the process is wait()ed
// in exactly one place. It is a no-op for an unknown lease or a nil handle.
func (r *Registry) SetProcess(id string, p processHandle) {
	r.mu.Lock()
	l, ok := r.leases[id]
	if !ok {
		r.mu.Unlock()
		return
	}
	l.process = p
	if p != nil {
		l.pid = p.pid()
	}
	exited := l.exited
	r.mu.Unlock()
	if p == nil {
		return
	}
	go func() {
		err := p.wait()
		r.mu.Lock()
		l.exitErr = err
		r.mu.Unlock()
		l.exitOnce.Do(func() { close(exited) })
	}()
}

// ExitedChan returns the lease's exit channel, closed once the instance
// process has been reaped. It returns nil for an unknown lease; a select on a
// nil channel blocks forever, so callers should also guard with a timeout or a
// known-lease check.
func (r *Registry) ExitedChan(id string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return nil
	}
	return l.exited
}

// ExitErr returns the instance process's wait() error for a lease, valid once
// ExitedChan is closed. It returns nil for an unknown lease or one that has
// not exited yet.
func (r *Registry) ExitErr(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return nil
	}
	return l.exitErr
}

// PID returns the OS process id tracked for a lease, or 0 if the lease is
// unknown or has no process recorded yet.
func (r *Registry) PID(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return 0
	}
	return l.pid
}

// AttachInstance binds an instance's dial-back connection to a known lease.
// It fails if the lease is unknown or already has an instance connection,
// which is how the gateway rejects an unrecognized register frame.
func (r *Registry) AttachInstance(id string, conn *wsConn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return fmt.Errorf("unknown lease: %s", id)
	}
	if l.instance != nil {
		return fmt.Errorf("lease %s already has an instance connection", id)
	}
	l.instance = conn
	if l.client != nil {
		l.state = leaseStateActive
	} else {
		l.state = leaseStateRegistered
	}
	return nil
}

// AttachClient binds a client connection to a known lease. It fails if the
// lease is unknown or already has a client connection (single client per
// lease for now).
func (r *Registry) AttachClient(id string, conn *wsConn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return fmt.Errorf("unknown lease: %s", id)
	}
	if l.client != nil {
		return fmt.Errorf("lease %s already has a client connection", id)
	}
	l.client = conn
	if l.instance != nil {
		l.state = leaseStateActive
	}
	return nil
}

// DetachInstance clears the instance connection from a lease (e.g. on the
// instance's read pump exiting). It leaves the lease in place so a client
// sees a clean teardown; later stories decide reconnect vs removal.
func (r *Registry) DetachInstance(id string, conn *wsConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return
	}
	if l.instance == conn {
		l.instance = nil
		l.state = leaseStateRegistered
	}
}

// DetachClient clears the client connection from a lease.
func (r *Registry) DetachClient(id string, conn *wsConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return
	}
	if l.client == conn {
		l.client = nil
		if l.instance != nil {
			l.state = leaseStateRegistered
		}
	}
}

// ClientConn returns the client connection bound to a lease, or nil if the
// lease is unknown or has no client attached.
func (r *Registry) ClientConn(id string) *wsConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return nil
	}
	return l.client
}

// InstanceConn returns the instance connection bound to a lease, or nil if
// the lease is unknown or has no instance attached.
func (r *Registry) InstanceConn(id string) *wsConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return nil
	}
	return l.instance
}

// Has reports whether a lease id is known to the registry.
func (r *Registry) Has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.leases[id]
	return ok
}

// Remove tears a lease down: it closes both connections (if any) and drops
// the lease from the map. Safe to call on an unknown or already-removed
// lease.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	l, ok := r.leases[id]
	if ok {
		l.state = leaseStateClosed
		delete(r.leases, id)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	if l.instance != nil {
		l.instance.shutdown(websocket.StatusGoingAway, "lease closed")
	}
	if l.client != nil {
		l.client.shutdown(websocket.StatusGoingAway, "lease closed")
	}
}

// newLeaseID returns a 128-bit random hex lease id.
func newLeaseID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating lease id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
