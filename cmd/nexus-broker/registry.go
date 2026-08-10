package main

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/nexusauth"
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

	// closeStatus and closeReason record the status code and reason the conn
	// was shut down with. They are written exactly once inside shutdown's
	// once.Do, so reading them after observing closed is race-free. They let a
	// crash teardown be told apart from a graceful one without a live socket.
	closeStatus websocket.StatusCode
	closeReason string
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
		c.closeStatus = status
		c.closeReason = reason
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

	// owner is the verified identity that claimed this lease, stamped once at
	// creation from the authenticated Principal on the POST /claim request. It is
	// set-once and deliberately orthogonal to state and releasing: NO lifecycle
	// transition may clear it, because the enforcement stories compare it long
	// after a lease has moved past pending, and a cleared owner would silently
	// read as "anonymous" — i.e. as open access.
	//
	// Only owner.ID is ever compared (lease ownership is principal-id equality and
	// nothing else); the rest of the Principal is carried for the scope check on
	// the filtered listing and for the audit/durability record. When the broker
	// runs with no `auth:` block there is no principal to stamp, so this is the
	// anonymous owner (see anonymousOwner) — a well-defined zero value rather than
	// an accident, so no code path can dereference something absent.
	owner nexusauth.Principal

	// spawnSecret is the per-spawn second factor this lease's instance must echo
	// in its register frame. It is minted by the claim path (newSpawnSecret) and
	// handed to the child through the environment, so only a process THIS broker
	// started can present it — which is what the lease id alone cannot prove,
	// since a lease id travels in ws_urls, client requests and logs.
	//
	// It is deliberately write-mostly: SetSpawnSecret stores it and
	// AttachInstance compares it, and nothing else reads it. There is no getter,
	// it is absent from LeaseSnapshot (so GET /leases cannot disclose it), it is
	// never logged, and a future lease-persistence story must NOT write it to
	// disk — a secret that outlives the process it was minted for authenticates
	// nothing, because the process it belongs to is gone.
	//
	// Guarded by Registry.mu.
	spawnSecret string

	// lastActivity is the time of the most recent REAL client activity on this
	// lease — i.e. an inbound io frame flowing client → instance (user input).
	// It is NOT bumped by instance → client output, pings, or control frames, so
	// the idle sweeper measures genuine inactivity. Guarded by Registry.mu;
	// stamped via markActivity from the gateway's client read pump.
	lastActivity time.Time

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

	// hasSlot records that this lease holds a capacity slot acquired in
	// NewLease. It is freed exactly once when the lease is Removed (see
	// Remove → releaseSlotLocked), so the slot count can never drift: a slot
	// is held iff a lease exists. Guarded by Registry.mu.
	hasSlot bool

	// reason records why the lease is being (or was) torn down: "" while live,
	// then the teardown cause ("manual release", "idle", reasonCrash, …). It is
	// set under Registry.mu at the moment releasing latches, so it is a
	// non-timing signal that distinguishes an unexpected crash from a graceful
	// release. Remove uses it to pick the client's close status, and a future
	// /leases endpoint (E4-S1) can surface it.
	reason string
}

// Registry is the gateway's shared, mutex-guarded map of lease id → lease.
// Its API is intentionally small and lockable so later stories (release,
// idle, crash, capacity, list) can extend it without reworking the gateway.
type Registry struct {
	logger *slog.Logger

	// now is the clock used for lease timestamps (createdAt, lastActivity) and
	// the idle sweeper's cutoff comparison. It defaults to time.Now; tests swap
	// it for a deterministic clock so idle reaping can be exercised without real
	// sleeps.
	now func() time.Time

	// maxConcurrent is the capacity ceiling: the most live leases (and thus
	// spawned instances) the registry permits at once. A non-positive value
	// means UNLIMITED (no cap). It is read under mu by tryAcquireSlotLocked.
	maxConcurrent int

	// slotsInUse is the number of capacity slots currently held — one per live
	// lease. Guarded by mu; mutated only via tryAcquireSlotLocked /
	// releaseSlotLocked, the single slot-accounting primitives (see slots.go).
	slotsInUse int

	// waiters is the FIFO queue of claims parked because the registry is at
	// capacity (see slots.go). A freed slot is handed to the front waiter
	// rather than decrementing the count, so a longer-queued claim is never
	// barged past. Guarded by mu.
	waiters *list.List

	// tickets is the client-WebSocket ticket store whose lease-scoped
	// capabilities must die with the lease (see Remove). It is nil until wired
	// via useTicketStore, and every ticketStore method is nil-receiver safe, so
	// a registry built without one (unit tests, and the tests that predate
	// tickets) needs no branch. It is NOT guarded by mu: it owns its own lock and
	// is only ever assigned at wiring time, before anything is served.
	tickets *ticketStore

	mu     sync.Mutex
	leases map[string]*lease
}

