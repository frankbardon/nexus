package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// newReleaseTestServer wires the release endpoint over an httptest server,
// returning the server and the shared registry so a test can pre-seed leases.
func newReleaseTestServer(t *testing.T, grace time.Duration) (*httptest.Server, *Registry) {
	t.Helper()
	reg := NewRegistry(testLogger(), 0)
	rs := NewReleaseServer(testLogger(), reg, grace)
	mux := http.NewServeMux()
	rs.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg
}

// newOwnedReleaseTestServer wires the release endpoint BEHIND the auth guard over
// an httptest server, using the two-principal chain, and returns the server plus
// the shared registry.
//
// Going through the guard is what makes these tests meaningful: the handler reads
// its caller from the request context, so a release route registered on a raw mux
// would see every caller as anonymous and could never exercise a cross-principal
// refusal.
func newOwnedReleaseTestServer(t *testing.T, grace time.Duration) (*httptest.Server, *Registry) {
	t.Helper()
	logger := testLogger()
	reg := NewRegistry(logger, 0)
	rs := NewReleaseServer(logger, reg, grace)
	mux := http.NewServeMux()
	rs.Register(newAuthGuard(logger, mustAuthChain(t, twoPrincipalAuthYAML)).Guard(mux))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg
}

// seedLease mints an anonymously-owned lease and attaches a nil-conn instance +
// the given process, mirroring what claim does after a successful spawn. It
// returns the lease id and the attached instance connection so the test can
// inspect queued frames.
func seedLease(t *testing.T, reg *Registry, proc processHandle) (string, *wsConn) {
	t.Helper()
	return seedLeaseOwned(t, reg, proc, anonymousOwner())
}

// seedLeaseOwned is seedLease for a lease claimed by a specific principal.
func seedLeaseOwned(t *testing.T, reg *Registry, proc processHandle, owner nexusauth.Principal) (string, *wsConn) {
	t.Helper()
	id, err := reg.NewLease(owner)
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	wc := newWSConn(nil)
	attachTestInstance(t, reg, id, wc)
	reg.SetProcess(id, proc)
	return id, wc
}

func TestReleaseLease_GracefulSendsShutdownAndRemoves(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	proc := newFakeProcess(100)
	id, wc := seedLease(t, reg, proc)

	// The instance exits cleanly on its own (as a real engine would after the
	// shutdown frame), so the grace path never force-kills.
	proc.exit()

	if err := reg.releaseLease(id, "test", 2*time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}

	// A shutdown frame was queued to the instance.
	select {
	case data := <-wc.send:
		f, err := brokerframe.Decode(data)
		if err != nil {
			t.Fatalf("decode queued frame: %v", err)
		}
		if f.Signal != brokerframe.SignalShutdown {
			t.Fatalf("queued frame signal = %q, want shutdown", f.Signal)
		}
		if f.LeaseID != id {
			t.Errorf("queued frame lease = %q, want %q", f.LeaseID, id)
		}
	default:
		t.Fatal("no shutdown frame was queued to the instance")
	}

	// The process was NOT force-killed (it exited gracefully).
	select {
	case <-proc.killed:
		t.Fatal("process was force-killed despite a graceful exit")
	default:
	}

	// The lease/slot is freed.
	if reg.Has(id) {
		t.Error("lease still present after release")
	}
}

func TestReleaseLease_ForceKillsOnGraceTimeout(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	// Compressed so the SIGTERM→SIGKILL window costs the suite milliseconds
	// rather than the production two seconds.
	reg.termGrace = 40 * time.Millisecond
	proc := newFakeProcess(101)
	proc.exitOnTerm = false // wedged: it exits on nothing short of SIGKILL
	id, _ := seedLease(t, reg, proc)

	start := time.Now()
	if err := reg.releaseLease(id, "test", 60*time.Millisecond); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	elapsed := time.Since(start)

	// The grace period must have elapsed before the kill.
	if elapsed < 60*time.Millisecond {
		t.Errorf("release returned in %v, want >= grace (60ms)", elapsed)
	}

	// The process was force-killed (no orphan).
	select {
	case <-proc.killed:
	case <-time.After(time.Second):
		t.Fatal("stuck instance was not force-killed")
	}

	if reg.Has(id) {
		t.Error("lease still present after forced release")
	}
}

