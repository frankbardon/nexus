package main

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// newTestTicketStore builds an ENABLED ticket store on a controllable clock, so
// every TTL assertion is made by advancing time rather than sleeping.
func newTestTicketStore(t *testing.T) (*ticketStore, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	s := newTicketStore(testLogger(), true)
	s.now = clk.now
	return s, clk
}

func TestTicketStore_MintAndRedeem(t *testing.T) {
	s, _ := newTestTicketStore(t)

	value, err := s.mint("lease-a", "principal-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if value == "" {
		t.Fatal("mint returned an empty ticket from an enabled store")
	}
	// Opaque and cryptographically random: 256 bits, hex encoded, and carrying no
	// trace of the lease or principal it is bound to (a ticket that embedded either
	// would leak them into every access log it lands in).
	if len(value) != 64 {
		t.Errorf("ticket length = %d, want 64 (256 bits hex)", len(value))
	}
	if _, err := hex.DecodeString(value); err != nil {
		t.Errorf("ticket %q is not hex: %v", value, err)
	}

	got, ok := s.redeem(value, "lease-a")
	if !ok {
		t.Fatal("a freshly minted ticket was not redeemable")
	}
	if got != "principal-a" {
		t.Errorf("redeemed principal = %q, want principal-a", got)
	}
}

// TestTicketStore_MintsDistinctValues guards against the worst possible bug in a
// credential minter: handing the same value to two callers.
func TestTicketStore_MintsDistinctValues(t *testing.T) {
	s, _ := newTestTicketStore(t)

	seen := make(map[string]struct{}, 256)
	for i := 0; i < 256; i++ {
		value, err := s.mint("lease-a", "principal-a")
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		if _, dup := seen[value]; dup {
			t.Fatalf("mint %d returned a duplicate ticket value", i)
		}
		seen[value] = struct{}{}
	}
}

func TestTicketStore_RedeemIsSingleUse(t *testing.T) {
	s, _ := newTestTicketStore(t)

	value, err := s.mint("lease-a", "principal-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := s.redeem(value, "lease-a"); !ok {
		t.Fatal("first redemption failed")
	}
	if _, ok := s.redeem(value, "lease-a"); ok {
		t.Error("second redemption succeeded; the ticket was not consumed")
	}
	if got := s.outstanding(); got != 0 {
		t.Errorf("outstanding = %d, want 0 after redemption", got)
	}
}

// TestTicketStore_ConcurrentRedeemAdmitsExactlyOne is the real single-use
// assertion. Sequential double-redemption only proves the record was deleted;
// this proves the find-and-delete is ATOMIC, which is what stops two racing
// WebSocket handshakes from both being admitted on one ticket.
func TestTicketStore_ConcurrentRedeemAdmitsExactlyOne(t *testing.T) {
	s, _ := newTestTicketStore(t)

	const racers = 64
	for round := 0; round < 32; round++ {
		value, err := s.mint("lease-a", "principal-a")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}

		var wins atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // line everyone up to maximise contention
				if _, ok := s.redeem(value, "lease-a"); ok {
					wins.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()

		if got := wins.Load(); got != 1 {
			t.Fatalf("round %d: %d concurrent redemptions succeeded, want exactly 1", round, got)
		}
	}
}