// NewRegistry constructs an empty lease registry with the given capacity
// ceiling. A non-positive maxConcurrent means unlimited (no cap) — the
// least-surprising meaning for an unconfigured value.
func NewRegistry(logger *slog.Logger, maxConcurrent int) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		logger:        logger,
		now:           time.Now,
		maxConcurrent: maxConcurrent,
		waiters:       list.New(),
		leases:        make(map[string]*lease),
	}
}

// useTicketStore binds the ticket store whose entries Remove must invalidate.
//
// It is a setter rather than a NewRegistry parameter because the store's own
// enabled/inert state is decided by the auth guard, which is built alongside the
// registry in run() — and because threading it through the constructor would
// churn every existing registry test for a dependency none of them exercise.
// Call it once at wiring time, before the broker serves: it is not safe to call
// concurrently with Remove.
func (r *Registry) useTicketStore(s *ticketStore) { r.tickets = s }

// anonymousOwner is the owner stamped on a lease that was claimed without an
// authenticated principal — which is the normal, supported state when the broker
// runs with no `auth:` block, and also covers a claim route registered outside
// the auth guard.
//
// It is a named constructor rather than an inline nexusauth.Principal{} so
// "nobody owns this lease" is a deliberate, greppable statement with one
// definition, instead of an incidental zero value scattered across call sites.
// The zero Principal has an empty ID, and empty-ID equals empty-ID, so with auth
// disabled every lease is owned by the same anonymous identity — which is exactly
// why the enforcement stories built on top of it will behave as the broker did
// before authentication existed.
func anonymousOwner() nexusauth.Principal { return nexusauth.Principal{} }

// cloneOwner deep-copies a Principal's reference-typed fields (Scopes, Claims)
// so the registry and its callers can never mutate state the other observes. It
// is applied on the way IN (the claim handler keeps its own Principal, which must
// not be able to change a lease's owner afterwards) and on the way OUT (an
// enforcement check must not be handed a live map into registry-guarded state) —
// the same values-only discipline Snapshot follows.
//
// nexusauth.Principal has an equivalent clone method, but it is unexported and
// cmd/nexus-broker is package main, so it cannot be reached from here. Keep this
// in sync if Principal gains another reference-typed field.
func cloneOwner(p nexusauth.Principal) nexusauth.Principal {
	out := p
	if p.Scopes != nil {
		out.Scopes = make([]string, len(p.Scopes))
		copy(out.Scopes, p.Scopes)
	}
	if p.Claims != nil {
		out.Claims = make(map[string]any, len(p.Claims))
		for k, v := range p.Claims {
			out.Claims[k] = v
		}
	}
	return out
}

// NewLease creates a fresh, pending lease with a randomly generated id owned by
// owner, and returns the id, acquiring a capacity slot WITHOUT waiting. At
// capacity it returns errNoCapacity immediately and consumes no slot.
// NewLeaseQueued is the claim path's entry point (it adds bounded FIFO waiting on
// top); NewLease is the non-waiting primitive used by tests and any caller that
// wants the old immediate-rejection semantics.
//
// Pass anonymousOwner() when there is no authenticated principal.
func (r *Registry) NewLease(owner nexusauth.Principal) (string, error) {
	id, err := newLeaseID()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Acquire a capacity slot BEFORE the lease exists, so a claim can never
	// spawn an instance past max_concurrent. The slot is bound to the lease
	// (hasSlot) and freed exactly once when the lease is Removed, so every
	// teardown path AND every claim error path frees it simply by removing the
	// lease — no separate slot bookkeeping to leak. At capacity this returns
	// errNoCapacity and no slot is consumed; the claim handler maps it to 503.
	if !r.tryAcquireSlotLocked() {
		return "", errNoCapacity
	}
	if err := r.insertLeaseLocked(id, owner); err != nil {
		r.releaseSlotLocked()
		return "", err
	}
	return id, nil
}

