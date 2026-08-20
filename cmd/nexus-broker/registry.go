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

	"github.com/frankbardon/nexus/pkg/brokerframe"
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

	// replayMu guards replay.
	replayMu sync.Mutex

	// replay holds frames the write pump must write BEFORE anything queued on
	// send. It is how a resuming client's buffered tail reaches the socket
	// ahead of the live stream.
	//
	// It is a separate staging slice rather than a pre-fill of send because
	// send is bounded at 256 frames while a replay is bounded in BYTES — a
	// megabyte of token deltas is thousands of frames, so priming the queue
	// would silently drop most of a replay against the very buffer that exists
	// to prevent silent loss. Staging it costs nothing: the frames are already
	// encoded and already retained by the lease's replay buffer.
	replay [][]byte

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

// primeReplay stages frames to be written ahead of everything queued on send.
// It is called once, while the registry still holds its lock and before the
// conn is published as a lease's client, so nothing can be queued in front of
// the staged frames.
func (c *wsConn) primeReplay(frames [][]byte) {
	if len(frames) == 0 {
		return
	}
	c.replayMu.Lock()
	defer c.replayMu.Unlock()
	c.replay = append(c.replay, frames...)
}

// takeReplay returns the staged frames and clears the staging slice, so the
// bytes are released as soon as the write pump has drained them.
func (c *wsConn) takeReplay() [][]byte {
	c.replayMu.Lock()
	defer c.replayMu.Unlock()
	frames := c.replay
	c.replay = nil
	return frames
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

// abort is shutdown for a peer that has already stopped answering: it records
// the same status and reason, but skips the WebSocket close handshake.
//
// The distinction is load-bearing rather than cosmetic. Conn.Close writes a
// close frame and then waits up to FIVE SECONDS for the peer to echo one, and it
// takes the connection's read lock to do so — a lock the read pump is holding
// while blocked in Read. So a graceful close against a socket whose peer is
// gone leaves the conn attached, and the lease believing it has a peer, for
// those five seconds. That is the whole bounded-detection property the liveness
// pump exists to provide, spent waiting for a reply that is not coming.
//
// CloseNow tears the underlying connection down at once, which unblocks the read
// pump immediately and runs the ordinary detach path. The peer sees an abnormal
// closure instead of a coded one, which is the honest description of what
// happened to it.
func (c *wsConn) abort(status websocket.StatusCode, reason string) {
	c.once.Do(func() {
		c.closeStatus = status
		c.closeReason = reason
		close(c.closed)
		if c.conn != nil {
			_ = c.conn.CloseNow()
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

	// The CLIENT connection is deliberately not a field here: it lives on
	// clientStream, which owns it under the same lock that assigns sequence
	// numbers. Read it through lease.clientConn. See the clientStream type
	// comment for why splitting the two was a race.

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
	// It is deliberately write-mostly: SetSpawnSecret (or RestoreLease) stores it
	// and AttachInstance compares it, and nothing else reads it. There is no
	// getter, it is absent from LeaseSnapshot (so GET /leases cannot disclose it),
	// it is never logged, and it is NEVER WRITTEN TO THE LEASE JOURNAL.
	//
	// It does, however, have to survive a broker restart, because a restored lease
	// must be able to recognise the instance that reattaches to it. That is solved
	// by DERIVATION rather than persistence: with a state_dir configured the value
	// is HMAC(broker key, lease id) instead of a random draw, so a restarted broker
	// recomputes it (see spawnKey and RestoreLease) and nothing bearer-shaped ever
	// reaches disk.
	//
	// Guarded by Registry.mu.
	spawnSecret string

	// lastActivity is the time of the most recent REAL client activity on this
	// lease — i.e. an inbound io frame flowing client → instance (user input).
	// It is NOT bumped by instance → client output, pings, or control frames, so
	// the idle sweeper measures genuine inactivity. Guarded by Registry.mu;
	// stamped via markActivity from the gateway's client read pump.
	lastActivity time.Time

	// turnStartedAt is when the instance last reported that it STARTED working
	// — the first io.status frame naming a work state after a settled period —
	// and the zero value means no turn is in flight. It is what makes "idle"
	// mean "nobody is waiting on this" rather than "nobody has typed lately":
	// a lease with a live turn is skipped by the idle sweeper however long ago
	// its last client input was, so a ten-minute autonomous turn is no longer
	// indistinguishable from an abandoned session.
	//
	// It is stamped ONCE per turn rather than refreshed on every work state, so
	// it measures the whole turn. That is deliberate and is what max_turn_duration
	// needs: a wedged instance that keeps emitting "thinking" forever would reset
	// a refreshed stamp on every frame and hold its lease indefinitely, which is
	// the exact failure the bound exists to catch.
	//
	// Guarded by Registry.mu; stamped via markTurnLive / markTurnSettled from the
	// gateway's INSTANCE read pump.
	turnStartedAt time.Time

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

	// binary is the `binaries:` registry entry this lease's instance was spawned
	// from — the NAME, not the path. The name is what identifies a variant: two
	// entries may legitimately point at the same executable, and an operator may
	// repoint an entry at a new build without that changing which variant a
	// session belongs to.
	//
	// It sits beside sessionID because the two together ARE the durable session →
	// binary mapping a resume is checked against (see BinaryForSession). Nothing
	// on the spawn path reads it — spawn already has the entry — it exists to be
	// recorded, journaled, and read back.
	//
	// Stamped once by the claim path (SetBinary) from the entry it resolved, and
	// carried through verbatim for a lease restored from the journal. Empty means
	// NOT RECORDED — a lease minted by a caller that never stamped one, or one
	// restored from a journal written before this field existed — never "the
	// binary named empty string". Guarded by Registry.mu.
	binary string

	// ioSink, when set, receives every SignalIO payload this lease's instance
	// sends, IN ADDITION to the normal client forwarding.
	//
	// It exists because the A2A ingress has no client WebSocket. A /claim client
	// is a socket the gateway relays frames to; an A2A caller is an HTTP request
	// the broker answers, so something in-process has to read the instance's
	// payloads. The gateway's instance read pump already decodes every frame, so
	// this is a hook on the decode it was doing anyway rather than a second pump.
	//
	// It does NOT displace the client conn: a lease may legitimately have both
	// (an operator attaching a client to an A2A-created lease to watch it), and
	// the sink is additive so neither observer starves the other.
	//
	// The function is invoked on the gateway's instance read-pump goroutine and
	// MUST NOT block — the mapping behind it only ever appends to buffered
	// channels. Guarded by Registry.mu for the pointer swap only; the call itself
	// happens outside the lock.
	ioSink func(brokerIOMessage)

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

	// restored marks a lease reconstructed from the journal at boot rather than
	// minted by a claim (see RestoreLease). It is what unattachedRestored filters
	// on, so the reattach reaper can bound how long a lease nothing came back for
	// keeps holding a slot.
	//
	// It no longer carries any authentication consequence. It used to: the spawn
	// secret was enforced for a restored lease even on a broker with no `auth:`
	// block, because a live pid is not identity — a pid can be reused by an
	// unrelated process between the broker dying and coming back. That reasoning
	// was always the general case rather than the restored one, and AttachInstance
	// now applies it to every registration.
	//
	// It is NOT cleared on reattach: the instance plugin reconnects on every
	// dropped socket, so a restored lease can register more than once, and each
	// registration must clear the same bar. Guarded by Registry.mu.
	restored bool

	// clientStream is this lease's client-bound frame sequencer and replay
	// buffer (see clientstream.go). It is created with the lease and released by
	// Remove, and is the reason a client can tell a truncated stream from a
	// finished one.
	//
	// It is NOT guarded by Registry.mu: it owns its own mutex, and holding
	// Registry.mu across a frame send would put the whole registry behind every
	// forwarded token. The POINTER is set once at lease creation and never
	// reassigned, so reading it under mu and using it outside is race-free.
	clientStream *clientStream

	// reason records why the lease is being (or was) torn down: "" while live,
	// then the teardown cause ("manual release", "idle", reasonCrash, …). It is
	// set under Registry.mu at the moment releasing latches, so it is a
	// non-timing signal that distinguishes an unexpected crash from a graceful
	// release. Remove uses it to pick the client's close status, and a future
	// /leases endpoint (E4-S1) can surface it.
	reason string
}

// clientConn returns the lease's attached client connection, or nil when no
// client is attached (or the lease somehow has no stream, which only a
// hand-built test lease can be).
func (l *lease) clientConn() *wsConn {
	if l.clientStream == nil {
		return nil
	}
	return l.clientStream.attached()
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

	// maxQueueDepth is the ceiling on how many claims may be parked in waiters at
	// once, from `max_queue_depth`. A claim arriving past it is refused
	// immediately with errQueueFull rather than parked, so an over-capacity broker
	// stops accumulating goroutines, timers and held-open connections without
	// bound. A non-positive value means UNLIMITED, matching maxConcurrent.
	//
	// It is ZERO for a registry built by NewRegistry and only takes a value at
	// wiring time (useQueueDepth) — deliberately, so an unwired registry queues
	// exactly as it did before this bound existed. The broker's DefaultConfig
	// supplies the real default. Read under mu.
	maxQueueDepth int

	// maxLeasesPerPrincipal and maxQueuedPerPrincipal are the per-principal
	// admission caps, from `max_leases_per_principal` and
	// `max_queued_per_principal`. Non-positive means that cap is off, which is
	// the default: they are an opt-in multi-tenant policy, not a behaviour every
	// broker acquires.
	//
	// Neither is a counter. Both are answered by walking state the registry
	// already holds — see admitPrincipalLeaseLocked / admitPrincipalQueueLocked —
	// so the single slot counter stays the only accounting path. Read under mu.
	maxLeasesPerPrincipal int
	maxQueuedPerPrincipal int

	// principalCaps gates the two caps above on authentication being CONFIGURED.
	// It is false unless the broker loaded an `auth:` block with at least one
	// validator.
	//
	// It exists because with auth off every lease is owned by anonymousOwner() —
	// a zero Principal with an empty id — so a per-principal cap applied to that
	// single shared identity would count the whole broker against one principal
	// and become a second, lower global cap. Gating on it (together with the
	// empty-id test in principalCapsApply) is what keeps an unauthenticated
	// deployment behaving exactly as it did before these keys existed. Written
	// once at wiring time, read under mu.
	principalCaps bool

	// tickets is the client-WebSocket ticket store whose lease-scoped
	// capabilities must die with the lease (see Remove). It is nil until wired
	// via useTicketStore, and every ticketStore method is nil-receiver safe, so
	// a registry built without one (unit tests, and the tests that predate
	// tickets) needs no branch. It is NOT guarded by mu: it owns its own lock and
	// is only ever assigned at wiring time, before anything is served.
	tickets *ticketStore

	// store persists lease lifecycle records so a broker restart can account for
	// the instances it left running. It is nil until wired via useLeaseStore, and
	// nil is a fully supported state: with no `state_dir` configured the registry
	// simply records nothing and behaves exactly as it did before durability
	// existed. Like tickets it is NOT guarded by mu — the implementation owns its
	// own lock, and this field is only assigned at wiring time, before anything is
	// served.
	store LeaseStore

	// sessionBinaries is the DURABLE session → binary index (see
	// sessionbinary.go). It is separate from store on purpose: the lease journal
	// drops a lease the moment it is released, and a resume happens after that, so
	// the pairing needs a home outside lease-lifecycle compaction.
	//
	// It is nil until wired via useSessionBinaryIndex, and nil is fully supported —
	// with no `state_dir` there is no index and BinaryForSession answers from live
	// leases alone, exactly as it did before this existed. Every method on the type
	// is nil-receiver safe, so no call site branches. Like tickets and store it is
	// NOT guarded by mu: it owns its own lock and is only assigned at wiring time.
	sessionBinaries *sessionBinaryIndex

	// metrics is the process-wide counter set every reporting subsystem reaches
	// through this registry — the gateway (frame drops), the claim server (claim
	// outcomes and spawn failures), restart recovery (restored leases) and the
	// SIGHUP reloader all already hold a *Registry, so hanging the counters here
	// avoids threading a second dependency through four constructors.
	//
	// It is nil until wired via useMetrics, and nil is fully supported: every
	// brokerMetrics method is nil-receiver safe, so a registry built without
	// wiring (every unit test) simply counts nothing. Like tickets and store it is
	// NOT guarded by mu — the counters are atomics and the field is only assigned
	// at wiring time, before anything is served.
	metrics *brokerMetrics

	// clientReplayLimit is the byte bound every new lease's clientStream is
	// created with, from `client_replay_buffer_bytes`. It is read only while
	// minting a lease, so the worst-case retention across the broker is this
	// value times max_concurrent.
	//
	// NewRegistry seeds it with defaultClientReplayBufferBytes so a registry
	// built without wiring (every unit test, and the paths that predate this)
	// buffers exactly as a configured one does; useClientReplayBuffer overrides
	// it at wiring time.
	clientReplayLimit int

	// brokerID and advertiseAddr are stamped on every record so a future shared
	// store can tell whose lease is whose without a data migration. advertiseAddr
	// is the RAW config value, verbatim as the operator wrote it, not the parsed
	// scheme/host pair — a record should round-trip what is in broker.yaml.
	brokerID      string
	advertiseAddr string

	// termGrace is the SIGTERM→SIGKILL window releaseLease uses, seeded from the
	// package constant of the same name. It is a field rather than a direct use of
	// that constant for exactly one reason: a test that wants to observe the
	// escalation should not have to spend two real seconds per case. It is NOT
	// configurable and is never written after construction — set it before the
	// registry is shared with any goroutine.
	termGrace time.Duration

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
		logger:            logger,
		now:               time.Now,
		maxConcurrent:     maxConcurrent,
		termGrace:         termGrace,
		clientReplayLimit: defaultClientReplayBufferBytes,
		waiters:           list.New(),
		leases:            make(map[string]*lease),
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

// useMetrics binds the process-wide counter set (see Registry.metrics). Like the
// other use* setters it is called once at wiring time, before the broker serves.
func (r *Registry) useMetrics(m *brokerMetrics) { r.metrics = m }

// Metrics returns the counter set bound to this registry, or nil. It is
// nil-receiver safe so a caller holding a possibly-nil *Registry (the reloader
// in a unit test) can report without a branch.
func (r *Registry) Metrics() *brokerMetrics {
	if r == nil {
		return nil
	}
	return r.metrics
}

// useClientReplayBuffer sets the per-lease client-bound replay bound in bytes.
// A non-positive limit disables retention: frames are still sequenced, so a
// client can still DETECT loss, but the broker keeps nothing to replay.
//
// Like useTicketStore it is a setter rather than a NewRegistry parameter, so the
// dozens of existing registry tests do not have to name a knob they do not
// exercise. Call it once at wiring time, before the broker serves: it affects
// leases minted after it and is not safe to call concurrently with NewLease.
func (r *Registry) useClientReplayBuffer(limit int) { r.clientReplayLimit = limit }

// useQueueDepth sets the ceiling on parked capacity waiters, from
// `max_queue_depth`. A non-positive limit means unlimited — the pre-E4-S4
// behaviour, and what an unwired registry keeps.
//
// A setter rather than a NewRegistry parameter for the same reason as its
// neighbours: dozens of existing registry tests would otherwise have to name a
// knob none of them exercise, and every one of them must go on proving the
// unbounded behaviour they were written against. Call it once at wiring time,
// before the broker serves.
func (r *Registry) useQueueDepth(limit int) { r.maxQueueDepth = limit }

// usePrincipalLimits binds the two per-principal admission caps and, crucially,
// whether they are in force at all.
//
// authConfigured MUST be "the broker loaded an `auth:` block with at least one
// validator" (Config.AuthChain.Enabled()). Passing true with auth off would make
// both caps apply to the single anonymous identity every unauthenticated lease
// shares, turning either one into a second global cap — see principalCaps.
//
// Non-positive limits leave the corresponding cap off, which is the default for
// both: they are an opt-in multi-tenant policy. Call it once at wiring time,
// before the broker serves.
func (r *Registry) usePrincipalLimits(authConfigured bool, maxLeases, maxQueued int) {
	r.principalCaps = authConfigured
	r.maxLeasesPerPrincipal = maxLeases
	r.maxQueuedPerPrincipal = maxQueued
}

// useLeaseStore binds the durability store every lease transition is journaled
// to, along with the broker identity stamped on each record.
//
// Like useTicketStore it is a setter rather than a NewRegistry parameter: the
// store is optional (an unset `state_dir` disables persistence entirely), and
// threading an optional dependency through the constructor would churn every
// existing registry test for something none of them exercise. Call it once at
// wiring time, before the broker serves — it is not safe to call concurrently
// with a lease transition.
//
// Pass a nil store to leave persistence off. Beware the typed-nil trap: pass a
// literal nil, never a nil *fileLeaseStore, or the nil check in recordLease
// silently stops firing.
func (r *Registry) useLeaseStore(s LeaseStore, brokerID, advertiseAddr string) {
	r.store = s
	r.brokerID = brokerID
	r.advertiseAddr = advertiseAddr
}

// useSessionBinaryIndex binds the durable session → binary index the registry
// records pairings into and reads back in BinaryForSession.
//
// A setter for the same reasons as useLeaseStore: the index is optional (an unset
// `state_dir` disables it), and threading an optional dependency through
// NewRegistry would churn every existing registry test for something none of them
// exercise. Call it once at wiring time, before the broker serves.
//
// Pass a nil *sessionBinaryIndex to leave the index off. There is no typed-nil
// trap here — the field is the concrete pointer type, not an interface, so a nil
// value is nil to every check and every method.
func (r *Registry) useSessionBinaryIndex(idx *sessionBinaryIndex) { r.sessionBinaries = idx }

// RecordSessionBinary durably pairs an engine session id with the `binaries:`
// entry name serving it.
//
// It exists because the pairing becomes knowable at two different moments and the
// index must be written at whichever one comes first:
//
//   - a RESUME names the session id in the claim body, so the pair is complete
//     before anything is spawned — the claim path records it there;
//   - a NEW session has no id until the instance reports one over its dial-back
//     connection, so MarkSessionID records it at that point.
//
// Both funnel here rather than reaching into r.sessionBinaries directly, so the
// index has exactly one owner and one place to change if it ever moves. Empty
// arguments, and a broker with no index, are no-ops.
func (r *Registry) RecordSessionBinary(sessionID, binary string) {
	r.sessionBinaries.record(sessionID, binary)
}

// leaseRecordLocked projects a lease onto its journal record. Caller MUST hold
// r.mu.
//
// It reads the lease's fields and NOT its spawnSecret — the one field on a lease
// that must never reach disk (see lease.spawnSecret). The projection is
// allowlist-shaped rather than a struct copy for exactly that reason: a field
// added to lease later cannot leak into the journal by default.
func (r *Registry) leaseRecordLocked(l *lease, kind leaseRecordKind) LeaseRecord {
	rec := LeaseRecord{
		Kind:          kind,
		LeaseID:       l.id,
		Owner:         ownerRecord(l.owner),
		SessionID:     l.sessionID,
		Binary:        l.binary,
		PID:           l.pid,
		BrokerID:      r.brokerID,
		AdvertiseAddr: r.advertiseAddr,
		CreatedAt:     l.createdAt,
	}
	if kind == leaseRecordReleased {
		at := r.now()
		rec.ReleasedAt = &at
		rec.Reason = l.reason
	}
	return rec
}

// recordLease journals a transition for a known lease. It takes r.mu itself, so
// callers MUST NOT hold it — the file write happens outside the lock, because
// blocking every other lease transition on a disk write would make durability a
// throughput problem.
//
// A missing store, or a lease that has already gone, is a silent no-op.
func (r *Registry) recordLease(id string, kind leaseRecordKind) {
	if r.store == nil {
		return
	}
	r.mu.Lock()
	l, ok := r.leases[id]
	var rec LeaseRecord
	if ok {
		rec = r.leaseRecordLocked(l, kind)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	r.appendRecord(rec)
}

// appendRecord writes one record, LOGGING any failure rather than surfacing it.
//
// This is the whole of the "write failures do not fail the claim or the release"
// rule, expressed as a function with no error return so no caller can
// accidentally propagate one. Losing a record degrades what a restart can
// recover; refusing the claim or the release that produced it would take the
// broker off the air for a disk problem it can otherwise ride out.
func (r *Registry) appendRecord(rec LeaseRecord) {
	if r.store == nil {
		return
	}
	if err := r.store.Append(rec); err != nil {
		r.logger.Error("persisting lease record failed; restart recovery for this lease is degraded",
			"lease_id", rec.LeaseID, "kind", string(rec.Kind), "error", err)
	}
}

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
	// Per-principal lease quota BEFORE the slot: an admission check, so a refused
	// claim never took a slot and has nothing to hand back. It is inert unless
	// authentication is configured and owner is a real principal (see
	// principalCapsApply), so an unauthenticated broker reaches the acquire below
	// exactly as it always did.
	//
	// One check suffices here, unlike in NewLeaseQueued: this whole function
	// holds r.mu from the check through insertLeaseLocked, so no concurrent claim
	// can slip a lease in between the two.
	if err := r.admitPrincipalLeaseLocked(owner); err != nil {
		r.mu.Unlock()
		return "", err
	}
	// Acquire a capacity slot BEFORE the lease exists, so a claim can never
	// spawn an instance past max_concurrent. The slot is bound to the lease
	// (hasSlot) and freed exactly once when the lease is Removed, so every
	// teardown path AND every claim error path frees it simply by removing the
	// lease — no separate slot bookkeeping to leak. At capacity this returns
	// errNoCapacity and no slot is consumed; the claim handler maps it to 503.
	if !r.tryAcquireSlotLocked() {
		r.mu.Unlock()
		return "", errNoCapacity
	}
	if err := r.insertLeaseLocked(id, owner); err != nil {
		r.releaseSlotLocked()
		r.mu.Unlock()
		return "", err
	}
	r.mu.Unlock()
	// Journal the mint OUTSIDE the lock, and before the id is returned: the
	// caller cannot spawn anything for a lease it has not been told about, so
	// there is no window in which a running instance is absent from the journal.
	r.recordLease(id, leaseRecordCreated)
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
// owner IS consulted for the per-principal admission caps (max_leases_per_-
// principal, max_queued_per_principal), which are checked BEFORE any slot is
// taken and are inert unless authentication is configured — see
// principalCapsApply. They do not touch the slot counter.
//
// Returns errNoCapacity if the cap is full and timeout <= 0 (no waiting),
// errQueueFull if the wait queue is at max_queue_depth, errPrincipalLeaseLimit
// or errPrincipalQueueLimit if owner is over one of its caps, errQueueTimeout if
// the wait exceeds timeout, or the wrapped ctx error if the request context is
// cancelled while queued. On any error no slot is held.
func (r *Registry) NewLeaseQueued(ctx context.Context, timeout time.Duration, owner nexusauth.Principal) (string, error) {
	id, err := newLeaseID()
	if err != nil {
		return "", err
	}
	if err := r.acquireSlot(ctx, timeout, owner); err != nil {
		return "", err
	}
	r.mu.Lock()
	// Re-check the per-principal lease quota in the SAME critical section as the
	// insert. acquireSlot's copy of this check is the fast rejection — it refuses
	// an over-quota caller before it parks — but it drops r.mu before the lease
	// exists, so two concurrent claims from one principal could both pass it and
	// both insert. Since the abuse this cap exists to stop is precisely a caller
	// looping /claim in parallel, the check that decides the outcome has to be
	// this one, where the count and the insert cannot be interleaved.
	if err := r.admitPrincipalLeaseLocked(owner); err != nil {
		r.mu.Unlock()
		// We hold a slot we are not going to bind; hand it back to the queue (or
		// decrement) exactly as the insert-failure path below does.
		r.releaseSlot()
		return "", err
	}
	insertErr := r.insertLeaseLocked(id, owner)
	r.mu.Unlock()
	if insertErr != nil {
		// We hold a slot we could not bind to a lease; hand it back to the
		// queue (or decrement) so nothing leaks.
		r.releaseSlot()
		return "", insertErr
	}
	// Journaled here for the same reason as in NewLease: before the id escapes,
	// so nothing can be spawned for a lease the journal has never heard of.
	r.recordLease(id, leaseRecordCreated)
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
		owner:           owner.Clone(),
		lastActivity:    now,
		ready:           make(chan struct{}),
		sessionReported: make(chan struct{}),
		exited:          make(chan struct{}),
		hasSlot:         true,
		clientStream:    newClientStream(r.clientReplayLimit),
	}
	return nil
}

// restoreSpec is one lease being brought back from the journal at boot. Every
// field comes from a persisted LeaseRecord except spawnSecret, which is DERIVED
// (see spawnKey) because no secret is ever written to disk.
type restoreSpec struct {
	id        string
	owner     nexusauth.Principal
	sessionID string
	pid       int
	createdAt time.Time

	// binary is the registry entry name recorded for this lease. It is carried
	// through restore rather than re-resolved from the current config because the
	// record states what was ACTUALLY spawned; re-deriving it would let a config
	// edit made while the broker was down rewrite history. An empty value — a
	// record written by a broker that predates the field — stays empty, which
	// BinaryForSession reads as "not recorded".
	binary string

	// spawnSecret is the value a reattaching instance must present. An empty one
	// would make the lease unattachable rather than open: AttachInstance compares
	// in constant time against it and a register frame carrying no secret is
	// refused outright, so there is no "" == "" hole.
	spawnSecret string
}

// RestoreLease reinserts a lease reconstructed from the journal, re-holding its
// capacity slot. It is the boot-time counterpart to NewLease and is called ONLY
// by recoverLeases, before the broker serves anything.
//
// It does NOT journal a record: the lease's `lease-created` (or superseding
// `lease-updated`) record is already on disk — restoring is the act of believing
// it, not of restating it.
//
// The slot is taken UNCONDITIONALLY rather than through tryAcquireSlotLocked. A
// restored lease is a process that is already running: refusing its slot because
// the operator lowered max_concurrent between restarts would not stop the
// instance, it would only hide it from the count and let the broker admit fresh
// claims on top of it — the exact over-admission the cap exists to prevent. Going
// briefly over the cap is honest; the count drains as leases are released, and
// the queue holds new claims back until it does.
//
// The per-principal caps are skipped for the same reason: a restored lease is a
// process that is already running under an identity that already owned it, and
// refusing it would hide it rather than stop it. A principal over its cap after
// a restart is simply admitted no new leases until it drains back under.
func (r *Registry) RestoreLease(spec restoreSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.leases[spec.id]; exists {
		return fmt.Errorf("lease id collision on restore: %s", spec.id)
	}
	r.slotsInUse++
	if r.maxConcurrent > 0 && r.slotsInUse > r.maxConcurrent {
		r.logger.Warn("restored leases exceed max_concurrent; the broker is over its cap "+
			"until enough leases are released, and will admit no new claims until then",
			"slots_in_use", r.slotsInUse, "max_concurrent", r.maxConcurrent)
	}
	l := &lease{
		id:        spec.id,
		state:     leaseStatePending,
		createdAt: spec.createdAt,
		owner:     spec.owner.Clone(),
		// Last activity is stamped NOW, not from the record: the journal carries no
		// activity time, and dating a restored lease from its creation would have
		// the idle sweeper reap a long-lived healthy session the moment it came
		// back. The reattach reaper, not the idle sweeper, is what bounds a lease
		// nobody reconnects to.
		lastActivity:    r.now(),
		ready:           make(chan struct{}),
		sessionReported: make(chan struct{}),
		exited:          make(chan struct{}),
		hasSlot:         true,
		restored:        true,
		spawnSecret:     spec.spawnSecret,
		sessionID:       spec.sessionID,
		binary:          spec.binary,
		pid:             spec.pid,
		// A restored lease starts a FRESH stream at sequence 1 with an empty
		// buffer. The buffer is in-memory and died with the old process, and
		// nothing on disk records where the sequence had got to — so restarting
		// the count is the only honest option. A client that reconnects across a
		// broker restart sees the sequence go backwards, which is the signal that
		// its stream did not survive.
		clientStream: newClientStream(r.clientReplayLimit),
	}
	r.leases[spec.id] = l
	if spec.sessionID != "" {
		// The session id is already known, so nothing should ever wait on this
		// channel. Closing a channel is non-blocking, so doing it under the lock
		// costs nothing.
		l.sessionOnce.Do(func() { close(l.sessionReported) })
	}
	return nil
}

// unattachedRestored filters ids down to the restored leases that no instance has
// registered against and that are not already being torn down. The reattach
// reaper calls it once its window elapses.
//
// "Never registered" is read off the lifecycle state rather than a second flag:
// a restored lease starts pending, AttachInstance moves it to registered/active,
// and DetachInstance drops it back only as far as registered. Pending after the
// window therefore means nothing ever presented a valid secret for it.
func (r *Registry) unattachedRestored(ids []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, id := range ids {
		l, ok := r.leases[id]
		if !ok || !l.restored || l.releasing {
			continue
		}
		if l.state == leaseStatePending {
			out = append(out, id)
		}
	}
	return out
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

// markTurnLive records that this lease's instance has STARTED working, if it was
// not already known to be working. The gateway calls it from the instance read
// pump for every io.status frame naming a work state (see turnStateIsWork).
//
// It is idempotent within a turn: the stamp is taken on the FIRST work state
// after a settled period and left alone thereafter, so turnStartedAt measures
// the turn rather than the gap since its most recent status frame. It is a no-op
// for an unknown lease.
func (r *Registry) markTurnLive(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok || !l.turnStartedAt.IsZero() {
		return
	}
	l.turnStartedAt = r.now()
}

// markTurnSettled records that this lease's instance has finished working — the
// instance reported the idle state, the only end-of-turn signal the IO envelope
// carries (see ioStateIdle).
//
// It ALSO stamps last-activity, and that is the load-bearing half. Without it a
// turn that ran longer than idle_timeout would be reapable the instant it
// finished: the client's input is by then older than the window, so the answer
// the user is about to read would be torn down before they could reply to it.
// Stamping here makes idle_timeout measure the human pause AFTER the turn, which
// is the thing it was always meant to measure.
//
// It is a no-op for an unknown lease, and harmless for a lease with no turn in
// flight (a second idle status, or an instance that reports idle at boot).
func (r *Registry) markTurnSettled(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return
	}
	l.turnStartedAt = time.Time{}
	l.lastActivity = r.now()
}

// turnLive reports whether the lease currently has a turn in flight. It exists
// for tests and logging; the sweeper uses idleLeases/overrunTurnLeases, which
// make the same judgement under one lock acquisition.
func (r *Registry) turnLive(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	return ok && !l.turnStartedAt.IsZero()
}

// idleLeases returns the ids of every live lease whose last client activity is
// strictly before cutoff, which is not already being torn down, and which has NO
// turn in flight. The idle sweeper computes cutoff as now-idle_timeout and
// releases the returned ids.
//
// The live-turn skip is unbounded on purpose: idleness is "nobody is waiting on
// this", and an instance that is thinking or running a tool is being waited on
// however long ago the client last typed. What stops that skip from being
// forever is max_turn_duration, enforced separately by overrunTurnLeases.
func (r *Registry) idleLeases(cutoff time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id, l := range r.leases {
		if l.releasing {
			continue
		}
		if !l.turnStartedAt.IsZero() {
			continue
		}
		if l.lastActivity.Before(cutoff) {
			ids = append(ids, id)
		}
	}
	return ids
}

// overrunTurnLeases returns the ids of every live lease whose in-flight turn
// started strictly before cutoff and which is not already being torn down. The
// idle sweeper computes cutoff as now-max_turn_duration and releases the
// returned ids with reasonTurnTimeout.
//
// It is the bound on the live-turn skip above: a turn that never reports idle —
// a wedged engine, a tool that never returns, a crashed agent loop that left its
// status at "thinking" — would otherwise hold its lease, and its capacity slot,
// for the lifetime of the broker.
func (r *Registry) overrunTurnLeases(cutoff time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id, l := range r.leases {
		if l.releasing || l.turnStartedAt.IsZero() {
			continue
		}
		if l.turnStartedAt.Before(cutoff) {
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
	var binary string
	if ok {
		l.sessionID = sessionID
		binary = l.binary
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	l.sessionOnce.Do(func() { close(l.sessionReported) })
	// The session id is only knowable once the instance reports it, so the mint
	// record could not carry it. Supersede that record with one that does.
	r.recordLease(id, leaseRecordUpdated)
	// This is the moment a NEW session's id and its binary first exist together,
	// so it is where the pairing becomes durable. It is recorded into the standalone
	// index and not only into the lease journal, because the journal drops this
	// lease when it is released and a resume only ever looks afterwards. The name is
	// read above under the same lock that set the session id, so the pair written
	// here is one that was true simultaneously rather than two separate reads.
	r.RecordSessionBinary(sessionID, binary)
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

// SetBinary stamps the `binaries:` registry entry name a lease's instance is
// spawned from. The claim path calls it immediately after the lease is minted
// and before the runner is invoked, so from that moment on every record the
// lease produces carries which variant it is running — including the
// `lease-updated` the session-id report writes, which is the record that first
// pairs a session id with a binary.
//
// It deliberately journals NOTHING of its own. The name is known at claim time
// but the session id is not, so a record written here could only say "this lease
// has a binary and no session yet", which is the mint record plus one field and
// one extra disk write per claim. The first record to carry the name is
// SetProcess's instead; the only window in which the journal holds a lease with
// no binary recorded is between the mint and the spawn, and a record with no pid
// is one restart recovery closes out without ever consulting its binary.
//
// It is a no-op for an unknown lease.
func (r *Registry) SetBinary(id, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.leases[id]; ok {
		l.binary = name
	}
}

// BinaryForSession answers "which `binaries:` entry is running this engine
// session", and reports whether the broker knows. It is the read side of the
// session → binary mapping: a resume claim consults it before spawning, so a
// persisted session is not handed to a different build than the one that created
// it.
//
// It answers from TWO sources, live leases first and the durable index second:
//
//  1. A live lease running that session. This is the freshest possible answer —
//     it is what the process on this machine is running right now — so it wins
//     even if the index disagrees, which it can only do if a binding was recorded
//     and the entry later repointed.
//  2. The durable session → binary index (sessionbinary.go). This is the case the
//     read exists for: a resume happens after the original lease was released, at
//     which point the lease is gone from memory AND from the compacted lease
//     journal, so a live-leases-only scan would report every real resume unknown.
//
// IT REMAINS BEST-EFFORT BY CONSTRUCTION, and ok=false is an ordinary answer
// rather than an error. The index is capped and prunes its oldest bindings, and a
// broker with no `state_dir` has no index at all — so a session can legitimately
// be unknown here. A caller MUST read ("", false) as "no opinion, proceed" and
// never as a mismatch; in particular a session created before the binding was
// recorded at all must resume exactly as it did before this existed. Adding
// retention to make a found binding a guarantee is explicitly out of scope.
//
// Two guards make an absent mapping report as absent rather than as an empty
// one:
//
//   - An empty session id is always unknown. Without that check the lease scan
//     would match the first pending lease it met — every lease carries an empty
//     session id until its instance reports one — and answer with an unrelated
//     lease's binary.
//   - A lease with no recorded binary is skipped. That is what a lease restored
//     from a journal written before this field existed looks like, and such a
//     resume must fall through to "unknown" rather than be told it asked for the
//     wrong build. (The index cannot hold an empty binary at all; record() drops
//     one and the reader skips one.)
//
// A session id names at most one live lease in practice (an instance holds its
// session directory for as long as it runs), so the scan's first match is the
// only match; it is a linear walk because the registry is bounded by
// max_concurrent and a second in-memory index would be state to keep in step for
// nothing.
func (r *Registry) BinaryForSession(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	r.mu.Lock()
	for _, l := range r.leases {
		if l.sessionID == sessionID && l.binary != "" {
			r.mu.Unlock()
			return l.binary, true
		}
	}
	r.mu.Unlock()
	// Consulted OUTSIDE r.mu: the index has its own lock, and holding the registry
	// lock across a second one is how lock-order problems are invented. Nothing
	// above is carried past the unlock, so there is nothing to be made stale by it.
	return r.sessionBinaries.lookup(sessionID)
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
// and Principal.Clone ensures no internal slice or map escapes — so the returned
// Principal is safe to read and mutate without the lock.
func (r *Registry) LeaseOwner(id string) (nexusauth.Principal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return anonymousOwner(), false
	}
	return l.owner.Clone(), true
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
	// The pid is only knowable once the process exists, so the mint record could
	// not carry it. Supersede that record with one that does — without a pid on
	// disk, restart recovery has no way to ask whether an instance is still alive.
	r.recordLease(id, leaseRecordUpdated)
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

// Sentinel reasons an instance's register frame is refused by the registry. They
// exist so the gateway can tell them apart in its LOG — a missing secret is the
// signature of an instance binary that predates the protocol, a wrong one is the
// signature of something impersonating an instance — while answering both, and
// an unknown lease, with the byte-identical close (see handleInstance).
//
// A third cause, a register frame declaring a schema version this broker does
// not speak, is raised by the gateway before the registry is consulted at all:
// see errRegisterVersionSkew. All three converge on the one place causes are
// told apart, logInstanceRegistrationRefused.
var (
	// errSpawnSecretAbsent means the register frame carried no secret at all
	// while the broker requires one.
	errSpawnSecretAbsent = errors.New("register frame carried no spawn secret")

	// errSpawnSecretMismatch means the register frame carried a secret that is
	// not the one this broker minted for the lease.
	errSpawnSecretMismatch = errors.New("register frame spawn secret does not match")
)

// AttachInstance binds an instance's dial-back connection to a known lease.
// It fails if the lease is unknown, presented a spawn secret that is not the one
// minted for the lease, or already has an instance connection. Those are how the
// gateway rejects an unrecognized register frame.
//
// THE SECRET IS ALWAYS REQUIRED — for a claimed lease as much as a restored one,
// on a broker with an `auth:` block as much as one without. It used to be gated
// on that block, which meant the documented default deployment authenticated the
// dial-back socket with the lease id alone; and a lease id is not a secret. It
// travels in ws_urls, client requests and logs, so anything that observed one
// could register as the instance for that lease the moment its real socket
// dropped, and inherit the client's session. `auth:` configures how CLIENTS are
// verified. It was never a statement about whether the broker should believe an
// unverified dialer, and reading it as one is the hole this closes.
//
// There is no deployment in which requiring it makes a lease unattachable: the
// claim path mints a value and records it BEFORE the runner can exec anything
// (see SetSpawnSecret), and restore derives one from the broker's key (see
// spawnKey and restoreSpec.spawnSecret), so a lease always has an expected value
// to compare against. What the requirement costs is an instance binary older
// than the protocol, which is refused rather than admitted — a deliberate
// breaking change, diagnosed by name in logInstanceRegistrationRefused.
//
// The comparison is constant-time. The presented value never reaches a log or a
// response, so a timing signal would be the only channel available to someone
// guessing it, and closing it costs one function call.
func (r *Registry) AttachInstance(id string, conn *wsConn, presentedSecret string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return fmt.Errorf("unknown lease: %s", id)
	}
	if presentedSecret == "" {
		return errSpawnSecretAbsent
	}
	// An empty expectation is not a wildcard: a lease whose secret never got
	// recorded compares unequal to every non-empty presented value and the
	// absent-secret branch above has already caught the empty one, so there is
	// no "" == "" hole.
	if subtle.ConstantTimeCompare([]byte(presentedSecret), []byte(l.spawnSecret)) != 1 {
		return errSpawnSecretMismatch
	}
	if l.instance != nil {
		return fmt.Errorf("lease %s already has an instance connection", id)
	}
	l.instance = conn
	if l.clientConn() != nil {
		l.state = leaseStateActive
	} else {
		l.state = leaseStateRegistered
	}
	return nil
}

// AttachClient binds a client connection to a known lease. It fails only if the
// lease is unknown.
//
// It attaches with NO replay: the connection sees the live stream from
// whatever the lease sends next, which is what a first connect wants and what
// every connect did before resumption existed. A reconnecting client that
// wants the tail it missed calls AttachClientFrom instead.
func (r *Registry) AttachClient(id string, conn *wsConn) (clientAttach, error) {
	return r.attachClient(id, conn, nil)
}

// AttachClientFrom binds a client connection to a known lease AND stages
// whatever the lease's replay buffer still holds after sequence `after`, plus
// an explicit gap notice when the buffer cannot reach back that far.
//
// The returned clientAttach is for the audit record; the frames themselves are
// already staged on conn by the time it returns, so the caller only has to
// start the write pump.
func (r *Registry) AttachClientFrom(id string, conn *wsConn, after uint64) (clientAttach, error) {
	return r.attachClient(id, conn, &after)
}

// attachClient is the single attach path. A nil `after` attaches with no
// replay; a non-nil one resumes from that sequence.
//
// A LEASE CARRIES ONE CLIENT, AND THE NEWEST ATTACH WINS. It used to be the
// oldest: a second connection was refused and the gateway closed it. That made
// the lease unreachable in exactly the situation the reconnect flow exists for —
// a half-open socket looks attached from the broker's side for as long as
// nothing writes to it, so the genuine client's fresh, correctly authenticated
// reconnect was refused on behalf of a socket that no longer had a peer. Liveness
// probing bounds that window but cannot remove it, because the client always
// notices its own dead connection before the broker's next probe does.
//
// Displacing is safe HERE and only here because the caller has already proved
// ownership: the gateway checks it before the upgrade, so the only principal that
// can evict a lease's client is the one that owns the lease. An eviction
// primitive a stranger could reach would be a denial of service on another
// tenant's session, which is why nothing on this path may be reordered to run
// before that check.
//
// The swap itself is atomic against sending — the stream owns the conn — so no
// frame can be numbered into a window between the outgoing conn leaving and the
// incoming one arriving. The displaced socket is closed by the caller, outside
// the lock.
func (r *Registry) attachClient(id string, conn *wsConn, after *uint64) (clientAttach, error) {
	r.mu.Lock()
	l, ok := r.leases[id]
	if !ok {
		r.mu.Unlock()
		return clientAttach{}, fmt.Errorf("unknown lease: %s", id)
	}
	if l.clientStream == nil {
		// Defensive: every lease is minted with a stream. Attaching without one
		// would leave a client that is never sent anything.
		r.mu.Unlock()
		return clientAttach{}, fmt.Errorf("lease %s has no client stream", id)
	}

	resume, evicted := l.clientStream.attach(conn, after, func(res clientResume) [][]byte {
		if res.gap == nil {
			return res.frames
		}
		// Counted HERE — where the gap is rendered for a specific client —
		// rather than where it is computed, so the metric counts gaps SERVED
		// and not gaps merely calculated. The reason is the wire value the
		// client is about to receive, so the counter and the frame cannot
		// disagree about why the stream broke.
		r.metrics.replayGap(res.gap.Reason)
		// The notice goes FIRST. A client that is told what it lost only after
		// being handed the survivors has already rendered them as a continuous
		// stream, which is the failure this whole path exists to remove.
		data, err := res.gap.encode(id)
		if err != nil {
			// Encoding a gap notice cannot fail with a well-formed payload, but
			// refusing the connection on a broker-side marshalling bug would be
			// worse than the gap: attach live and let the client notice the
			// sequence jump, which is exactly the pre-resume behaviour.
			r.logger.Error("could not encode a stream gap notice; attaching the client "+
				"without one, so the gap is detectable from the sequence only",
				"lease_id", id, "error", err)
			return res.frames
		}
		return append([][]byte{data}, res.frames...)
	})

	if l.instance != nil {
		l.state = leaseStateActive
	}
	owner := l.owner
	r.mu.Unlock()

	if evicted == nil {
		return clientAttach{resume: resume}, nil
	}

	r.metrics.clientEvicted()

	// The displaced socket is closed with a CODED close, not an abort, because
	// the code is the whole point: a client has to be able to tell "you were
	// superseded" from "the instance crashed" and from "the lease was released",
	// and an abort's abnormal closure says none of those things.
	//
	// It runs in its own goroutine because a coded close writes a close frame and
	// then waits up to five seconds for the peer to echo one — and the peer being
	// gone is the single most likely reason this eviction is happening at all.
	// Waiting for it would make the new client's connect hostage to the dead one.
	// Nothing waits on the result: the stream has already stopped referring to
	// this conn, so it is out of the data path from the moment attach returned.
	go evicted.shutdown(evictedCloseStatus, evictedCloseReason)

	// The AUDIT record for an eviction is the gateway's, because the gateway is
	// where the credential that admitted the displacing connection is known. This
	// one is the mechanical half — the socket teardown — and is kept at Debug so
	// an eviction produces exactly one operator-facing record. The owner is
	// stated because by construction it IS the principal that displaced: the
	// gateway refuses anyone else before the upgrade.
	r.logger.Debug("closing the client socket displaced by a newer connection",
		"lease_id", id, "principal_id", owner.ID, "reason", evictedCloseReason,
		"close_status", int(evictedCloseStatus))

	return clientAttach{resume: resume, evicted: true}, nil
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
//
// It detaches conn only if conn is STILL the attached one. A displaced socket
// runs this path too — its read pump ends the moment eviction closes it — and
// must not unattach the connection that superseded it, nor walk the lease back
// out of `active` while a live client is streaming.
func (r *Registry) DetachClient(id string, conn *wsConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok || l.clientStream == nil {
		return
	}
	if l.clientStream.detach(conn) && l.instance != nil {
		l.state = leaseStateRegistered
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
	return l.clientConn()
}

// SendClientFrame is the ONE way a frame reaches a lease's client. It assigns
// the next per-lease sequence, retains the encoded bytes in the lease's replay
// buffer, and queues them on the attached client connection — all under the
// stream's own lock, so the wire order can never disagree with the numbering and
// an attach happening at the same instant either precedes the frame (and
// replays it) or follows it (and receives it live).
//
// It sequences and retains the frame EVEN WHEN NO CLIENT IS ATTACHED, and even
// when an attached client's send queue is full. Those are precisely the two
// paths that used to log and continue, leaving the client believing it had
// heard everything the agent said; retaining the frame is what makes them
// recoverable and the sequence is what makes them visible.
//
// It returns errUnknownLease for a lease that no longer exists — a frame in
// flight when a lease is torn down has nowhere to go and nothing to be replayed
// to, so it is dropped, not buffered.
func (r *Registry) SendClientFrame(id string, f brokerframe.Frame) (uint64, clientFrameOutcome, error) {
	r.mu.Lock()
	l, ok := r.leases[id]
	if !ok {
		r.mu.Unlock()
		return 0, clientFrameDropped, errUnknownLease
	}
	stream := l.clientStream
	r.mu.Unlock()

	if stream == nil {
		// Defensive: every lease is minted with a stream. A nil one would mean an
		// unsequenced client, which is the failure this story exists to remove, so
		// say so rather than silently forwarding.
		return 0, clientFrameDropped, fmt.Errorf("lease %s has no client stream", id)
	}
	seq, outcome, err := stream.send(f)
	if err != nil {
		return 0, clientFrameDropped, fmt.Errorf("encoding client-bound frame for lease %s: %w", id, err)
	}
	return seq, outcome, nil
}

// ClientReplayFrom returns the retained client-bound frames for a lease with a
// sequence greater than after, oldest first, and reports whether the buffer
// still accounts for every frame in that range (see clientStream.replayFrom).
//
// It is the read seam for the resume handshake: nothing in the broker calls it
// yet beyond its tests.
func (r *Registry) ClientReplayFrom(id string, after uint64) ([][]byte, bool, error) {
	r.mu.Lock()
	l, ok := r.leases[id]
	r.mu.Unlock()
	if !ok || l.clientStream == nil {
		return nil, false, errUnknownLease
	}
	frames, complete := l.clientStream.replayFrom(after)
	return frames, complete, nil
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

// SetIOSink installs (or clears, with a nil fn) the in-process observer of this
// lease's instance IO payloads. It is a no-op for an unknown lease.
//
// It is set-then-read rather than a subscriber list because a lease has at most
// one in-process observer: the A2A ingress's binding for that lease, which
// itself fans out to however many tasks are running on it. Keeping the registry
// side single-valued means the registry never has to reason about observer
// lifetimes — the binding does, and the sink dies with the lease.
func (r *Registry) SetIOSink(id string, fn func(brokerIOMessage)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.leases[id]; ok {
		l.ioSink = fn
	}
}

// ioSink returns the lease's in-process IO observer, or nil when there is none.
// The gateway calls it per SignalIO frame and skips decoding entirely when it
// answers nil, so a lease with no A2A task attached pays nothing.
func (r *Registry) ioSink(id string) func(brokerIOMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	if !ok {
		return nil
	}
	return l.ioSink
}

// InstanceAttached reports whether a lease is live, not being torn down, and has
// its instance dial-back connection bound — i.e. whether a payload sent to it
// now has somewhere to go.
//
// It is the "can I still use this?" predicate the A2A ingress asks before
// reusing the instance it started for a context. Has() is not enough: a lease
// mid-release is still in the map, and a lease whose instance socket dropped is
// still in the map too, and sending to either produces a turn that silently
// never runs.
func (r *Registry) InstanceAttached(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[id]
	return ok && !l.releasing && l.instance != nil
}

// LeaseForSession answers "is some live lease already running this engine
// session", reporting the lease id and whether its instance is attached.
//
// It exists to stop the broker booting a SECOND process on one session
// directory. Two engines sharing a session directory do not fail loudly; they
// interleave writes into one history and one set of per-plugin data dirs, and
// the damage surfaces much later as a conversation that lost turns. The window
// where this is reachable is real: after a broker restart the A2A ingress has
// forgotten which lease served a context, restart recovery has restored the
// lease the surviving instance is reattaching to, and the durable context index
// still names the session — so without this check the next A2A message would
// spawn a duplicate.
//
// found=false means no live lease holds the session and it is safe to spawn.
// found=true with attached=false is the restart window: the lease is restored
// and its instance has not dialled back yet. That case is deliberately NOT
// reported as "safe to spawn" — the caller must wait or refuse rather than
// double-boot.
//
// A lease being torn down is skipped: its session is on its way to being free,
// and the caller's retry (or the next message) will find it gone.
//
// It is a linear walk for the same reason BinaryForSession is: the registry is
// bounded by max_concurrent, and a second index would be state to keep in step
// for nothing.
func (r *Registry) LeaseForSession(sessionID string) (id string, attached, found bool) {
	if sessionID == "" {
		return "", false, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for leaseID, l := range r.leases {
		if l.sessionID != sessionID || l.releasing {
			continue
		}
		return leaseID, l.instance != nil, true
	}
	return "", false, false
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
// Ticket invalidation AND the lease-released journal record both live HERE
// rather than in releaseLease, because Remove is the one point every teardown
// converges on. releaseLease ends here, and crash detection (watchExit) calls
// Remove DIRECTLY, bypassing releaseLease entirely — so a hook on releaseLease
// would leave a crashed lease's tickets redeemable for the rest of their TTL and
// its instance recorded on disk as still running, which are exactly the gaps
// both hooks exist to close. One call site, all three teardown reasons.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	l, ok := r.leases[id]
	var (
		released   LeaseRecord
		clientConn *wsConn
	)
	if ok {
		// Projected while the lease still exists and under the same lock that
		// deletes it, so the record cannot observe a half-torn-down lease.
		released = r.leaseRecordLocked(l, leaseRecordReleased)
		l.state = leaseStateClosed
		// Free the lease's capacity slot exactly once. Map deletion below
		// guarantees a second Remove finds nothing, so the slot is never
		// double-freed; clearing hasSlot is belt-and-braces.
		if l.hasSlot {
			l.hasSlot = false
			r.releaseSlotLocked()
		}
		// Free the replay buffer here rather than leaving it to the garbage
		// collector. Remove is the single teardown sink, and a projected record,
		// an A2A binding or a test can outlive the map entry while still holding
		// the lease pointer — so "the map forgot it" is not the same as "the
		// memory is gone". This makes it the same thing.
		if l.clientStream != nil {
			// Take the client conn under the same lock that deletes the lease, so
			// an attach racing this teardown either published before the take (and
			// is closed below) or finds a lease that is already gone. Closing it
			// happens outside the lock, where every other socket teardown does.
			clientConn = l.clientStream.detachAny()
			l.clientStream.release()
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
	if clientConn != nil {
		// The client's close status is derived from the teardown reason so a
		// connected client can tell a crash apart from a normal release. The
		// reason was set under mu before Remove was called; the lock/unlock
		// above is the barrier that publishes it.
		status, reason := clientCloseForReason(l.reason)
		clientConn.shutdown(status, reason)
	}

	// Journal the teardown LAST, outside the lock and after the peers have been
	// closed: bookkeeping must never sit between a teardown and the client
	// learning about it, however slow the disk is.
	//
	// Gated on `ok` — unlike ticket invalidation, which is idempotent — because a
	// second, concurrent Remove finds no lease and must not append a duplicate
	// release for one already recorded as gone.
	r.appendRecord(released)
}

// newLeaseID returns a 128-bit random hex lease id.
func newLeaseID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating lease id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