func TestTicketStore_RedeemAfterTTLFails(t *testing.T) {
	s, clk := newTestTicketStore(t)

	value, err := s.mint("lease-a", "principal-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// One tick short of the TTL: still good, so the failure below is the expiry and
	// not an off-by-a-lot.
	clk.advance(ticketTTL - time.Millisecond)
	if _, ok := s.redeem(value, "lease-a"); !ok {
		t.Fatal("ticket expired BEFORE its TTL elapsed")
	}

	value, err = s.mint("lease-a", "principal-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Exactly at the expiry instant the ticket is already dead: expiry is
	// "now >= expiresAt", so the boundary is closed rather than a one-tick window.
	clk.advance(ticketTTL)
	if _, ok := s.redeem(value, "lease-a"); ok {
		t.Error("ticket was redeemable at its expiry instant")
	}
	// The refused redemption also dropped the record, so a caller cannot keep an
	// expired ticket resident by re-presenting it.
	if got := s.outstanding(); got != 0 {
		t.Errorf("outstanding = %d, want 0 after an expired redemption", got)
	}
}

// TestTicketStore_TicketIsBoundToItsLease proves a ticket for lease A is refused
// for lease B — the property that stops a caller who legitimately holds one lease
// from using its ticket to open another's socket.
func TestTicketStore_TicketIsBoundToItsLease(t *testing.T) {
	s, _ := newTestTicketStore(t)

	forA, err := s.mint("lease-a", "principal-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if _, ok := s.redeem(forA, "lease-b"); ok {
		t.Fatal("a ticket minted for lease-a was accepted for lease-b")
	}
	// The failed binding check is not a USE: the ticket is still good for its own
	// lease. Burning it here would let a stale reconnect destroy a credential the
	// legitimate holder still needs.
	if _, ok := s.redeem(forA, "lease-a"); !ok {
		t.Error("a wrong-lease attempt consumed the ticket; the owner can no longer redeem it")
	}
}

// TestTicketStore_InvalidateLeaseIsScopedToThatLease proves invalidation kills all
// of one lease's tickets and none of another's.
func TestTicketStore_InvalidateLeaseIsScopedToThatLease(t *testing.T) {
	s, _ := newTestTicketStore(t)

	var doomed []string
	for i := 0; i < 3; i++ {
		value, err := s.mint("lease-a", "principal-a")
		if err != nil {
			t.Fatalf("mint a%d: %v", i, err)
		}
		doomed = append(doomed, value)
	}
	survivor, err := s.mint("lease-b", "principal-b")
	if err != nil {
		t.Fatalf("mint b: %v", err)
	}

	s.invalidateLease("lease-a")

	for i, value := range doomed {
		if _, ok := s.redeem(value, "lease-a"); ok {
			t.Errorf("ticket %d survived invalidation of its lease", i)
		}
	}
	if _, ok := s.redeem(survivor, "lease-b"); !ok {
		t.Error("invalidating lease-a also destroyed lease-b's ticket")
	}
	// Idempotent, and harmless for a lease that holds nothing.
	s.invalidateLease("lease-a")
	s.invalidateLease("never-existed")
}

// TestTicketStore_DoesNotGrowWithoutBound pins the bound: minting sweeps, so after
// any mint the store holds only tickets issued within the last ticketTTL — and the
// per-lease index is cleaned up too, since a shrinking value map paired with a
// leaking index is still an unbounded store.
func TestTicketStore_DoesNotGrowWithoutBound(t *testing.T) {
	s, clk := newTestTicketStore(t)

	// 500 tickets across 500 leases, all abandoned rather than redeemed or
	// released — the worst case for growth.
	for i := 0; i < 500; i++ {
		if _, err := s.mint("lease-"+strconv.Itoa(i), "principal-a"); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if got := s.outstanding(); got != 500 {
		t.Fatalf("outstanding = %d, want 500 before any expiry", got)
	}

	// Let the whole window lapse; the next mint must collect all of them.
	clk.advance(ticketTTL + time.Second)
	if _, err := s.mint("lease-fresh", "principal-a"); err != nil {
		t.Fatalf("mint after expiry: %v", err)
	}
	if got := s.outstanding(); got != 1 {
		t.Errorf("outstanding = %d, want 1 (a mint sweeps every expired ticket)", got)
	}
	if got := s.leasesTracked(); got != 1 {
		t.Errorf("leases tracked = %d, want 1 (the per-lease index must be swept too)", got)
	}

	// Redemption and invalidation clean their index entries as well, so the store
	// returns to empty rather than to "empty values, 2 leases".
	value, err := s.mint("lease-redeemed", "principal-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := s.redeem(value, "lease-redeemed"); !ok {
		t.Fatal("redeem: not ok")
	}
	s.invalidateLease("lease-fresh")
	if got := s.outstanding(); got != 0 {
		t.Errorf("outstanding = %d, want 0", got)
	}
	if got := s.leasesTracked(); got != 0 {
		t.Errorf("leases tracked = %d, want 0", got)
	}
}

// TestTicketStore_DisabledIsInert is the backward-compatibility guarantee: with no
// `auth:` block the store issues nothing at all, so the claim response omits
// `ticket` and no code path can start depending on a credential that is not there.
func TestTicketStore_DisabledIsInert(t *testing.T) {
	s := newTicketStore(testLogger(), false)

	if s.issuing() {
		t.Error("a store built with enabled=false reports itself as issuing")
	}
	value, err := s.mint("lease-a", "principal-a")
	if err != nil {
		t.Fatalf("mint on a disabled store returned an error: %v", err)
	}
	if value != "" {
		t.Errorf("mint on a disabled store returned %q, want an empty ticket", value)
	}
	if got := s.outstanding(); got != 0 {
		t.Errorf("outstanding = %d, want 0 on a disabled store", got)
	}
}

// TestTicketStore_NilIsSafe pins the nil-receiver contract every caller relies on
// to avoid a "do we have a ticket store?" branch — Registry.Remove in particular
// calls invalidateLease unconditionally.
func TestTicketStore_NilIsSafe(t *testing.T) {
	var s *ticketStore

	if s.issuing() {
		t.Error("a nil store reports itself as issuing")
	}
	value, err := s.mint("lease-a", "principal-a")
	if err != nil || value != "" {
		t.Errorf("nil mint = (%q, %v), want (\"\", nil)", value, err)
	}
	if _, ok := s.redeem("anything", "lease-a"); ok {
		t.Error("a nil store redeemed a ticket")
	}
	s.invalidateLease("lease-a")
	if got := s.outstanding(); got != 0 {
		t.Errorf("nil outstanding = %d, want 0", got)
	}
	if got := s.leasesTracked(); got != 0 {
		t.Errorf("nil leasesTracked = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Invalidation through every teardown path
// ---------------------------------------------------------------------------

// newTicketedRegistry builds a registry wired to an enabled ticket store, as run()
// wires the real pair. Both run on the same controllable clock so a test can drive
// the idle sweeper's cutoff and a ticket's TTL from one place.
func newTicketedRegistry(t *testing.T) (*Registry, *ticketStore, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	reg := NewRegistry(testLogger(), 0)
	reg.now = clk.now
	s := newTicketStore(testLogger(), true)
	s.now = clk.now
	reg.useTicketStore(s)
	return reg, s, clk
}

// teardownEnv is everything a teardown path needs to tear ONE lease down while
// leaving a bystander lease alive.
type teardownEnv struct {
	reg *Registry
	clk *fakeClock

	// id is the lease to tear down, and proc its (not yet exited) process.
	id   string
	proc *fakeProcess

	// bystander is a second live lease that must survive the teardown untouched,
	// tickets included.
	bystander string
}

// TestTicketInvalidation_EveryTeardownPath is the load-bearing invalidation test:
// a lease's tickets must die with the lease however it died.
//
// All three reasons are asserted through the SAME table because they reach the
// registry by different routes and only one of them is obvious: manual release and
// the idle sweeper funnel through releaseLease, but crash detection (watchExit)
// calls Remove DIRECTLY, bypassing releaseLease entirely. A hook on releaseLease
// would pass the first two rows and leave a crashed lease's tickets redeemable —
// which is why the hook lives in Remove.
func TestTicketInvalidation_EveryTeardownPath(t *testing.T) {
	cases := []struct {
		name string
		// teardown tears env.id down the way this path does, and must leave that
		// lease removed — and env.bystander alive — when it returns.
		teardown func(t *testing.T, env teardownEnv)
	}{
		{
			name: "manual release",
			teardown: func(t *testing.T, env teardownEnv) {
				t.Helper()
				close(env.proc.exited) // the instance obeys the shutdown frame
				if err := env.reg.releaseLease(env.id, "manual release", 2*time.Second); err != nil {
					t.Fatalf("releaseLease: %v", err)
				}
			},
		},
		{
			name: "idle sweep",
			teardown: func(t *testing.T, env teardownEnv) {
				t.Helper()
				close(env.proc.exited)
				// Age BOTH leases past the idle window, then give the bystander fresh
				// client activity so the sweeper's own selection picks exactly one. The
				// sweeper is driven rather than releaseLease called directly, so the
				// row genuinely exercises the idle path end to end.
				//
				// The advance stays well inside ticketTTL on purpose: the registry and
				// the ticket store share this clock, so a longer jump would expire the
				// bystander's ticket and the survival assertion would pass for the wrong
				// reason (or fail for one).
				env.clk.advance(10 * time.Second)
				env.reg.markActivity(env.bystander)
				sweeper := newIdleSweeper(testLogger(), env.reg, 5*time.Second, 0, 2*time.Second)
				sweeper.sweep()
				// The sweeper releases in a goroutine per lease.
				waitFor(t, func() bool { return !env.reg.Has(env.id) })
			},
		},
		{
			name: "crash",
			teardown: func(t *testing.T, env teardownEnv) {
				t.Helper()
				// The instance dies with NOBODY having asked it to: watchExit
				// classifies it as a crash and removes the lease itself, WITHOUT
				// going through releaseLease.
				close(env.proc.exited)
				env.reg.watchExit(env.id)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, tickets, clk := newTicketedRegistry(t)
			proc := newFakeProcess(900)
			id, _, _ := seedLiveLease(t, reg, proc)

			// Two tickets on the doomed lease, one on a bystander lease that must
			// survive — an invalidation that swept everything would otherwise pass.
			var doomed []string
			for i := 0; i < 2; i++ {
				value, err := tickets.mint(id, "principal-a")
				if err != nil {
					t.Fatalf("mint: %v", err)
				}
				doomed = append(doomed, value)
			}
			otherID, _, _ := seedLiveLease(t, reg, newFakeProcess(901))
			survivor, err := tickets.mint(otherID, "principal-b")
			if err != nil {
				t.Fatalf("mint bystander: %v", err)
			}

			tc.teardown(t, teardownEnv{reg: reg, clk: clk, id: id, proc: proc, bystander: otherID})
			if reg.Has(id) {
				t.Fatalf("precondition: %s did not remove the lease", tc.name)
			}
			if !reg.Has(otherID) {
				t.Fatalf("precondition: %s also removed the bystander lease", tc.name)
			}

			for i, value := range doomed {
				if _, ok := tickets.redeem(value, id); ok {
					t.Errorf("ticket %d is still redeemable after %s", i, tc.name)
				}
			}
			if _, ok := tickets.redeem(survivor, otherID); !ok {
				t.Errorf("%s invalidated an unrelated lease's ticket", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// POST /ticket/{lease_id}
// ---------------------------------------------------------------------------

// newTicketTestServer wires POST /ticket/{lease_id} BEHIND the auth guard over an
// httptest server, using the two-principal chain, and returns the server plus the
// registry and ticket store.
//
// Going through the guard is what makes these tests meaningful: the handler reads
// its caller from the request context, so a route on a raw mux would see every
// caller as anonymous and could never exercise a cross-principal refusal.
func newTicketTestServer(t *testing.T) (*httptest.Server, *Registry, *ticketStore) {
	t.Helper()
	logger := testLogger()
	reg, tickets, _ := newTicketedRegistry(t)
	mux := http.NewServeMux()
	NewTicketServer(logger, reg, tickets).
		Register(newAuthGuard(logger, mustAuthChain(t, twoPrincipalAuthYAML)).Guard(mux))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg, tickets
}

// ownerPrincipalIdentity is the Principal the twoPrincipalAuthYAML owner token
// resolves to, for seeding a lease that principal owns.
func ownerPrincipalIdentity() nexusauth.Principal {
	return nexusauth.Principal{ID: ownerPrincipal}
}

func TestTicketRoute_OwnerGetsAFreshTicket(t *testing.T) {
	ts, reg, tickets := newTicketTestServer(t)
	id, _ := seedLeaseOwned(t, reg, newFakeProcess(910), ownerPrincipalIdentity())

	resp := doAuthed(t, http.MethodPost, ts.URL+"/ticket/"+id, ownerToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body ticketResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.LeaseID != id {
		t.Errorf("lease_id = %q, want %q", body.LeaseID, id)
	}
	if body.Ticket == "" {
		t.Fatal("no ticket issued to the lease owner")
	}

	// The issued value is one the store actually recorded, bound to this lease and
	// to the owner — not a random string the handler invented.
	got, ok := tickets.redeem(body.Ticket, id)
	if !ok {
		t.Fatal("the issued ticket is not redeemable; the route returned a value the store never recorded")
	}
	if got != ownerPrincipal {
		t.Errorf("ticket principal = %q, want %q", got, ownerPrincipal)
	}
}

// TestTicketRoute_RefusalsAreIndistinguishable is the security property: a valid
// caller asking for a ticket on someone else's lease gets exactly what it gets for
// a lease that never existed, so the route is not a lease-id oracle.
func TestTicketRoute_RefusalsAreIndistinguishable(t *testing.T) {
	ts, reg, tickets := newTicketTestServer(t)
	id, _ := seedLeaseOwned(t, reg, newFakeProcess(911), ownerPrincipalIdentity())

	unowned := doAuthed(t, http.MethodPost, ts.URL+"/ticket/"+id, otherToken, "")
	unknown := doAuthed(t, http.MethodPost, ts.URL+"/ticket/no-such-lease", otherToken, "")
	assertIdenticalRefusals(t, http.StatusNotFound, unknown, unowned)

	// A refused caller was issued nothing at all — the count is the assertion the
	// status code cannot make.
	if got := tickets.outstanding(); got != 0 {
		t.Errorf("outstanding tickets after refusals = %d, want 0", got)
	}
	// ...and the lease is untouched: a refusal must not disturb the owner's session.
	if !reg.Has(id) {
		t.Error("a refused ticket request tore down the lease")
	}
}

// TestTicketRoute_OwnerRefusedAfterRelease proves the refresh route stops issuing
// the moment the lease is gone. Without this, a client whose lease was reaped by
// the idle sweeper could keep minting credentials for it.
func TestTicketRoute_OwnerRefusedAfterRelease(t *testing.T) {
	ts, reg, _ := newTicketTestServer(t)
	proc := newFakeProcess(912)
	id, _ := seedLeaseOwned(t, reg, proc, ownerPrincipalIdentity())
	close(proc.exited)
	if err := reg.releaseLease(id, "manual release", 2*time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}

	resp := doAuthed(t, http.MethodPost, ts.URL+"/ticket/"+id, ownerToken, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a released lease", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if got["error"] != unknownLeaseError {
		t.Errorf("error = %q, want %q", got["error"], unknownLeaseError)
	}
}

// TestTicketRoute_AuthDisabledIsInert is the backward-compatibility half: with no
// `auth:` block the route answers 200 and issues nothing, rather than erroring.
// An anonymous caller "owns" every anonymously-claimed lease, so the ownership
// check admits it exactly as the release route does.
func TestTicketRoute_AuthDisabledIsInert(t *testing.T) {
	logger := testLogger()
	reg := NewRegistry(logger, 0)
	cfg := mustLoadConfig(t, "")
	if cfg.AuthChain.Enabled() {
		t.Fatal("precondition: auth should be disabled for this test")
	}
	guard := newAuthGuard(logger, cfg.AuthChain)
	tickets := newTicketStore(logger, guard.enabled())
	reg.useTicketStore(tickets)

	mux := http.NewServeMux()
	NewTicketServer(logger, reg, tickets).Register(guard.Guard(mux))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	id, _ := seedLease(t, reg, newFakeProcess(913))

	resp := doAuthed(t, http.MethodPost, ts.URL+"/ticket/"+id, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with auth disabled", resp.StatusCode)
	}
	// The `ticket` key must be ABSENT, not present-and-empty: a client has to be
	// able to tell "this broker issues no tickets" from "your ticket is the empty
	// string". Decoding into ticketResponse would collapse the two.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if _, present := fields["ticket"]; present {
		t.Errorf("body carries a ticket with auth disabled: %s", body)
	}
	if got := tickets.outstanding(); got != 0 {
		t.Errorf("outstanding = %d, want 0 with auth disabled", got)
	}
}
