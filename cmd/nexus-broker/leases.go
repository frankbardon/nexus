package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"time"
)

// Surface lease states are the operator-facing vocabulary reported by GET
// /leases. They are a stable projection of the registry's internal lifecycle
// enum (pending/registered/active/closed) plus the releasing latch, so the API
// surface does not leak — or get coupled to — the internal state names. The
// mapping lives in lease.surfaceState.
const (
	// surfaceStateSpawning means the lease exists but its instance has not yet
	// dialed back and registered — the claim is still booting an engine.
	surfaceStateSpawning = "spawning"

	// surfaceStateActive means the instance has dialed back (and optionally a
	// client is attached); frames can flow.
	surfaceStateActive = "active"

	// surfaceStateDraining means a teardown (manual release, idle, crash) has
	// latched for this lease and it is being shut down.
	surfaceStateDraining = "draining"
)

// surfaceState projects a lease's internal lifecycle onto the operator-facing
// vocabulary reported by GET /leases. A latched teardown wins over everything
// (the lease is on its way out), then a not-yet-registered lease reads as
// spawning, and anything live reads as active. Caller MUST hold Registry.mu.
func (l *lease) surfaceState() string {
	if l.releasing {
		return surfaceStateDraining
	}
	if l.state == leaseStatePending {
		return surfaceStateSpawning
	}
	return surfaceStateActive
}

// LeaseSnapshot is an immutable, point-in-time view of a single live lease for
// the read-only GET /leases surface. It carries values only — no pointers into
// registry-guarded state — so a caller may read it freely without the lock.
type LeaseSnapshot struct {
	ID        string `json:"lease_id"`
	SessionID string `json:"session_id,omitempty"`
	PID       int    `json:"pid"`
	State     string `json:"state"`

	// Binary is the `binaries:` registry entry NAME this lease's instance was
	// spawned from. It is a name and never a path, so it discloses nothing GET
	// /binaries does not already list, and it is the answer to the one question
	// a multi-variant broker could previously only be asked by grepping the
	// claim log: which build is this lease running.
	//
	// It is emitted ALWAYS, including as the empty string, and the empty string
	// means NOT RECORDED — a lease restored from a journal written before the
	// field existed, or one minted by a path that never stamped one. It never
	// means "the entry named empty string", which cannot exist: config
	// validation refuses an unnamed registry entry. Rendering it with omitempty
	// would collapse "not recorded" into "absent for some reason", which is the
	// distinction lease.binary exists to preserve.
	Binary string `json:"binary"`

	Reason       string    `json:"reason,omitempty"`
	LastActivity time.Time `json:"last_activity"`
	CreatedAt    time.Time `json:"created_at"`
}

// RegistrySnapshot is an immutable, point-in-time view of the registry: the
// leases an audience is entitled to see plus, for an operator audience, the
// capacity/queue aggregates. Encoded as-is it is the OPERATOR JSON shape
// returned by GET /leases (E4-S2 documents it); a caller-scoped listing encodes
// the narrower ownLeasesBody instead, so the aggregate keys are absent rather
// than present-and-zero.
type RegistrySnapshot struct {
	MaxConcurrent int `json:"max_concurrent"`
	SlotsInUse    int `json:"slots_in_use"`
	QueueDepth    int `json:"queue_depth"`

	// MaxQueueDepth is the configured `max_queue_depth` ceiling, reported BESIDE
	// the observed QueueDepth. On its own a depth of 12 says nothing: an operator
	// cannot tell a busy broker from one about to start refusing claims with
	// "capacity queue full" without the bound in the same response. 0 means
	// unlimited, matching MaxConcurrent's reading of the same shape.
	MaxQueueDepth int `json:"max_queue_depth"`

	Leases []LeaseSnapshot `json:"leases"`
}

