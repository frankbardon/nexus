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
func (c *wsConn) shutdown(status websocket.StatusCode, reason string) {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close(status, reason)
	})
}

// lease holds the paired connections plus bookkeeping for a single claimed
// instance. Fields are guarded by Registry.mu — never read or mutate them
// outside a Registry method.
type lease struct {
	id        string
	state     leaseState
	createdAt time.Time
	instance  *wsConn
	client    *wsConn
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
		id:        id,
		state:     leaseStatePending,
		createdAt: time.Now(),
	}
	return id, nil
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
