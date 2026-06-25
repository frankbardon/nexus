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
	ID           string    `json:"lease_id"`
	SessionID    string    `json:"session_id,omitempty"`
	PID          int       `json:"pid"`
	State        string    `json:"state"`
	Reason       string    `json:"reason,omitempty"`
	LastActivity time.Time `json:"last_activity"`
	CreatedAt    time.Time `json:"created_at"`
}

// RegistrySnapshot is an immutable, point-in-time view of the whole registry:
// every live lease plus the capacity/queue aggregates. It is the JSON shape
// returned by GET /leases (E4-S2 documents it).
type RegistrySnapshot struct {
	MaxConcurrent int             `json:"max_concurrent"`
	SlotsInUse    int             `json:"slots_in_use"`
	QueueDepth    int             `json:"queue_depth"`
	Leases        []LeaseSnapshot `json:"leases"`
}

// Snapshot returns a point-in-time, value-only view of the registry for the
// read-only GET /leases surface. It is taken entirely under Registry.mu so it
// never races with concurrent mutations, and it copies every field into plain
// values (no internal pointers/maps escape) so the caller can read it without
// the lock. The lock is held only for the copy; JSON encoding happens after it
// is released. Leases are sorted by creation time (then id) for a stable,
// deterministic surface.
func (r *Registry) Snapshot() RegistrySnapshot {
	r.mu.Lock()
	snap := RegistrySnapshot{
		MaxConcurrent: r.maxConcurrent,
		SlotsInUse:    r.slotsInUse,
		QueueDepth:    r.waiters.Len(),
		Leases:        make([]LeaseSnapshot, 0, len(r.leases)),
	}
	for _, l := range r.leases {
		snap.Leases = append(snap.Leases, LeaseSnapshot{
			ID:           l.id,
			SessionID:    l.sessionID,
			PID:          l.pid,
			State:        l.surfaceState(),
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

// LeasesServer handles GET /leases: a read-only introspection surface that
// lists every live lease plus the capacity/queue aggregates. It performs no
// mutation — it only reads a registry Snapshot.
type LeasesServer struct {
	logger   *slog.Logger
	registry *Registry
}

// NewLeasesServer constructs a leases-listing handler.
func NewLeasesServer(logger *slog.Logger, registry *Registry) *LeasesServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &LeasesServer{logger: logger, registry: registry}
}

// Register wires the leases endpoint onto a mux.
func (s *LeasesServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /leases", s.handleLeases)
}

// handleLeases writes the registry snapshot as JSON. It is a pure read: it
// takes a value snapshot under the registry lock and encodes it, never
// mutating any lease.
func (s *LeasesServer) handleLeases(w http.ResponseWriter, _ *http.Request) {
	snap := s.registry.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		s.logger.Warn("encoding leases snapshot failed", "error", err)
	}
}