// NewLeaseQueued creates a fresh, pending lease owned by owner, waiting in FIFO
// order for a capacity slot if the registry is at max_concurrent. E3-S2's POST
// /claim calls it instead of NewLease so an over-cap claim queues rather than
// failing outright. The slot is reserved on the SAME counter as NewLease — the
// queue adds no second accounting path.
//
// owner is carried through untouched rather than consulted: the slot is still
// acquired BEFORE the lease exists, so ownership changes nothing about capacity
// accounting or the order of the errNoCapacity / errQueueTimeout / cancelled
// branches. Pass anonymousOwner() when there is no authenticated principal.
//
// Returns errNoCapacity if the cap is full and timeout <= 0 (no waiting),
// errQueueTimeout if the wait exceeds timeout, or the wrapped ctx error if the
// request context is cancelled while queued. On any error no slot is held.
func (r *Registry) NewLeaseQueued(ctx context.Context, timeout time.Duration, owner nexusauth.Principal) (string, error) {
	id, err := newLeaseID()
	if err != nil {
		return "", err
	}
	if err := r.acquireSlot(ctx, timeout); err != nil {
		return "", err
	}
	r.mu.Lock()
	insertErr := r.insertLeaseLocked(id, owner)
	r.mu.Unlock()
	if insertErr != nil {
		// We hold a slot we could not bind to a lease; hand it back to the
		// queue (or decrement) so nothing leaks.
		r.releaseSlot()
		return "", insertErr
	}
	return id, nil
}

// insertLeaseLocked inserts a fresh pending lease for an ALREADY-acquired
// capacity slot, stamped with its owner. Caller MUST hold r.mu and MUST have
// reserved a slot (the lease is created with hasSlot=true, never incrementing the
// counter itself). Returns an error on the astronomically unlikely id collision.
//
// The owner is stamped here — the single place a lease comes into existence — so
// there is no window in which a live lease has no owner recorded.
func (r *Registry) insertLeaseLocked(id string, owner nexusauth.Principal) error {
	if _, exists := r.leases[id]; exists {
		return fmt.Errorf("lease id collision: %s", id)
	}
	now := r.now()
	r.leases[id] = &lease{
		id:              id,
		state:           leaseStatePending,
		createdAt:       now,
		owner:           cloneOwner(owner),
		lastActivity:    now,
		ready:           make(chan struct{}),
		sessionReported: make(chan struct{}),
		exited:          make(chan struct{}),
		hasSlot:         true,
	}
	return nil
}

// markActivity stamps a lease's last-activity time with the registry clock. The
// gateway calls it ONLY for real client input — inbound io frames flowing
// client → instance — so the idle sweeper resets on genuine user activity and
// not on instance output, pings, or control frames. It is a no-op for an
// unknown lease.
func (r *Registry) markActivity(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.leases[id]; ok {
		l.lastActivity = r.now()
	}
}