// ownLeasesBody is the caller-scoped projection of a RegistrySnapshot: the
// caller's own leases and nothing else.
//
// The capacity aggregates are OMITTED rather than zeroed because a zero is a
// claim about the broker ("no cap configured, nothing running") that a client
// which does not know it is unprivileged would believe, whereas an absent key is
// unambiguously "not disclosed to you". They are also never read for such an
// audience in the first place (see SnapshotFor), so there is no real number in
// memory for a later projection bug to leak.
type ownLeasesBody struct {
	Leases []LeaseSnapshot `json:"leases"`
}

// leaseAudience is the disclosure decision for one GET /leases request: which
// leases may be listed, and whether the broker-wide capacity aggregates are
// disclosed at all. It is resolved once per request from the caller's Principal
// and then handed to the registry, so the filtering happens inside the snapshot
// (under the lock) rather than against an already-encoded response.
type leaseAudience struct {
	// operator marks a caller entitled to the WHOLE registry: one holding the
	// configured admin scope, or — when authentication is disabled — every
	// caller, since there is then no identity to scope by and no tenant boundary
	// to protect.
	operator bool

	// ownerID is the principal id a caller-scoped listing is restricted to. It is
	// consulted only when operator is false, and it is compared for exact
	// equality — the same principal-id equality lease ownership uses everywhere
	// else (see ownsLease).
	ownerID string
}

// operatorAudience is the unrestricted audience: every lease, plus aggregates.
func operatorAudience() leaseAudience { return leaseAudience{operator: true} }

// ownerAudience restricts a listing to the leases owned by principal id.
func ownerAudience(id string) leaseAudience { return leaseAudience{ownerID: id} }

// admits reports whether l may be shown to this audience. Caller MUST hold
// Registry.mu — it reads lease.owner, which the lock guards.
func (a leaseAudience) admits(l *lease) bool {
	return a.operator || l.owner.ID == a.ownerID
}

// body picks the JSON value to encode for this audience: the full snapshot for
// an operator (unchanged from before scoping existed), the leases-only
// projection for everyone else.
func (a leaseAudience) body(snap RegistrySnapshot) any {
	if a.operator {
		return snap
	}
	return ownLeasesBody{Leases: snap.Leases}
}

// Snapshot returns the UNRESTRICTED point-in-time view of the registry: every
// live lease plus the aggregates. It is the operator view, and the one internal
// callers (and tests asserting on broker state) want. Caller-facing listings go
// through SnapshotFor with the requester's audience.
func (r *Registry) Snapshot() RegistrySnapshot {
	return r.SnapshotFor(operatorAudience())
}

// SnapshotFor returns a point-in-time, value-only view of the registry limited
// to what audience is entitled to see. It is taken entirely under Registry.mu so
// it never races with concurrent mutations, and it copies every field into plain
// values (no internal pointers/maps escape) so the caller can read it without
// the lock. The lock is held only for the copy; JSON encoding happens after it
// is released. Leases are sorted by creation time (then id) for a stable,
// deterministic surface.
//
// Filtering happens HERE, inside the locked copy, rather than over an assembled
// response: a lease a caller may not see is never copied out of the registry at
// all, so no later projection, log line or encoding step can leak it. The
// aggregates are read in the same single pass for the same reason — suppressing
// them is a matter of not reading them, not of a second traversal.
//
// An audience that owns nothing yields an empty (non-nil) lease slice, which
// encodes as `[]`. That is a successful, empty listing — deliberately not a 404
// and not an error.
func (r *Registry) SnapshotFor(audience leaseAudience) RegistrySnapshot {
	r.mu.Lock()
	snap := RegistrySnapshot{Leases: make([]LeaseSnapshot, 0, len(r.leases))}
	if audience.operator {
		snap.MaxConcurrent = r.maxConcurrent
		snap.SlotsInUse = r.slotsInUse
		snap.QueueDepth = r.waiters.Len()
		snap.MaxQueueDepth = r.maxQueueDepth
	}
	for _, l := range r.leases {
		if !audience.admits(l) {
			continue
		}
		snap.Leases = append(snap.Leases, LeaseSnapshot{
			ID:           l.id,
			SessionID:    l.sessionID,
			PID:          l.pid,
			State:        l.surfaceState(),
			Binary:       l.binary,
			Reason:       l.reason,
			LastActivity: l.lastActivity,
			CreatedAt:    l.createdAt,
		})
	}
	r.mu.Unlock()

	sort.Slice(snap.Leases, func(i, j int) bool {
		if snap.Leases[i].CreatedAt.Equal(snap.Leases[j].CreatedAt) {
			return snap.Leases[i].ID < snap.Leases[j].ID
		}
		return snap.Leases[i].CreatedAt.Before(snap.Leases[j].CreatedAt)
	})
	return snap
}

