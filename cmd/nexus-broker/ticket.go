package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ticketTTL is how long a client WebSocket ticket stays redeemable.
//
// It is a CONSTANT, deliberately not a config key. A ticket travels in a URL
// query parameter (browsers cannot put a header on a WebSocket handshake), which
// means it lands in proxy access logs, browser history and referrer chains no
// matter what the broker does. The short window plus single use are the whole
// mitigation for that exposure, so letting an operator widen it would let them
// silently remove the only thing making the design safe. 30s is long enough to
// cover a claim → dial round trip on a slow link and short enough that a leaked
// URL is stale before anyone reads the log it landed in.
const ticketTTL = 30 * time.Second

// ticketRecord is one issued, not-yet-redeemed ticket.
//
// It stores the principal ID rather than the whole Principal because ID is the
// only field authorization ever compares (see nexusauth.Principal and
// ownsLease), and keeping a full Principal here would invite a future check to
// consult a tenant or scope snapshot frozen at mint time.
type ticketRecord struct {
	leaseID     string
	principalID string
	expiresAt   time.Time
}

// ticketStore issues and redeems single-use, lease-scoped, short-lived tickets
// for the client WebSocket route.
//
// Why tickets exist at all: browser JavaScript cannot set headers on a WebSocket
// handshake, so an upstream `Authorization: Bearer` credential can never reach
// `WS /lease/{lease_id}` from a browser. A ticket is the one credential the
// broker itself mints. It is NOT a general-purpose token: it is bound to a single
// lease AND a single principal, expires in ticketTTL, and is consumed by its
// first successful redemption.
//
// It is safe for concurrent mint/redeem/sweep. Every method is nil-receiver safe
// so a caller wired without a store (unit tests, and any future path that has no
// business issuing tickets) needs no branch.
type ticketStore struct {
	logger *slog.Logger

	// enabled is false when the broker runs with no `auth:` block. A disabled
	// store issues NOTHING: mint returns an empty value and no error, so
	// POST /claim omits the `ticket` field and WS /lease/{lease_id} keeps working
	// with no ticket exactly as it did before tickets existed.
	//
	// The flag lives HERE, in one place, rather than at each mint site: "auth is
	// off, so ticket issuance is inert" is a single decision, and spreading it
	// across callers is how one of them ends up minting a credential nothing
	// checks.
	enabled bool

	// now is the clock used for expiry stamping and expiry checks. It defaults to
	// time.Now; tests swap it for a deterministic clock so the TTL is exercised by
	// advancing time rather than sleeping. Unexported and set only at wiring time,
	// mirroring Registry.now.
	now func() time.Time

	mu sync.Mutex

	// byValue maps a ticket value to its record. It is the redemption index.
	byValue map[string]ticketRecord

	// byLease maps a lease id to the set of ticket values issued for it, so
	// invalidating a released lease's tickets is a direct lookup rather than a
	// scan of every outstanding ticket. Both maps are mutated together and are
	// only ever consistent under mu.
	byLease map[string]map[string]struct{}
}

// newTicketStore builds a ticket store. Pass enabled=false when the broker has no
// `auth:` block, which makes issuance inert (see ticketStore.enabled).
func newTicketStore(logger *slog.Logger, enabled bool) *ticketStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &ticketStore{
		logger:  logger,
		enabled: enabled,
		now:     time.Now,
		byValue: make(map[string]ticketRecord),
		byLease: make(map[string]map[string]struct{}),
	}
}

// issuing reports whether this store hands out tickets at all. A nil store never
// does, which is what makes every other method's nil check meaningful.
func (s *ticketStore) issuing() bool { return s != nil && s.enabled }