// seedWedgedLease mints a lease with a process but NO instance connection: the
// dial-back socket is gone (the instance is wedged, or mid reconnect-backoff)
// while the process itself is still very much alive.
//
// That is the case the shutdown frame cannot address, and the whole reason
// teardown may not depend on it: releaseLease has nothing to send the frame on,
// so anything graceful has to come from a signal.
func seedWedgedLease(t *testing.T, reg *Registry, proc processHandle) string {
	t.Helper()
	id, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	reg.SetProcess(id, proc)
	return id
}

// TestReleaseLease_WedgedInstanceGetsSIGTERMBeforeSIGKILL is the regression test
// for a release that used to be a straight SIGKILL.
//
// With no socket the instance never sees the shutdown frame, so before this the
// grace period was spent waiting for something that had never been asked and the
// engine's first and only notice of teardown was SIGKILL — no flush, no session
// persisted. SIGTERM is what the engine handles as a clean shutdown, so an
// instance that takes it must never be killed at all.
func TestReleaseLease_WedgedInstanceGetsSIGTERMBeforeSIGKILL(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	reg.termGrace = 2 * time.Second // never expected to elapse here
	proc := newFakeProcess(102)     // a real engine shuts down cleanly on SIGTERM
	id := seedWedgedLease(t, reg, proc)

	start := time.Now()
	if err := reg.releaseLease(id, "test", 40*time.Millisecond); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	elapsed := time.Since(start)

	select {
	case <-proc.termed:
	default:
		t.Fatal("a wedged instance was never sent SIGTERM; with no socket it received no teardown request at all")
	}
	select {
	case <-proc.killed:
		t.Error("the instance was SIGKILLed even though it exited on SIGTERM")
	default:
	}
	if got := proc.signalOrder(); len(got) != 1 || got[0] != "terminate" {
		t.Errorf("signals = %v, want exactly [terminate]", got)
	}
	// The escalation must not start before release_grace has actually elapsed —
	// SIGTERM is the fallback for a missing socket, not a replacement for the
	// operator's shutdown budget.
	if elapsed < 40*time.Millisecond {
		t.Errorf("release returned in %v, want >= grace (40ms)", elapsed)
	}
	if reg.Has(id) {
		t.Error("lease still present after release")
	}
}

// TestReleaseLease_EscalatesTermThenKill pins the ORDER, which is the property
// that matters: an instance that ignores SIGTERM is still killed, but only after
// it has had its chance to flush.
func TestReleaseLease_EscalatesTermThenKill(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	reg.termGrace = 40 * time.Millisecond
	proc := newFakeProcess(103)
	proc.exitOnTerm = false // ignores SIGTERM and never exits
	id := seedWedgedLease(t, reg, proc)

	start := time.Now()
	if err := reg.releaseLease(id, "test", 40*time.Millisecond); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	elapsed := time.Since(start)

	got := proc.signalOrder()
	if len(got) != 2 || got[0] != "terminate" || got[1] != "kill" {
		t.Fatalf("signals = %v, want [terminate kill]", got)
	}
	// grace + termGrace, both of which must actually have been waited out.
	if elapsed < 80*time.Millisecond {
		t.Errorf("release returned in %v, want >= grace + term_grace (80ms)", elapsed)
	}
	if reg.Has(id) {
		t.Error("lease still present after forced release")
	}
}

// TestReleaseLease_TermGraceDefaultsToTheSharedConstant guards against the two
// teardown paths drifting apart again: the spawned path's second window and the
// adopted path's escalation are one value.
func TestReleaseLease_TermGraceDefaultsToTheSharedConstant(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	if reg.termGrace != termGrace {
		t.Errorf("registry termGrace = %v, want the shared constant %v", reg.termGrace, termGrace)
	}
}

func TestReleaseLease_UnknownLeaseErrors(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	err := reg.releaseLease("does-not-exist", "test", time.Second)
	if !errors.Is(err, errUnknownLease) {
		t.Fatalf("releaseLease(unknown) = %v, want errUnknownLease", err)
	}
}

func TestReleaseLease_IdempotentDoubleRelease(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	proc := newFakeProcess(102)
	id, _ := seedLease(t, reg, proc)
	proc.exit()

	if err := reg.releaseLease(id, "test", time.Second); err != nil {
		t.Fatalf("first releaseLease: %v", err)
	}
	// Second release of the now-gone lease is a clean no-op (unknown), not a
	// panic.
	if err := reg.releaseLease(id, "test", time.Second); !errors.Is(err, errUnknownLease) {
		t.Fatalf("second releaseLease = %v, want errUnknownLease", err)
	}
}