// idleLeases returns the ids of every live lease whose last client activity is
// strictly before cutoff and which is not already being torn down. The idle
// sweeper computes cutoff as now-idle_timeout and releases the returned ids.
func (r *Registry) idleLeases(cutoff time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id, l := range r.leases {
		if l.releasing {
			continue
		}
		if l.lastActivity.Before(cutoff) {
			ids = append(ids, id)
		}
	}
	return ids
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

// LeaseOwner returns the identity that claimed a lease and reports whether the
// lease is known. It is the single read path the enforcement stories use: an
// ownership check compares the caller's principal ID against the returned
// Principal's ID, and nothing else.
//
// The distinction between (anonymousOwner(), true) and (anonymousOwner(), false)
// matters and must not be collapsed: the first is a real lease claimed with no
// authentication, the second is a lease that does not exist. A caller that
// ignores ok would treat an unknown lease id as an anonymously-owned one.
//
// It follows Snapshot's discipline — the lock is taken, values are copied out,
// and cloneOwner ensures no internal slice or map escapes — so the returned
// Principal is safe to read and mutate without the lock.
func (r *Registry) LeaseOwner(id string) (nexusauth.Principal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return anonymousOwner(), false
	}
	return cloneOwner(l.owner), true
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

// SetSpawnSecret records the secret this lease's instance must echo in its
// register frame. The claim path calls it BEFORE the runner is invoked, so the
// expected value exists from the moment a process could possibly dial back — a
// secret recorded after the spawn would leave a window in which a fast instance
// registers against an empty expectation.
//
// It is a no-op for an unknown lease. It does NOT log the value.
func (r *Registry) SetSpawnSecret(id, secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.leases[id]; ok {
		l.spawnSecret = secret
	}
}

// Sentinel reasons an instance's register frame is refused. They exist so the
// gateway can tell the two apart in its LOG — a missing secret is the signature
// of an instance binary that predates the protocol, a wrong one is the signature
// of something impersonating an instance — while answering both, and an unknown
// lease, with the byte-identical close (see handleInstance).
var (
	// errSpawnSecretAbsent means the register frame carried no secret at all
	// while the broker requires one.
	errSpawnSecretAbsent = errors.New("register frame carried no spawn secret")

	// errSpawnSecretMismatch means the register frame carried a secret that is
	// not the one this broker minted for the lease.
	errSpawnSecretMismatch = errors.New("register frame spawn secret does not match")
)

// AttachInstance binds an instance's dial-back connection to a known lease.
// It fails if the lease is unknown, already has an instance connection, or —
// when enforce is set — presented a spawn secret that is not the one minted for
// the lease. Those are how the gateway rejects an unrecognized register frame.
//
// enforce is the config gate: the caller passes true only when the broker has an
// `auth:` block. With it false the presented secret is ignored entirely, so an
// instance binary that predates the spawn-secret protocol keeps registering
// exactly as it did before — the backward-compatibility guarantee, expressed as
// "do not run the check" rather than "run a check that always passes".
//
// The comparison is constant-time. The presented value never reaches a log or a
// response, so a timing signal would be the only channel available to someone
// guessing it, and closing it costs one function call.
func (r *Registry) AttachInstance(id string, conn *wsConn, presentedSecret string, enforce bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return fmt.Errorf("unknown lease: %s", id)
	}
	if enforce {
		if presentedSecret == "" {
			return errSpawnSecretAbsent
		}
		if subtle.ConstantTimeCompare([]byte(presentedSecret), []byte(l.spawnSecret)) != 1 {
			return errSpawnSecretMismatch
		}
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

// Remove tears a lease down: it closes both connections (if any), invalidates
// every client-WebSocket ticket issued for the lease, and drops the lease from
// the map. Safe to call on an unknown or already-removed lease.
//
// Ticket invalidation lives HERE rather than in releaseLease because Remove is
// the one point every teardown converges on. releaseLease ends here, and crash
// detection (watchExit) calls Remove DIRECTLY, bypassing releaseLease entirely —
// so a hook on releaseLease would leave a crashed lease's tickets redeemable for
// the rest of their TTL, which is exactly the gap the invalidation exists to
// close.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	l, ok := r.leases[id]
	if ok {
		l.state = leaseStateClosed
		// Free the lease's capacity slot exactly once. Map deletion below
		// guarantees a second Remove finds nothing, so the slot is never
		// double-freed; clearing hasSlot is belt-and-braces.
		if l.hasSlot {
			l.hasSlot = false
			r.releaseSlotLocked()
		}
		delete(r.leases, id)
	}
	r.mu.Unlock()

	// Every ticket issued for this lease dies with it. Called UNCONDITIONALLY,
	// outside the registry lock: it is idempotent, the ticket store owns its own
	// mutex (so nesting the two locks buys nothing but a deadlock risk), and
	// gating it on `ok` would reintroduce a window where a concurrent second
	// Remove — the one that finds nothing — is also the one that skips
	// invalidation.
	r.tickets.invalidateLease(id)

	if !ok {
		return
	}
	if l.instance != nil {
		l.instance.shutdown(websocket.StatusGoingAway, "lease closed")
	}
	if l.client != nil {
		// The client's close status is derived from the teardown reason so a
		// connected client can tell a crash apart from a normal release. The
		// reason was set under mu before Remove was called; the lock/unlock
		// above is the barrier that publishes it.
		status, reason := clientCloseForReason(l.reason)
		l.client.shutdown(status, reason)
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