// LeasesServer handles GET /leases: a read-only introspection surface that lists
// the caller's live leases — or, for an operator, every lease plus the
// capacity/queue aggregates. It performs no mutation; it only reads a scoped
// registry snapshot.
type LeasesServer struct {
	logger   *slog.Logger
	registry *Registry

	// guard is consulted for ONE thing: whether authentication is enabled at all.
	// With it disabled there is no principal to scope by and every lease is owned
	// by the same anonymous identity, so the endpoint must answer exactly as it
	// did before scoping existed — all leases, all aggregates. A nil guard reads
	// as disabled (authGuard.enabled is nil-safe), which is what a test wiring
	// this handler without auth gets.
	guard *authGuard

	// adminScope is the scope a credential must carry to be treated as an
	// operator, from `auth.admin_scope`. Empty means NO caller is an operator
	// while auth is on: the endpoint is then caller-scoped for everyone, which is
	// the safe reading of "the operator did not configure an admin scope".
	adminScope string
}

// NewLeasesServer constructs a leases-listing handler. guard decides whether the
// listing is scoped at all (see LeasesServer.guard) and adminScope names the
// scope that lifts a caller to the operator view.
func NewLeasesServer(logger *slog.Logger, registry *Registry, guard *authGuard, adminScope string) *LeasesServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &LeasesServer{logger: logger, registry: registry, guard: guard, adminScope: adminScope}
}

// Register wires the leases endpoint onto a mux. It takes a routeMux so main can
// register it behind the auth guard.
func (s *LeasesServer) Register(mux routeMux) {
	mux.HandleFunc("GET /leases", s.handleLeases)
}

// handleLeases writes the caller's scoped registry snapshot as JSON. It is a
// pure read: it takes a value snapshot under the registry lock and encodes it,
// never mutating any lease.
//
// A caller that owns no lease gets 200 and an empty list. There is deliberately
// no 404 and no error for that case: "you have nothing running" is a legitimate
// answer, and making it an error would also turn it into an oracle for whether
// anything is running at all.
func (s *LeasesServer) handleLeases(w http.ResponseWriter, r *http.Request) {
	audience := s.audienceFor(r)
	snap := s.registry.SnapshotFor(audience)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(audience.body(snap)); err != nil {
		s.logger.Warn("encoding leases snapshot failed", "error", err)
	}
}

// audienceFor decides what r's caller may see.
//
// The auth-disabled branch comes FIRST and is the backward-compatibility
// guarantee: a broker.yaml with no `auth:` block must answer this endpoint
// byte-for-byte as it did before scoping existed. It cannot be folded into the
// principal path either, because with auth off every lease is owned by the
// anonymous identity — the owner filter would admit them all, but the aggregates
// would be suppressed, silently changing the response shape for every existing
// deployment.
func (s *LeasesServer) audienceFor(r *http.Request) leaseAudience {
	if !s.guard.enabled() {
		return operatorAudience()
	}
	// The admin scope is layered on top of ownership here rather than inside
	// ownsLease: it widens READ visibility only. The mutating lease routes
	// (POST /release, WS /lease/{id}) stay strict principal-id equality, so a
	// leaked operator credential cannot kill or hijack another tenant's session.
	caller := callerPrincipal(r)
	if s.adminScope != "" && caller.HasScope(s.adminScope) {
		return operatorAudience()
	}
	return ownerAudience(caller.ID)
}