// mint issues a fresh ticket for leaseID bound to principalID and returns its
// value. A disabled (or nil) store returns an empty value and NO error: not
// issuing is a supported state, not a failure, and callers distinguish the two by
// the error alone.
//
// The caller is responsible for having established that principalID may act on
// leaseID — the store records the binding, it does not authorize it.
func (s *ticketStore) mint(leaseID, principalID string) (string, error) {
	if !s.issuing() {
		return "", nil
	}
	value, err := newTicketValue()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	// Sweep on mint. Minting is the ONLY way the store grows, so sweeping here
	// bounds it exactly: after any mint the store holds only tickets issued within
	// the last ticketTTL. That is a real bound with no background goroutine to own,
	// start or stop, and the sweep is O(outstanding tickets) over a set that is
	// small by construction (one live window's worth).
	s.sweepLocked()
	s.byValue[value] = ticketRecord{
		leaseID:     leaseID,
		principalID: principalID,
		expiresAt:   s.now().Add(ticketTTL),
	}
	if s.byLease[leaseID] == nil {
		s.byLease[leaseID] = make(map[string]struct{}, 1)
	}
	s.byLease[leaseID][value] = struct{}{}
	outstanding := len(s.byValue)
	s.mu.Unlock()

	// The ticket VALUE is never logged, here or anywhere. It is a bearer
	// credential that already ends up in URLs (and therefore access logs); the
	// broker's own logs must not add to that exposure. lease_id and principal_id
	// are what an operator needs to correlate an issuance with a connection.
	s.logger.Debug("issued lease ticket",
		"lease_id", leaseID, "principal_id", principalID,
		"ttl", ticketTTL, "outstanding", outstanding)
	return value, nil
}

// redeem consumes the ticket named by value, provided it is unexpired and was
// issued for leaseID. It returns the principal ID the ticket was bound to.
//
// Redemption is ATOMIC: the record is removed under the same lock that found it,
// so two concurrent redemptions of one ticket cannot both succeed — exactly one
// sees ok=true. That is the whole of "single use".
//
// A ticket presented for the WRONG lease is refused WITHOUT being consumed. The
// lease binding is an authorization check, not a use, and burning a ticket on a
// failed check would let a mismatched request (a stale reconnect to a lease that
// has been replaced, say) destroy a credential the legitimate holder still needs.
func (s *ticketStore) redeem(value, leaseID string) (string, bool) {
	if s == nil || value == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byValue[value]
	if !ok {
		return "", false
	}
	if !s.now().Before(rec.expiresAt) {
		// Expired: drop it now rather than leaving it for the next sweep, so a
		// caller cannot keep an expired record alive by re-presenting it.
		s.forgetLocked(value, rec.leaseID)
		return "", false
	}
	if rec.leaseID != leaseID {
		return "", false
	}
	s.forgetLocked(value, rec.leaseID)
	return rec.principalID, true
}

// invalidateLease destroys every ticket issued for a lease. It is called from
// Registry.Remove, the single point every teardown converges on, so a lease's
// capabilities cannot outlive the lease however it died — manual release, idle
// sweep or crash.
//
// It is idempotent and a no-op for a lease with no outstanding tickets, which is
// what lets Remove call it unconditionally.
func (s *ticketStore) invalidateLease(leaseID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	values := s.byLease[leaseID]
	n := len(values)
	for value := range values {
		delete(s.byValue, value)
	}
	delete(s.byLease, leaseID)
	s.mu.Unlock()

	if n > 0 {
		s.logger.Debug("invalidated lease tickets", "lease_id", leaseID, "count", n)
	}
}

// forgetLocked removes one ticket from both indexes. Caller MUST hold s.mu.
func (s *ticketStore) forgetLocked(value, leaseID string) {
	delete(s.byValue, value)
	if values := s.byLease[leaseID]; values != nil {
		delete(values, value)
		if len(values) == 0 {
			// Drop the empty set too, or byLease grows one entry per lease that
			// ever held a ticket and never shrinks.
			delete(s.byLease, leaseID)
		}
	}
}

// sweepLocked drops every expired ticket. Caller MUST hold s.mu.
func (s *ticketStore) sweepLocked() {
	now := s.now()
	for value, rec := range s.byValue {
		if !now.Before(rec.expiresAt) {
			s.forgetLocked(value, rec.leaseID)
		}
	}
}

// outstanding reports how many tickets the store currently holds, including any
// expired ones not yet swept. It backs the bounded-growth assertions.
func (s *ticketStore) outstanding() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byValue)
}