func TestReleaseHTTP_KnownLeaseReturns200(t *testing.T) {
	ts, reg := newReleaseTestServer(t, 2*time.Second)
	proc := newFakeProcess(200)
	id, _ := seedLease(t, reg, proc)
	proc.exit()

	resp, err := http.Post(ts.URL+"/release/"+id, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "released" || body["lease_id"] != id {
		t.Errorf("unexpected body: %+v", body)
	}
	if reg.Has(id) {
		t.Error("lease not removed after HTTP release")
	}
}

func TestReleaseHTTP_UnknownLeaseReturns404(t *testing.T) {
	ts, _ := newReleaseTestServer(t, time.Second)

	resp, err := http.Post(ts.URL+"/release/nope", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestReleaseHTTP_NonOwnerIsRefusedAndInstanceSurvives is the load-bearing
// ownership test: a valid credential belonging to a DIFFERENT principal gets 404,
// and — the part a status-code-only test would miss — the owner's instance is
// untouched. A check placed after releaseLease would answer 404 having already
// queued the shutdown frame and killed the session.
func TestReleaseHTTP_NonOwnerIsRefusedAndInstanceSurvives(t *testing.T) {
	ts, reg := newOwnedReleaseTestServer(t, 2*time.Second)
	proc := newFakeProcess(210)
	id, wc := seedLeaseOwned(t, reg, proc, nexusauth.Principal{ID: ownerPrincipal})

	resp := doAuthed(t, http.MethodPost, ts.URL+"/release/"+id, otherToken, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-owner release status = %d, want 404", resp.StatusCode)
	}

	// The lease is still live and still holds its slot.
	if !reg.Has(id) {
		t.Fatal("a non-owner's release tore the lease down")
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Errorf("slots in use = %d, want 1 (a refused release must free nothing)", got)
	}
	// No shutdown frame reached the instance...
	select {
	case data := <-wc.send:
		t.Fatalf("a refused release queued a frame to the instance: %q", data)
	default:
	}
	// ...and the process was neither asked to stop nor killed.
	select {
	case <-proc.killed:
		t.Fatal("a refused release force-killed the owner's instance")
	default:
	}

	// The rightful owner still gets the FULL shared teardown: shutdown frame,
	// bounded grace, slot freed. Without this the 404 above could just mean
	// "release is broken".
	proc.exit() // the instance exits cleanly on the shutdown frame
	owned := doAuthed(t, http.MethodPost, ts.URL+"/release/"+id, ownerToken, "")
	if owned.StatusCode != http.StatusOK {
		t.Fatalf("owner release status = %d, want 200", owned.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(owned.Body).Decode(&body); err != nil {
		t.Fatalf("decode owner release body: %v", err)
	}
	if body["status"] != "released" || body["lease_id"] != id {
		t.Errorf("owner release body = %+v, want a released envelope for %s", body, id)
	}
	select {
	case data := <-wc.send:
		f, err := brokerframe.Decode(data)
		if err != nil {
			t.Fatalf("decode queued frame: %v", err)
		}
		if f.Signal != brokerframe.SignalShutdown {
			t.Errorf("queued frame signal = %q, want shutdown", f.Signal)
		}
	default:
		t.Error("the owner's release queued no shutdown frame")
	}
	if reg.Has(id) {
		t.Error("lease still present after the owner released it")
	}
	if got := reg.SlotsInUse(); got != 0 {
		t.Errorf("slots in use after owner release = %d, want 0", got)
	}
}

// TestReleaseHTTP_OwnerReleaseKeepsForceKillBackstop proves the ownership check
// short-circuits nothing downstream of it: an authenticated owner releasing a
// STUBBORN instance still gets the whole shared teardown, force-kill backstop
// included, and the slot is still freed.
func TestReleaseHTTP_OwnerReleaseKeepsForceKillBackstop(t *testing.T) {
	const grace = 60 * time.Millisecond
	ts, reg := newOwnedReleaseTestServer(t, grace)
	reg.termGrace = 40 * time.Millisecond
	proc := newFakeProcess(215)
	proc.exitOnTerm = false // wedged: neither the frame nor SIGTERM moves it
	id, _ := seedLeaseOwned(t, reg, proc, nexusauth.Principal{ID: ownerPrincipal})

	start := time.Now()
	resp := doAuthed(t, http.MethodPost, ts.URL+"/release/"+id, ownerToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner release status = %d, want 200", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Errorf("release returned in %v, want >= grace (%v): the bounded wait was skipped", elapsed, grace)
	}
	select {
	case <-proc.killed:
	case <-time.After(time.Second):
		t.Fatal("a stuck instance was not force-killed on an owner release")
	}
	if reg.Has(id) {
		t.Error("lease still present after a forced owner release")
	}
	if got := reg.SlotsInUse(); got != 0 {
		t.Errorf("slots in use after forced owner release = %d, want 0", got)
	}
}

// TestReleaseHTTP_UnknownAndUnownedRefusalsAreIdentical pins the oracle property
// on the release route: to a caller that owns neither, a lease that does not exist
// and a lease owned by someone else must produce byte-identical responses.
func TestReleaseHTTP_UnknownAndUnownedRefusalsAreIdentical(t *testing.T) {
	ts, reg := newOwnedReleaseTestServer(t, time.Second)
	id, _ := seedLeaseOwned(t, reg, newFakeProcess(211), nexusauth.Principal{ID: ownerPrincipal})

	unknown := doAuthed(t, http.MethodPost, ts.URL+"/release/no-such-lease", otherToken, "")
	unowned := doAuthed(t, http.MethodPost, ts.URL+"/release/"+id, otherToken, "")
	assertIdenticalRefusals(t, http.StatusNotFound, unknown, unowned)
}

// TestReleaseHTTP_OwnerDoubleReleaseIsIdempotent404 proves the idempotent 404 is
// unchanged by ownership: the owner's second release of its own, already-gone
// lease still gets 404 — the ownership check must not turn that into a different
// answer.
func TestReleaseHTTP_OwnerDoubleReleaseIsIdempotent404(t *testing.T) {
	ts, reg := newOwnedReleaseTestServer(t, time.Second)
	proc := newFakeProcess(212)
	id, _ := seedLeaseOwned(t, reg, proc, nexusauth.Principal{ID: ownerPrincipal})
	proc.exit()

	first := doAuthed(t, http.MethodPost, ts.URL+"/release/"+id, ownerToken, "")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first release status = %d, want 200", first.StatusCode)
	}
	second := doAuthed(t, http.MethodPost, ts.URL+"/release/"+id, ownerToken, "")
	if second.StatusCode != http.StatusNotFound {
		t.Fatalf("second release status = %d, want 404 (idempotent)", second.StatusCode)
	}
	assertErrorBody(t, second, unknownLeaseError)
}

// TestReleaseHTTP_AuthDisabledReleasesAnonymousLease is the
// backward-compatibility guarantee stated explicitly: with no `auth:` block the
// lease owner and the caller are both the anonymous identity, so an id-equality
// check admits the caller and release behaves exactly as it did before ownership
// existed.
//
// The second half is defence in depth. A named owner cannot arise while auth is
// off (the chain is fixed at boot), but if one ever did, the check must still
// refuse rather than treat "auth disabled" as "everything is releasable".
func TestReleaseHTTP_AuthDisabledReleasesAnonymousLease(t *testing.T) {
	ts, reg := newReleaseTestServer(t, time.Second) // registered on a RAW mux: no guard
	proc := newFakeProcess(213)
	id, _ := seedLease(t, reg, proc)
	proc.exit()

	resp, err := http.Post(ts.URL+"/release/"+id, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous release of an anonymous lease = %d, want 200", resp.StatusCode)
	}
	if reg.Has(id) {
		t.Error("lease not removed after an anonymous release")
	}

	namedProc := newFakeProcess(214)
	namedID, _ := seedLeaseOwned(t, reg, namedProc, nexusauth.Principal{ID: ownerPrincipal})
	refused, err := http.Post(ts.URL+"/release/"+namedID, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release (named owner): %v", err)
	}
	refused.Body.Close()
	if refused.StatusCode != http.StatusNotFound {
		t.Errorf("anonymous release of a named-owner lease = %d, want 404", refused.StatusCode)
	}
	if !reg.Has(namedID) {
		t.Error("an anonymous caller tore down a named principal's lease")
	}
}

func TestReleaseHTTP_DoubleReleaseIsClean(t *testing.T) {
	ts, reg := newReleaseTestServer(t, time.Second)
	proc := newFakeProcess(201)
	id, _ := seedLease(t, reg, proc)
	proc.exit()

	first, err := http.Post(ts.URL+"/release/"+id, "application/json", nil)
	if err != nil {
		t.Fatalf("first POST /release: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.StatusCode)
	}

	second, err := http.Post(ts.URL+"/release/"+id, "application/json", nil)
	if err != nil {
		t.Fatalf("second POST /release: %v", err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Fatalf("second status = %d, want 404 (idempotent)", second.StatusCode)
	}
}