// leasesTracked reports how many leases have at least one outstanding ticket. It
// exists so a test can prove the per-lease index is cleaned up too — byValue
// shrinking while byLease leaks empty sets would still be an unbounded store.
func (s *ticketStore) leasesTracked() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byLease)
}

// newTicketValue returns a 256-bit cryptographically random ticket value, hex
// encoded.
//
// Hex rather than base64 because the value is carried as a URL query parameter
// and hex needs no escaping under any encoder; 256 bits rather than the lease
// id's 128 because a ticket is a credential presented by an untrusted client,
// and the cost of the extra 32 characters is nil.
func newTicketValue() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating ticket: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ticketResponse is the JSON body of POST /ticket/{lease_id}.
//
// Ticket is omitted (rather than empty) when the store is inert — the same
// omitempty convention claimResponse.SessionID uses — so a client can tell "no
// ticket was issued because this broker does not authenticate" from "a ticket was
// issued and it is the empty string", which would be a bug.
type ticketResponse struct {
	LeaseID string `json:"lease_id"`
	Ticket  string `json:"ticket,omitempty"`
}

// TicketServer handles POST /ticket/{lease_id}: it mints a REPLACEMENT client
// WebSocket ticket for a caller that owns the lease.
//
// The refresh route exists because ticketTTL is deliberately tight. A claim's
// ticket covers the first connect; a reconnect after a dropped socket, a laptop
// resume, or any pause longer than 30s needs a fresh one, and the alternative to
// this route would be re-claiming — which spawns a new instance and abandons the
// live session.
type TicketServer struct {
	logger   *slog.Logger
	registry *Registry
	tickets  *ticketStore
}

// NewTicketServer constructs the ticket refresh handler. A nil tickets store
// makes the route inert (200 with no `ticket` field), matching a broker with
// authentication disabled.
func NewTicketServer(logger *slog.Logger, registry *Registry, tickets *ticketStore) *TicketServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &TicketServer{logger: logger, registry: registry, tickets: tickets}
}

// Register wires the ticket endpoint onto a mux. It takes a routeMux so main can
// register it behind the auth guard — which is what authenticates it: a route
// registered through the guard is authenticated by construction rather than by
// someone remembering to wrap its handler.
func (s *TicketServer) Register(mux routeMux) {
	mux.HandleFunc("POST /ticket/{lease_id}", s.handleTicket)
}

// handleTicket mints a fresh ticket for the lease named in the path, provided the
// authenticated caller owns it.
//
// The ownership rule is the SAME predicate release uses (ownsLease, principal-ID
// equality), and an unknown lease answers identically to an unowned one. Both
// matter: a mint route that leaked "this lease exists" would be a lease-id
// enumeration oracle, and one that minted for a non-owner would hand out a
// capability to hijack another tenant's live session — a strictly worse outcome
// than the release route's, since it survives past the request.
func (s *TicketServer) handleTicket(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("lease_id")
	if leaseID == "" {
		s.fail(w, http.StatusBadRequest, "ticket requires a lease id")
		return
	}

	caller := callerPrincipal(r)
	if !ownsLease(s.registry, leaseID, caller) {
		logLeaseDenied(s.logger, r, caller, leaseID)
		writeUnknownLease(w)
		return
	}

	value, err := s.tickets.mint(leaseID, caller.ID)
	if err != nil {
		// Nothing was issued and nothing was consumed, so a retry is safe.
		s.logger.Error("minting lease ticket failed", "lease_id", leaseID,
			"principal_id", caller.ID, "error", err)
		s.fail(w, http.StatusInternalServerError, "minting ticket")
		return
	}

	// The value is deliberately absent from this record — see ticketStore.mint.
	s.logger.Info("issued lease ticket", "route", routeLabel(r),
		"lease_id", leaseID, "principal_id", caller.ID, "issued", value != "")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ticketResponse{LeaseID: leaseID, Ticket: value})
}

// fail writes a JSON error response, in the broker's usual envelope.
func (s *TicketServer) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
