//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// TestClaimSpawnProxyRoundTrip proves the full E1-S4 spine deterministically:
// POST /claim spawns a stub instance, the instance dials back + registers +
// signals ready, the claim returns {lease_id, ws_url}, and a client connecting
// to ws_url exchanges an IO frame with the instance through the gateway. It
// uses a stub instance (testdata/stubinstance) instead of the real nexus
// binary, so it needs no LLM API key and makes no network calls.
func TestClaimSpawnProxyRoundTrip(t *testing.T) {
	stubBin := buildStubInstance(t)
	base := startStubBroker(t, stubBin)

	// Claim a new session; this blocks until the stub signals ready.
	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	// A new session reports the engine-generated id back to the caller.
	if cr.SessionID != "stub-new-session" {
		t.Fatalf("new-session claim session_id = %q, want %q", cr.SessionID, "stub-new-session")
	}

	// Connect a client to the broker's per-lease endpoint and round-trip a
	// frame through the spawned instance (which echoes it back).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, cr.WSURL, nil)
	if err != nil {
		t.Fatalf("dial ws_url %s: %v", cr.WSURL, err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	out, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: cr.LeaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"hello":"world"}`),
	})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := client.Write(ctx, websocket.MessageText, out); err != nil {
		t.Fatalf("client write: %v", err)
	}

	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	echo, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echo.Signal != brokerframe.SignalIO || string(echo.Payload) != `{"hello":"world"}` {
		t.Fatalf("unexpected echo frame: %+v", echo)
	}
}

// TestClaimResumePassesRecall proves the resume path deterministically: a claim
// carrying session_id spawns the stub with -recall <id>. The stub reports the
// recalled id back as its session id, so the claim response echoing that id is
// proof the broker passed -recall (no LLM, no real engine).
func TestClaimResumePassesRecall(t *testing.T) {
	stubBin := buildStubInstance(t)
	base := startStubBroker(t, stubBin)

	const priorSession = "prior-session-xyz"
	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n","session_id":"`+priorSession+`"}`)
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	// The stub reports back exactly the id it was told to -recall; an echo of
	// the requested id proves the broker spawned it with -recall <id>.
	if cr.SessionID != priorSession {
		t.Fatalf("resume claim session_id = %q, want %q (proves -recall was passed)", cr.SessionID, priorSession)
	}
}

// TestReleaseGracefulShutdown proves POST /release tears a live instance down
// cleanly: the broker sends a shutdown frame, the stub exits on it (graceful
// path), and the lease is freed so a client can no longer connect. Uses the
// stub instance, so no LLM and no API key.
func TestReleaseGracefulShutdown(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin)

	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if cr.LeaseID == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	if !reg.Has(cr.LeaseID) {
		t.Fatal("lease missing after claim")
	}

	resp, err := http.Post("http://"+base+"/release/"+cr.LeaseID, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release status = %d, want 200", resp.StatusCode)
	}

	// The lease/slot is freed.
	if reg.Has(cr.LeaseID) {
		t.Error("lease still present after release")
	}
	// A client can no longer connect to the released lease.
	dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dcancel()
	if c, _, err := websocket.Dial(dctx, cr.WSURL, nil); err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Error("client unexpectedly connected to a released lease")
	}

	// Releasing an already-gone lease is a clean 404, not a panic.
	again, err := http.Post("http://"+base+"/release/"+cr.LeaseID, "application/json", nil)
	if err != nil {
		t.Fatalf("second POST /release: %v", err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusNotFound {
		t.Errorf("second release status = %d, want 404", again.StatusCode)
	}
}

// TestReleaseForceKillsStubbornInstance proves the bounded-grace force-kill
// backstop: with STUB_IGNORE_SHUTDOWN=1 the stub ignores the shutdown frame, so
// the broker must force-kill it after the (short) release grace. The release
// still succeeds and the lease is freed.
func TestReleaseForceKillsStubbornInstance(t *testing.T) {
	t.Setenv("STUB_IGNORE_SHUTDOWN", "1")
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin, withReleaseGrace(150*time.Millisecond))

	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if cr.LeaseID == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}

	start := time.Now()
	resp, err := http.Post("http://"+base+"/release/"+cr.LeaseID, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release status = %d, want 200", resp.StatusCode)
	}
	// The grace must have elapsed before the force-kill (proving the fallback
	// ran rather than a graceful exit).
	if elapsed < 150*time.Millisecond {
		t.Errorf("release returned in %v, want >= grace (150ms) — graceful path, not force-kill", elapsed)
	}
	if reg.Has(cr.LeaseID) {
		t.Error("lease still present after forced release")
	}
}

// TestCrashDetectionFreesSlotAndClosesClient proves the E2-S4 crash path end to
// end: a live instance dies UNEXPECTEDLY (not via POST /release), and the broker
// frees its slot, removes the lease, and closes that client's WS with the
// distinguishable crash status — while a SECOND concurrent lease is untouched.
// It uses the stub instance in STUB_CRASH_AFTER_READY mode (it exits abnormally
// on the first IO frame), so it is deterministic with no LLM and no API key.
func TestCrashDetectionFreesSlotAndClosesClient(t *testing.T) {
	// Both spawned stubs inherit this, but only the one that receives an IO
	// frame crashes — so the sibling lease stays alive.
	t.Setenv("STUB_CRASH_AFTER_READY", "1")
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin)

	crashCR := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	keepCR := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if crashCR.LeaseID == "" || keepCR.LeaseID == "" {
		t.Fatalf("incomplete claim responses: %+v %+v", crashCR, keepCR)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Connect a client to each lease.
	crashClient, _, err := websocket.Dial(ctx, crashCR.WSURL, nil)
	if err != nil {
		t.Fatalf("dial crash lease: %v", err)
	}
	defer crashClient.Close(websocket.StatusNormalClosure, "")
	keepClient, _, err := websocket.Dial(ctx, keepCR.WSURL, nil)
	if err != nil {
		t.Fatalf("dial keep lease: %v", err)
	}
	defer keepClient.Close(websocket.StatusNormalClosure, "")

	// Capture the crashing lease pointer so we can read its terminal reason
	// after it is removed from the registry map (the object outlives the entry).
	reg.mu.Lock()
	crashLease := reg.leases[crashCR.LeaseID]
	reg.mu.Unlock()
	if crashLease == nil {
		t.Fatal("crash lease missing from registry after claim")
	}

	// Poke the crashing instance with an IO frame; the stub exits abnormally.
	trigger, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: crashCR.LeaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"crash":"now"}`),
	})
	if err != nil {
		t.Fatalf("encode trigger frame: %v", err)
	}
	if err := crashClient.Write(ctx, websocket.MessageText, trigger); err != nil {
		t.Fatalf("write trigger frame: %v", err)
	}

	// The crashing client's WS is closed with the distinguishable crash status.
	if _, _, rerr := crashClient.Read(ctx); rerr == nil {
		t.Fatal("expected the crashed lease's client WS to close")
	} else if cs := websocket.CloseStatus(rerr); cs != crashCloseStatus {
		t.Errorf("crash close status = %v, want %v (err=%v)", cs, crashCloseStatus, rerr)
	}

	// Slot freed: the lease is gone from the registry (no orphaned entry).
	waitFor(t, func() bool { return !reg.Has(crashCR.LeaseID) })

	// Lease reason reflects a crash, not a graceful release.
	reg.mu.Lock()
	gotReason := crashLease.reason
	reg.mu.Unlock()
	if gotReason != reasonCrash {
		t.Errorf("crashed lease reason = %q, want %q", gotReason, reasonCrash)
	}

	// The OTHER lease is untouched: still present and its client still open.
	if !reg.Has(keepCR.LeaseID) {
		t.Error("the sibling lease was removed by the crash")
	}
	// A short read on the sibling client should TIME OUT (still connected),
	// not return a close error. CloseStatus returns -1 for a non-close error.
	rctx, rcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, _, rerr := keepClient.Read(rctx)
	rcancel()
	if rerr == nil {
		t.Error("unexpected frame on the sibling lease's client")
	} else if cs := websocket.CloseStatus(rerr); cs != -1 {
		t.Errorf("sibling lease client was closed (status %v); want still-open", cs)
	}
}

// TestIdleTimeoutReleasesInstance proves the E2-S3 idle path end to end: a
// claimed instance with a connected-but-silent client (no io frames) is released
// once it sits idle past a short idle_timeout. The instance process is gone, the
// lease is removed, and the client's WS is closed with the graceful going-away
// status — distinct from the crash status — proving the idle teardown went
// through the shared release path and not crash detection. Instance → client
// lifecycle frames (register/ready/session-id) do NOT reset the timer, which is
// exactly why a never-typing client still gets reaped. Deterministic, no LLM.
func TestIdleTimeoutReleasesInstance(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin,
		withIdleTimeout(300*time.Millisecond),
		withReleaseGrace(2*time.Second))

	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if cr.LeaseID == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}

	// Connect a client but send NO io frames: the lease must go idle despite an
	// open client connection and any instance-side output.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, cr.WSURL, nil)
	if err != nil {
		t.Fatalf("dial ws_url %s: %v", cr.WSURL, err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	// The idle sweeper releases the lease; reading drains any instance output and
	// then surfaces the close. The close status must be going-away (idle), never
	// the crash status.
	for {
		if _, _, rerr := client.Read(ctx); rerr != nil {
			cs := websocket.CloseStatus(rerr)
			if cs == crashCloseStatus {
				t.Fatalf("idle release closed client with crash status %v (err=%v)", cs, rerr)
			}
			if cs != websocket.StatusGoingAway {
				t.Fatalf("idle close status = %v, want going-away (err=%v)", cs, rerr)
			}
			break
		}
	}

	// The instance process is gone and the lease/slot is freed.
	waitFor(t, func() bool { return !reg.Has(cr.LeaseID) })
}

// TestMaxConcurrentCapRejectsOverCapClaim proves the E3-S1 capacity cap end to
// end with cap=1: a first claim goes live and holds the only slot, a second
// claim arriving at capacity is rejected with a distinct 503 (no instance
// spawned past the cap), and once the first lease is released a third claim
// succeeds — proving the slot was freed and re-acquirable. Deterministic, no
// LLM and no API key (stub instance).
func TestMaxConcurrentCapRejectsOverCapClaim(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin, withMaxConcurrent(1))

	// First claim takes the only slot and goes live.
	first := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if first.LeaseID == "" {
		t.Fatalf("incomplete first claim: %+v", first)
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use after first claim = %d, want 1", got)
	}

	// Second claim arrives at capacity: it must be rejected with 503 and NOT
	// consume a slot or spawn an instance.
	resp := postClaimRaw(t, base, `{"config":"engine:\n  name: stub\n"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap claim status = %d, want 503", resp.StatusCode)
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use after rejected claim = %d, want 1 (no drift)", got)
	}

	// Release the first lease, freeing its slot.
	rel, err := http.Post("http://"+base+"/release/"+first.LeaseID, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	rel.Body.Close()
	if rel.StatusCode != http.StatusOK {
		t.Fatalf("release status = %d, want 200", rel.StatusCode)
	}
	waitFor(t, func() bool { return reg.SlotsInUse() == 0 })

	// A third claim now succeeds against the freed slot.
	third := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if third.LeaseID == "" {
		t.Fatalf("incomplete third claim after release: %+v", third)
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use after third claim = %d, want 1", got)
	}
}

// TestQueuedClaimProceedsWhenSlotFrees proves the E3-S2 FIFO wait queue end to
// end with cap=1: a first claim goes live and holds the only slot, a second
// claim arriving at capacity PARKS in the queue (it does not 503), and once the
// first lease is released the queued claim is granted the slot, spawns, becomes
// ready, and can converse through the gateway. Deterministic, no LLM (stub).
func TestQueuedClaimProceedsWhenSlotFrees(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin,
		withMaxConcurrent(1),
		withQueueWaitTimeout(10*time.Second))

	// Claim A takes the only slot and goes live.
	first := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if first.LeaseID == "" {
		t.Fatalf("incomplete first claim: %+v", first)
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use after first claim = %d, want 1", got)
	}

	// Claim B arrives at capacity: it must PARK in the queue, not get 503'd.
	bCh := make(chan claimResponse, 1)
	go func() { bCh <- postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`) }()
	waitFor(t, func() bool { return reg.QueueLen() == 1 })
	select {
	case <-bCh:
		t.Fatal("queued claim B returned before a slot freed")
	case <-time.After(200 * time.Millisecond):
	}

	// Release A, freeing its slot → B is granted it and proceeds.
	rel, err := http.Post("http://"+base+"/release/"+first.LeaseID, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	rel.Body.Close()
	if rel.StatusCode != http.StatusOK {
		t.Fatalf("release status = %d, want 200", rel.StatusCode)
	}

	var second claimResponse
	select {
	case second = <-bCh:
	case <-time.After(20 * time.Second):
		t.Fatal("queued claim B never proceeded after the slot freed")
	}
	if second.LeaseID == "" || second.LeaseID == first.LeaseID {
		t.Fatalf("queued claim B lease = %q (first = %q)", second.LeaseID, first.LeaseID)
	}

	// B is live: a client can converse through it (echo round-trip).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, second.WSURL, nil)
	if err != nil {
		t.Fatalf("dial B ws_url %s: %v", second.WSURL, err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	out, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: second.LeaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"hello":"queued"}`),
	})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := client.Write(ctx, websocket.MessageText, out); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	echo, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echo.Signal != brokerframe.SignalIO || string(echo.Payload) != `{"hello":"queued"}` {
		t.Fatalf("unexpected echo frame: %+v", echo)
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use after B proceeds = %d, want 1 (no drift)", got)
	}
}

// TestQueuedClaimTimesOut proves a queued claim that never gets a slot returns a
// distinct 503 "capacity wait timed out" after queue_wait_timeout, leaves the
// holder untouched, and leaks no waiter. Deterministic, no LLM (stub).
func TestQueuedClaimTimesOut(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin,
		withMaxConcurrent(1),
		withQueueWaitTimeout(200*time.Millisecond))

	first := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if first.LeaseID == "" {
		t.Fatalf("incomplete first claim: %+v", first)
	}

	// Second claim queues and then times out because no slot ever frees.
	start := time.Now()
	resp := postClaimRaw(t, base, `{"config":"engine:\n  name: stub\n"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("queue-timeout status = %d, want 503", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "capacity wait timed out" {
		t.Fatalf("error = %q, want %q (distinct timeout message)", body["error"], "capacity wait timed out")
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("queued claim returned in %v, want >= queue_wait_timeout (200ms)", elapsed)
	}

	// No drift, no leaked waiter, the holder is untouched.
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use after queue timeout = %d, want 1 (no drift)", got)
	}
	waitFor(t, func() bool { return reg.QueueLen() == 0 })
	if !reg.Has(first.LeaseID) {
		t.Error("first lease vanished during the queued claim's timeout")
	}
}

// TestLeasesListsLiveLeaseThenGoneAfterRelease proves the E4-S1 introspection
// surface end to end: after a claim, GET /leases lists the live lease with its
// session id and an active state; after POST /release the lease no longer
// appears and the slot is freed. Uses the stub instance, so no LLM and no API
// key.
func TestLeasesListsLiveLeaseThenGoneAfterRelease(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin, withMaxConcurrent(3))

	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	if cr.LeaseID == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}

	// GET /leases lists the live lease with the right id, session id, an active
	// state, and the capacity aggregates.
	snap := getLeasesIntegration(t, base)
	if snap.MaxConcurrent != 3 {
		t.Errorf("max_concurrent = %d, want 3", snap.MaxConcurrent)
	}
	if snap.SlotsInUse != 1 {
		t.Errorf("slots_in_use = %d, want 1", snap.SlotsInUse)
	}
	if snap.QueueDepth != 0 {
		t.Errorf("queue_depth = %d, want 0", snap.QueueDepth)
	}
	if len(snap.Leases) != 1 {
		t.Fatalf("leases len = %d, want 1", len(snap.Leases))
	}
	got := snap.Leases[0]
	if got.ID != cr.LeaseID {
		t.Errorf("lease_id = %q, want %q", got.ID, cr.LeaseID)
	}
	if got.SessionID != cr.SessionID || got.SessionID == "" {
		t.Errorf("session_id = %q, want %q", got.SessionID, cr.SessionID)
	}
	if got.State != surfaceStateActive {
		t.Errorf("state = %q, want %q", got.State, surfaceStateActive)
	}
	if got.PID == 0 {
		t.Error("pid not populated")
	}

	// Release the lease; it must drop off the surface and free its slot.
	resp, err := http.Post("http://"+base+"/release/"+cr.LeaseID, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release status = %d, want 200", resp.StatusCode)
	}
	waitFor(t, func() bool { return !reg.Has(cr.LeaseID) })

	after := getLeasesIntegration(t, base)
	if len(after.Leases) != 0 {
		t.Fatalf("leases len after release = %d, want 0", len(after.Leases))
	}
	if after.SlotsInUse != 0 {
		t.Errorf("slots_in_use after release = %d, want 0", after.SlotsInUse)
	}
}

// TestLeasesListingIsScopedPerPrincipal is the end-to-end half of E2-S3, over a
// REAL claimed lease with a live spawned instance rather than a seeded registry
// entry: the claimant sees its own lease without the capacity aggregates, a second
// valid principal sees an empty list, and the operator credential sees the lease
// plus the aggregates.
//
// It exercises the DEFAULT admin scope (the fixture sets no `admin_scope:`), so it
// covers the wiring a stock broker.yaml gets — the unit tests deliberately use a
// custom scope to prove the key is honoured.
func TestLeasesListingIsScopedPerPrincipal(t *testing.T) {
	stubBin := buildStubInstance(t)
	b := startAuthedStubBroker(t, stubBin, withMaxConcurrent(3))

	cr := b.Claim(t, b.Token, stubClaimBody)
	if cr.LeaseID == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}

	// --- the claimant: its own lease, no aggregates -------------------------
	var own map[string]json.RawMessage
	ownBody := b.Leases(t, b.Token)
	if err := json.Unmarshal(ownBody, &own); err != nil {
		t.Fatalf("decode owner listing %q: %v", ownBody, err)
	}
	if len(own) != 1 {
		t.Errorf("owner listing keys = %v, want only \"leases\"", own)
	}
	ownSnap := decodeLeasesBody(t, ownBody)
	if len(ownSnap.Leases) != 1 || ownSnap.Leases[0].ID != cr.LeaseID {
		t.Fatalf("owner listing = %+v, want exactly lease %s", ownSnap.Leases, cr.LeaseID)
	}

	// --- a second valid principal: nothing at all --------------------------
	if got, want := string(b.Leases(t, b.OtherToken)), `{"leases":[]}`; got != want {
		t.Errorf("cross-principal listing = %s, want %s (no lease ids, no aggregates)", got, want)
	}

	// --- the operator: everything ------------------------------------------
	adminSnap := decodeLeasesBody(t, b.Leases(t, b.AdminToken))
	if len(adminSnap.Leases) != 1 || adminSnap.Leases[0].ID != cr.LeaseID {
		t.Fatalf("operator listing = %+v, want lease %s", adminSnap.Leases, cr.LeaseID)
	}
	if adminSnap.MaxConcurrent != 3 || adminSnap.SlotsInUse != 1 {
		t.Errorf("operator aggregates = {max %d, in-use %d}, want {3, 1}",
			adminSnap.MaxConcurrent, adminSnap.SlotsInUse)
	}
	if adminSnap.Leases[0].PID == 0 {
		t.Error("operator listing carries no pid; the operator view must be the full one")
	}
}

// decodeLeasesBody decodes a GET /leases body into a snapshot. Omitted
// aggregates decode as zero, which is why the suppression assertions above work
// on the raw bytes instead.
func decodeLeasesBody(t *testing.T, body []byte) RegistrySnapshot {
	t.Helper()
	var snap RegistrySnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode leases body %q: %v", body, err)
	}
	return snap
}

// stubClaimBody is the minimal valid claim body every stub-instance test posts.
const stubClaimBody = `{"config":"engine:\n  name: stub\n"}`

// TestAuthenticatedClaimSpawnsAndProxiesEndToEnd is the positive half of E1-S3:
// with client authentication ON, a claim carrying a valid credential still drives
// the WHOLE spine — the guard admits it, an instance spawns, dials back,
// registers and signals ready, the response carries {lease_id, ws_url,
// session_id}, and a client connects to ws_url and round-trips an IO frame.
//
// Handler unit tests cannot reach this: they stop at the boundary with a fake
// runner. What is proven here and nowhere else is that the middleware wraps the
// claim handler WITHOUT truncating it — an auth layer that consumed the request
// body, dropped the request context, or answered before delegating would pass
// every unit test in auth_test.go and fail here.
func TestAuthenticatedClaimSpawnsAndProxiesEndToEnd(t *testing.T) {
	stubBin := buildStubInstance(t)
	b := startAuthedStubBroker(t, stubBin)

	cr := b.Claim(t, b.Token, stubClaimBody)
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	// The session id is carried through even though the request went through the
	// guard — i.e. the post-ready session-report wait still ran.
	if cr.SessionID != "stub-new-session" {
		t.Fatalf("authenticated claim session_id = %q, want %q", cr.SessionID, "stub-new-session")
	}
	// Exactly one instance was spawned: the guard neither blocked the spawn nor
	// (via a retry/double-dispatch bug) doubled it.
	if got := b.SpawnCount(); got != 1 {
		t.Fatalf("spawn count = %d, want 1", got)
	}
	if !b.Registry.Has(cr.LeaseID) {
		t.Fatal("lease missing from the registry after an authenticated claim")
	}

	// The lease is genuinely live: round-trip a frame through the spawned
	// instance. The client WS is not behind the bearer middleware, but it resolves
	// the credential itself and enforces lease ownership, so the claimant presents
	// the same token it claimed with. (A single-use ticket becomes an accepted
	// alternative in a later story; the bearer path keeps working.)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := b.DialLease(ctx, cr.WSURL, b.Token)
	if err != nil {
		t.Fatalf("dial ws_url %s: %v", cr.WSURL, err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	out, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: cr.LeaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"hello":"authed"}`),
	})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := client.Write(ctx, websocket.MessageText, out); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	echo, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echo.Signal != brokerframe.SignalIO || string(echo.Payload) != `{"hello":"authed"}` {
		t.Fatalf("unexpected echo frame: %+v", echo)
	}
}

// TestUnauthenticatedClaimIsRefusedAndSpawnsNothing is the load-bearing half of
// E1-S3: a refused claim must cost NOTHING.
//
// The status code alone is not the assertion. A guard that ran after the claim
// handler — or a handler invoked before the middleware — would still answer 401
// while having already exec()d an instance, written a temp config, and burned a
// capacity slot. So this asserts the absence directly: zero spawn attempts
// through the only seam that reaches the OS (countingRunner), zero leases in the
// registry, zero slots in use and an empty capacity queue.
func TestUnauthenticatedClaimIsRefusedAndSpawnsNothing(t *testing.T) {
	stubBin := buildStubInstance(t)
	b := startAuthedStubBroker(t, stubBin)

	// Both flavours of "not authenticated" are refused: no header at all, and a
	// header carrying a token the operator never configured.
	for _, tc := range []struct{ name, token string }{
		{"no credential", ""},
		{"unconfigured credential", "not-a-configured-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := b.Do(t, http.MethodPost, "/claim", tc.token, stubClaimBody)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("claim status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
		})
	}

	// Nothing was spawned. commandRunner.start is the ONLY path from handleClaim
	// to a process, so zero starts is exhaustive: no stub-instance process was
	// ever created.
	if got := b.SpawnCount(); got != 0 {
		t.Errorf("spawn count = %d, want 0 — a refused claim spawned an instance "+
			"(the guard ran AFTER the claim handler)", got)
	}
	// And nothing was reserved: no lease, no capacity slot, no queued waiter.
	snap := b.Registry.Snapshot()
	if len(snap.Leases) != 0 {
		t.Errorf("leases after refused claims = %d, want 0: %+v", len(snap.Leases), snap.Leases)
	}
	if snap.SlotsInUse != 0 {
		t.Errorf("slots_in_use after refused claims = %d, want 0", snap.SlotsInUse)
	}
	if snap.QueueDepth != 0 {
		t.Errorf("queue_depth after refused claims = %d, want 0", snap.QueueDepth)
	}

	// A valid credential against the SAME server still works, so the zeros above
	// mean "refused" and not "this broker cannot spawn at all".
	cr := b.Claim(t, b.Token, stubClaimBody)
	if cr.LeaseID == "" {
		t.Fatalf("incomplete claim response with a valid credential: %+v", cr)
	}
	if got := b.SpawnCount(); got != 1 {
		t.Errorf("spawn count after one accepted claim = %d, want 1", got)
	}
}

// TestCrossPrincipalLeaseAccessIsRefused is the end-to-end ownership test:
// principal A claims a real spawned instance, and principal B — holding a
// perfectly valid credential of its own — is refused both mutating surfaces.
//
// The load-bearing assertion is not the status code. It is that A's instance is
// still ALIVE and still conversing after B's attempt: a check that answered 404
// only after releaseLease had queued the shutdown frame would pass a
// status-code-only test while having killed A's session. So this asserts the
// process never exited, the lease never entered teardown, and A can still
// round-trip a frame through the instance afterwards.
//
// B's WebSocket attempt is made BEFORE A connects, so its refusal cannot be
// mistaken for the one-client-per-lease rule.
func TestCrossPrincipalLeaseAccessIsRefused(t *testing.T) {
	stubBin := buildStubInstance(t)
	b := startAuthedStubBroker(t, stubBin)

	cr := b.Claim(t, b.Token, stubClaimBody)
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}

	// --- B attempts to release A's lease -----------------------------------
	unowned := b.Do(t, http.MethodPost, "/release/"+cr.LeaseID, b.OtherToken, "")
	if unowned.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-principal release status = %d, want 404", unowned.StatusCode)
	}
	// ...and it is indistinguishable from releasing a lease that never existed, so
	// B cannot use response differencing to learn that the id is live.
	unknown := b.Do(t, http.MethodPost, "/release/no-such-lease-id", b.OtherToken, "")
	unownedBody, err := io.ReadAll(unowned.Body)
	if err != nil {
		t.Fatalf("read unowned refusal body: %v", err)
	}
	unknownBody, err := io.ReadAll(unknown.Body)
	if err != nil {
		t.Fatalf("read unknown refusal body: %v", err)
	}
	if unknown.StatusCode != unowned.StatusCode || string(unknownBody) != string(unownedBody) {
		t.Errorf("refusals differ — lease-id oracle:\n unowned: %d %q\n unknown: %d %q",
			unowned.StatusCode, unownedBody, unknown.StatusCode, unknownBody)
	}

	// A's instance survived: the lease is still registered, holds its slot, and the
	// process has NOT exited (the shutdown frame was never sent).
	if !b.Registry.Has(cr.LeaseID) {
		t.Fatal("B's refused release tore down A's lease")
	}
	if got := b.Registry.SlotsInUse(); got != 1 {
		t.Errorf("slots in use = %d, want 1 (a refused release must free nothing)", got)
	}
	select {
	case <-b.Registry.ExitedChan(cr.LeaseID):
		t.Fatal("B's refused release killed A's instance process")
	default:
	}

	// --- B attempts to connect to A's lease --------------------------------
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	if conn, resp, err := b.DialLease(dctx, cr.WSURL, b.OtherToken); err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("B completed the client WebSocket handshake on A's lease")
	} else if resp == nil {
		t.Fatalf("B's dial returned no response: %v", err)
	} else if resp.StatusCode != http.StatusNotFound {
		t.Errorf("B's handshake status = %d, want 404", resp.StatusCode)
	}
	if got := b.Registry.ClientConn(cr.LeaseID); got != nil {
		t.Error("B's refused dial attached a client connection to A's lease")
	}

	// --- A is unaffected ---------------------------------------------------
	// The strongest liveness proof available: A connects and the spawned instance
	// echoes a frame back through the gateway.
	client, _, err := b.DialLease(dctx, cr.WSURL, b.Token)
	if err != nil {
		t.Fatalf("the lease owner was refused its own lease after B's attempts: %v", err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	out, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: cr.LeaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"hello":"still-alive"}`),
	})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := client.Write(dctx, websocket.MessageText, out); err != nil {
		t.Fatalf("owner write after refused cross-principal attempts: %v", err)
	}
	_, data, err := client.Read(dctx)
	if err != nil {
		t.Fatalf("owner read after refused cross-principal attempts: %v", err)
	}
	echo, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echo.Signal != brokerframe.SignalIO || string(echo.Payload) != `{"hello":"still-alive"}` {
		t.Fatalf("unexpected echo frame: %+v", echo)
	}

	// And A can still release its own lease through the full shared teardown.
	//
	// A's socket is closed first, purely for speed: the teardown closes the client
	// WS, and coder/websocket's close handshake waits ~5s for a peer that is not
	// currently reading. That wait is pre-existing behaviour with nothing to do with
	// ownership, so there is no reason to pay it here.
	client.Close(websocket.StatusNormalClosure, "")
	released := b.Do(t, http.MethodPost, "/release/"+cr.LeaseID, b.Token, "")
	if released.StatusCode != http.StatusOK {
		t.Fatalf("owner release status = %d, want 200", released.StatusCode)
	}
	waitFor(t, func() bool { return !b.Registry.Has(cr.LeaseID) })
}

// TestClaimTicketIssuedRefreshedAndInvalidatedOnRelease is the end-to-end half of
// E3-S2, over a REAL claimed lease with a live spawned instance: an authenticated
// claim hands back a redeemable ticket, its owner can mint a replacement, another
// valid principal cannot, and releasing the lease kills every ticket it holds.
//
// The load-bearing assertions are the redemptions, not the presence of a string in
// the JSON. A route that returned a random value the store had never recorded
// would satisfy every response-shape check and hand out a credential the
// WebSocket will refuse; and invalidation is only observable by redeeming, since
// the WebSocket does not accept `?ticket=` until E3-S3.
func TestClaimTicketIssuedRefreshedAndInvalidatedOnRelease(t *testing.T) {
	stubBin := buildStubInstance(t)
	b := startAuthedStubBroker(t, stubBin)

	cr := b.Claim(t, b.Token, stubClaimBody)
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	if cr.Ticket == "" {
		t.Fatal("authenticated claim returned no ticket; a browser client could never open the lease socket")
	}

	// --- the claim's ticket is real, and bound to THIS lease ----------------
	// Presented for another lease it is refused — and, because a failed binding
	// check is not a use, it survives for the invalidation assertion below.
	if _, ok := b.Tickets.redeem(cr.Ticket, "some-other-lease"); ok {
		t.Error("the claim's ticket was accepted for a different lease")
	}

	// A throwaway ticket proves the binding resolves to the CLAIMANT rather than to
	// the anonymous identity. It is minted (and consumed) separately so the claim's
	// own ticket stays unredeemed for the release assertion — redeeming that one
	// here would make the invalidation check vacuous.
	probe := b.Ticket(t, b.Token, cr.LeaseID)
	gotPrincipal, ok := b.Tickets.redeem(probe.Ticket, cr.LeaseID)
	if !ok {
		t.Fatal("a freshly minted ticket was not redeemable for its own lease")
	}
	if gotPrincipal != b.Principal {
		t.Errorf("ticket principal = %q, want %q (the claimant)", gotPrincipal, b.Principal)
	}
	if _, ok := b.Tickets.redeem(probe.Ticket, cr.LeaseID); ok {
		t.Error("a ticket was redeemable twice over HTTP-issued state; single use is not enforced")
	}

	// --- refresh: the owner gets a fresh, DIFFERENT ticket ------------------
	refreshed := b.Ticket(t, b.Token, cr.LeaseID)
	if refreshed.LeaseID != cr.LeaseID {
		t.Errorf("refresh lease_id = %q, want %q", refreshed.LeaseID, cr.LeaseID)
	}
	if refreshed.Ticket == "" {
		t.Fatal("refresh returned no ticket for the lease owner")
	}
	if refreshed.Ticket == cr.Ticket {
		t.Error("refresh returned the SAME ticket; a refresh must mint a new one, not re-serve a used one")
	}

	// --- a second valid principal is refused, indistinguishably from unknown -
	unowned := b.Do(t, http.MethodPost, "/ticket/"+cr.LeaseID, b.OtherToken, "")
	if unowned.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-principal refresh status = %d, want 404", unowned.StatusCode)
	}
	unknown := b.Do(t, http.MethodPost, "/ticket/no-such-lease-id", b.OtherToken, "")
	unownedBody, err := io.ReadAll(unowned.Body)
	if err != nil {
		t.Fatalf("read unowned refusal body: %v", err)
	}
	unknownBody, err := io.ReadAll(unknown.Body)
	if err != nil {
		t.Fatalf("read unknown refusal body: %v", err)
	}
	if unknown.StatusCode != unowned.StatusCode || string(unknownBody) != string(unownedBody) {
		t.Errorf("refusals differ — lease-id oracle:\n unowned: %d %q\n unknown: %d %q",
			unowned.StatusCode, unownedBody, unknown.StatusCode, unknownBody)
	}
	// The refused caller got nothing: the store holds exactly the two tickets A
	// was issued.
	if got := b.Tickets.outstanding(); got != 2 {
		t.Errorf("outstanding tickets = %d, want 2 (a refused refresh must mint nothing)", got)
	}

	// --- release invalidates every ticket for the lease ---------------------
	released := b.Do(t, http.MethodPost, "/release/"+cr.LeaseID, b.Token, "")
	if released.StatusCode != http.StatusOK {
		t.Fatalf("owner release status = %d, want 200", released.StatusCode)
	}
	waitFor(t, func() bool { return !b.Registry.Has(cr.LeaseID) })

	for name, value := range map[string]string{"claim": cr.Ticket, "refreshed": refreshed.Ticket} {
		if _, ok := b.Tickets.redeem(value, cr.LeaseID); ok {
			t.Errorf("the %s ticket is still redeemable after the lease was released", name)
		}
	}
	if got := b.Tickets.outstanding(); got != 0 {
		t.Errorf("outstanding tickets after release = %d, want 0", got)
	}
}

// TestBrowserTicketJourney is the whole point of the epic, end to end over a REAL
// claimed lease with a live spawned instance: a browser-shaped client, holding
// nothing but the URL and ticket a claim handed it, opens its own session socket,
// converses through it, cannot replay the ticket, and recovers with a refresh.
//
// It is deliberately the full journey rather than four separate cases, because the
// sequence is the property: claim → connect with `?ticket=` → round-trip an IO
// frame → reconnect with the SAME ticket refused → POST /ticket/{lease_id} →
// reconnect succeeds. The client presents NO Authorization header anywhere in it,
// which is exactly what a browser can do and the reason tickets exist.
//
// The round trips are the load-bearing assertions. A handshake that returned 101
// proves only that the gateway accepted a socket; echoing a frame off the spawned
// instance proves the connection was attached to the right lease and is genuinely
// usable — which a ticket path that resolved a principal but bound the socket to
// nothing would fail.
func TestBrowserTicketJourney(t *testing.T) {
	stubBin := buildStubInstance(t)
	b := startAuthedStubBroker(t, stubBin)

	cr := b.Claim(t, b.Token, stubClaimBody)
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	if cr.Ticket == "" {
		t.Fatal("authenticated claim returned no ticket; a browser client could never open the lease socket")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// --- connect with the claim's ticket, no header at all -------------------
	client, resp, err := b.DialLeaseTicket(ctx, cr.WSURL, cr.Ticket)
	if err != nil {
		t.Fatalf("dial %s with ?ticket=: %v", cr.WSURL, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("handshake status = %d, want 101", resp.StatusCode)
	}
	roundTripIOFrame(t, ctx, client, cr.LeaseID, `{"hello":"ticket"}`)

	// Close and wait for the lease to have no client, so the replay below is
	// refused on credential grounds and not by the one-client-per-lease rule.
	//
	// The client initiates this close, so the ~5s close-handshake wait a
	// server-initiated close pays against a non-reading peer does not apply — the
	// gateway's read pump is reading and answers immediately.
	client.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return b.Registry.ClientConn(cr.LeaseID) == nil })

	// --- the ticket is spent: a replay is refused before the upgrade ---------
	replay, replayResp, replayErr := b.DialLeaseTicket(ctx, cr.WSURL, cr.Ticket)
	if replayErr == nil {
		replay.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a spent ticket opened the lease socket a second time")
	}
	if replayResp == nil {
		t.Fatalf("replayed dial returned no response: %v", replayErr)
	}
	if replayResp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("replay handshake status = 101: the socket was upgraded and then closed, not refused")
	}
	if replayResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("replay handshake status = %d, want 401", replayResp.StatusCode)
	}
	if got := b.Registry.ClientConn(cr.LeaseID); got != nil {
		t.Error("the replayed dial attached a client to the lease")
	}
	// A's instance is untouched by the refusal: still leased, still holding its slot.
	if !b.Registry.Has(cr.LeaseID) {
		t.Fatal("the refused replay tore the lease down")
	}

	// --- refresh, and reconnect ---------------------------------------------
	// This is the leg that matters most: the 30s TTL means every real reconnect
	// (dropped socket, laptop resume) goes through this route, and re-claiming is
	// not an alternative because it would spawn a new instance and abandon the
	// live session.
	refreshed := b.Ticket(t, b.Token, cr.LeaseID)
	if refreshed.Ticket == "" {
		t.Fatal("refresh issued no ticket for the lease owner")
	}
	if refreshed.Ticket == cr.Ticket {
		t.Fatal("refresh re-served the spent ticket")
	}

	reconnected, _, err := b.DialLeaseTicket(ctx, cr.WSURL, refreshed.Ticket)
	if err != nil {
		t.Fatalf("dial with a refreshed ticket: %v", err)
	}
	defer reconnected.Close(websocket.StatusNormalClosure, "")
	// The SAME live instance is still on the other end — the session survived the
	// disconnect, which is the whole reason the refresh route exists.
	roundTripIOFrame(t, ctx, reconnected, cr.LeaseID, `{"hello":"reconnected"}`)
}

// roundTripIOFrame writes one IO frame into a client socket and asserts the stub
// instance echoes it back unchanged. It is the strongest liveness proof available
// over a broker connection: the frame has to reach the spawned process and come
// back through the gateway's routing.
func roundTripIOFrame(t *testing.T, ctx context.Context, conn *websocket.Conn, leaseID, payload string) {
	t.Helper()
	out, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(payload),
	})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, out); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	echo, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echo.Signal != brokerframe.SignalIO || string(echo.Payload) != payload {
		t.Fatalf("unexpected echo frame: %+v (want payload %s)", echo, payload)
	}
}

// TestClaimOmitsTicketWhenAuthDisabled is the backward-compatibility half: with no
// `auth:` block a claim carries NO ticket field at all, and the client WebSocket
// still opens with no ticket and no credential — so an existing deployment sees
// nothing change.
func TestClaimOmitsTicketWhenAuthDisabled(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, _ := startStubBrokerWithRegistry(t, stubBin)

	// Read the RAW body: decoding into claimResponse turns an omitted `ticket` into
	// an indistinguishable empty string, and "absent" is the property under test.
	resp := postClaimRaw(t, base, stubClaimBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read claim body: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode claim body %q: %v", raw, err)
	}
	if _, present := fields["ticket"]; present {
		t.Errorf("claim body carries a ticket with auth disabled: %s", raw)
	}

	var cr claimResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}

	// And the lease socket still opens with no ticket and no Authorization header.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, cr.WSURL, nil)
	if err != nil {
		t.Fatalf("dial ws_url %s with no credential and no ticket: %v", cr.WSURL, err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	// Ticket refresh is inert rather than an error: 200, no ticket.
	tr, err := http.Post("http://"+base+"/ticket/"+cr.LeaseID, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /ticket: %v", err)
	}
	defer tr.Body.Close()
	if tr.StatusCode != http.StatusOK {
		t.Fatalf("ticket refresh status = %d, want 200 with auth disabled", tr.StatusCode)
	}
	trBody, err := io.ReadAll(tr.Body)
	if err != nil {
		t.Fatalf("read ticket body: %v", err)
	}
	var trFields map[string]json.RawMessage
	if err := json.Unmarshal(trBody, &trFields); err != nil {
		t.Fatalf("decode ticket body %q: %v", trBody, err)
	}
	if _, present := trFields["ticket"]; present {
		t.Errorf("ticket refresh issued a ticket with auth disabled: %s", trBody)
	}
}

// TestInstanceDialBackRequiresSpawnSecret is E5-S1 end to end on an
// authenticated broker: a REAL spawned instance registers and converses, and a
// hand-rolled dial-back holding a VALID lease id but the wrong secret is refused.
//
// The two halves have to be one test. The positive half alone would pass against
// a broker that checks nothing; the negative half alone would pass against a
// broker that refuses every dial-back, which is indistinguishable from a broken
// gateway. Together they say the check is both present and correct.
//
// The negative half uses a lease minted directly on the broker's registry rather
// than a claimed one, because a claimed lease already holds its instance
// connection — a second dial-back would then be refused by the one-instance rule
// and the assertion would prove nothing about the secret. The correct-secret dial
// at the end closes that gap from the other side.
func TestInstanceDialBackRequiresSpawnSecret(t *testing.T) {
	stubBin := buildStubInstance(t)
	b := startAuthedStubBroker(t, stubBin)

	// --- a real spawn registers and is genuinely usable ---------------------
	// The stub echoes the injected NEXUS_BROKER_SPAWN_SECRET in its register
	// frame exactly as nexus.io.broker does. If the broker minted a secret it did
	// not inject, or enforced one it did not mint, this claim would never become
	// ready.
	cr := b.Claim(t, b.Token, stubClaimBody)
	if cr.LeaseID == "" || cr.WSURL == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := b.DialLease(ctx, cr.WSURL, b.Token)
	if err != nil {
		t.Fatalf("dial ws_url %s: %v", cr.WSURL, err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	roundTripIOFrame(t, ctx, client, cr.LeaseID, `{"hello":"spawn-secret"}`)

	// --- a hand-rolled dial-back with a valid lease id but a wrong secret ----
	const minted = "9d4e7a10c3b28f56ae0192b3c4d5e6f7"
	leaseID, err := b.Registry.NewLease(nexusauth.Principal{ID: b.Principal})
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}
	b.Registry.SetSpawnSecret(leaseID, minted)
	instanceURL := "ws://" + b.Base + instanceWSPath

	impostor := dialInstance(t, ctx, instanceURL)
	defer impostor.Close(websocket.StatusNormalClosure, "")
	writeInstanceFrame(t, ctx, impostor, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  "00000000000000000000000000000000",
	})
	rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
	_, _, rerr := impostor.Read(rctx)
	rcancel()
	if rerr == nil {
		t.Fatal("the broker kept an impostor dial-back open: a valid lease id alone was enough")
	}
	if got := websocket.CloseStatus(rerr); got != websocket.StatusPolicyViolation {
		t.Errorf("impostor close status = %v, want %v (the same close an unknown lease gets)",
			got, websocket.StatusPolicyViolation)
	}
	if b.Registry.InstanceConn(leaseID) != nil {
		t.Fatal("the impostor was attached to the lease")
	}

	// Non-vacuity: the SAME lease over the SAME endpoint accepts the minted
	// secret, so the refusal above is the secret check rather than a gateway that
	// refuses everything hand-rolled.
	legit := dialInstance(t, ctx, instanceURL)
	defer legit.Close(websocket.StatusNormalClosure, "")
	writeInstanceFrame(t, ctx, legit, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  minted,
	})
	waitFor(t, func() bool { return b.Registry.InstanceConn(leaseID) != nil })

	// And the real claimed lease is untouched by any of it.
	if !b.Registry.Has(cr.LeaseID) {
		t.Error("the impostor's refusal disturbed the live claimed lease")
	}
}

// dialInstance opens a raw WebSocket to the broker's instance dial-back
// endpoint, standing in for a spawned process.
func dialInstance(t *testing.T, ctx context.Context, url string) *websocket.Conn {
	t.Helper()
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return conn
}

// writeInstanceFrame encodes and writes one frame to a hand-rolled instance
// connection.
func writeInstanceFrame(t *testing.T, ctx context.Context, conn *websocket.Conn, f brokerframe.Frame) {
	t.Helper()
	data, err := brokerframe.Encode(f)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// TestHealthzStaysOpenOnAuthEnabledBroker pins the deliberate exemption at the
// integration layer: healthz is registered on the raw mux, so a probe with no
// credential gets 200 from a broker whose control plane is refusing anonymous
// callers on the very same listener.
func TestHealthzStaysOpenOnAuthEnabledBroker(t *testing.T) {
	stubBin := buildStubInstance(t)
	b := startAuthedStubBroker(t, stubBin)

	// Precondition, checked on the same server: a guarded route refuses an
	// anonymous caller. Without this, a 200 from healthz could just mean auth was
	// never enabled.
	if resp := b.Do(t, http.MethodGet, "/leases", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /leases = %d, want 401 (precondition: auth must be ON)", resp.StatusCode)
	}

	resp := b.Do(t, http.MethodGet, "/healthz", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET /healthz = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("healthz status = %q, want %q", body["status"], "ok")
	}
}

// ---------------------------------------------------------------------------
// Shared auth-enabled integration fixture
// ---------------------------------------------------------------------------

// Credentials the auth-enabled integration fixture is built around.
//
// THREE principals are configured, not one, even though E1-S3 only needs the
// first: E2-S2's cross-principal ownership test needs a second *valid* identity
// to be refused a lease it does not own, and E2-S3's scoped `GET /leases` needs a
// third holding the operator scope. Minting them here means those stories extend
// this fixture instead of forking it.
const (
	authedPrimaryToken     = "integration-primary-token"
	authedPrimaryPrincipal = "integration-primary"

	authedOtherToken     = "integration-other-token"
	authedOtherPrincipal = "integration-other"

	authedAdminToken     = "integration-admin-token"
	authedAdminPrincipal = "integration-admin"
)

// integrationAuthYAML is the broker `auth:` block the fixture loads. Static
// tokens are used on purpose: they are the one validator type that needs no
// network and no clock, which is what keeps this harness key-free and offline.
//
// It sets no `admin_scope`, so the operator credential carries the DEFAULT scope
// — which makes the out-of-the-box configuration the one exercised end to end.
const integrationAuthYAML = `
auth:
  validators:
    - type: static
      tokens:
        - token: "` + authedPrimaryToken + `"
          principal: "` + authedPrimaryPrincipal + `"
          tenant: "acme"
        - token: "` + authedOtherToken + `"
          principal: "` + authedOtherPrincipal + `"
          tenant: "acme"
        - token: "` + authedAdminToken + `"
          principal: "` + authedAdminPrincipal + `"
          tenant: "acme"
          scopes: "` + defaultAdminScope + `"
`

// authedStubBroker is the shared auth-enabled integration fixture: a
// stub-instance broker wired exactly as run() wires the real one but with client
// authentication ON, plus the credentials and request helpers a test needs to
// drive it.
//
// It is the deliberate seam for the stories that build on E1-S3 — E2-S2 (lease
// ownership across principals) and E3-S3 (single-use WebSocket tickets) both
// need "an auth-enabled broker plus an authenticated client" and must NOT each
// re-derive it; a second copy is a second thing to forget when the guard
// changes. Extend this type (another credential, another helper) rather than
// starting a parallel fixture.
type authedStubBroker struct {
	// Base is the broker's host:port.
	Base string

	// Registry is the broker's live lease registry, so a test can assert lease
	// presence — or, for a refusal, absence — directly rather than inferring it
	// from HTTP responses.
	Registry *Registry

	// Tickets is the broker's live ticket store, so a test can redeem or count
	// tickets directly. Redemption has no HTTP surface until the WebSocket accepts
	// `?ticket=` (E3-S3), so this is the only way to prove an issued ticket is
	// genuinely redeemable — and, after a teardown, genuinely is not.
	Tickets *ticketStore

	// Token is the primary caller's bearer token and Principal the identity it
	// resolves to.
	Token     string
	Principal string

	// OtherToken is a second VALID credential resolving to OtherPrincipal, for
	// tests about one principal reaching another's resources.
	OtherToken     string
	OtherPrincipal string

	// AdminToken is a third VALID credential carrying the broker's operator scope,
	// for tests about the unrestricted introspection view.
	AdminToken     string
	AdminPrincipal string

	// spawns is the counting wrapper around the real exec runner, read via
	// SpawnCount.
	spawns *countingRunner
}

// startAuthedStubBroker boots a stub-instance broker with client authentication
// enabled and returns the fixture. Extra options are applied AFTER the auth and
// runner defaults, so a caller can still override capacity, grace, or the runner.
func startAuthedStubBroker(t *testing.T, stubBin string, opts ...stubBrokerOption) *authedStubBroker {
	t.Helper()
	spawns := &countingRunner{inner: execRunner{}}
	all := append([]stubBrokerOption{
		withAuthFromYAML(t, integrationAuthYAML),
		withCommandRunner(spawns),
	}, opts...)
	base, reg := startStubBrokerWithRegistry(t, stubBin, all...)
	return &authedStubBroker{
		Base:     base,
		Registry: reg,
		// The store the broker actually wired, read back off the registry rather
		// than constructed a second time here — a fixture-local copy would let the
		// tests pass while the served broker used a different store.
		Tickets:        reg.tickets,
		Token:          authedPrimaryToken,
		Principal:      authedPrimaryPrincipal,
		OtherToken:     authedOtherToken,
		OtherPrincipal: authedOtherPrincipal,
		AdminToken:     authedAdminToken,
		AdminPrincipal: authedAdminPrincipal,
		spawns:         spawns,
	}
}

// URL builds an absolute http URL for a broker path.
func (b *authedStubBroker) URL(path string) string { return "http://" + b.Base + path }

// Do performs an HTTP request against the broker, asserting nothing about the
// result so a caller can inspect a refusal.
//
// An empty token sends NO Authorization header at all — that is what an
// unauthenticated caller looks like on the wire, and it is a different case from
// presenting a wrong one (missing credential vs invalid credential).
func (b *authedStubBroker) Do(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, b.URL(path), strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// Claim performs an authenticated POST /claim and decodes the success response,
// failing on any non-200. It is postClaimJSON plus a credential.
func (b *authedStubBroker) Claim(t *testing.T, token, body string) claimResponse {
	t.Helper()
	resp := b.Do(t, http.MethodPost, "/claim", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d, want 200", resp.StatusCode)
	}
	var cr claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	return cr
}

// Leases performs an authenticated GET /leases and returns the RAW response
// body, failing on any non-200.
//
// The raw bytes are returned rather than a decoded snapshot because the
// caller-scoped shape is defined by which keys are ABSENT, and decoding into
// RegistrySnapshot turns an omitted aggregate into an indistinguishable zero.
func (b *authedStubBroker) Leases(t *testing.T, token string) []byte {
	t.Helper()
	resp := b.Do(t, http.MethodGet, "/leases", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /leases status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read leases body: %v", err)
	}
	return bytes.TrimSpace(body)
}

// Ticket performs an authenticated POST /ticket/{lease_id} and decodes the
// success response, failing on any non-200. Use Do directly to inspect a refusal.
func (b *authedStubBroker) Ticket(t *testing.T, token, leaseID string) ticketResponse {
	t.Helper()
	resp := b.Do(t, http.MethodPost, "/ticket/"+leaseID, token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ticket/%s status = %d, want 200", leaseID, resp.StatusCode)
	}
	var tr ticketResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode ticket response: %v", err)
	}
	return tr
}

// SpawnCount is how many instance spawns the broker has attempted. Zero after a
// refused request is the proof that the refusal preceded the spawn.
func (b *authedStubBroker) SpawnCount() int64 { return b.spawns.count() }

// DialLease opens the client WebSocket for a lease with an optional bearer token
// ("" sends no Authorization header at all), asserting nothing so a caller can
// inspect a refusal. On a failed handshake the conn is nil and the *http.Response
// carries the broker's refusal.
func (b *authedStubBroker) DialLease(ctx context.Context, wsURL, token string) (*websocket.Conn, *http.Response, error) {
	var opts *websocket.DialOptions
	if token != "" {
		opts = &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
		}
	}
	return websocket.Dial(ctx, wsURL, opts)
}

// DialLeaseTicket opens the client WebSocket for a lease presenting a single-use
// ticket as a query parameter and NO Authorization header — the browser-shaped
// request, since browser JavaScript cannot set headers on a WebSocket handshake.
//
// It asserts nothing, so a caller can inspect a refusal; on a failed handshake the
// conn is nil and the *http.Response carries the broker's refusal.
func (b *authedStubBroker) DialLeaseTicket(ctx context.Context, wsURL, ticket string) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, wsURL+"?"+ticketQueryParam+"="+url.QueryEscape(ticket), nil)
}

// getLeasesIntegration performs GET /leases against a running stub broker and
// decodes the snapshot.
func getLeasesIntegration(t *testing.T, base string) RegistrySnapshot {
	t.Helper()
	resp, err := http.Get("http://" + base + "/leases")
	if err != nil {
		t.Fatalf("GET /leases: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /leases status = %d, want 200", resp.StatusCode)
	}
	var snap RegistrySnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode leases snapshot: %v", err)
	}
	return snap
}

// postClaimRaw posts a claim body and returns the raw response without asserting
// status, so a test can inspect a non-200 (e.g. a 503 capacity rejection).
func postClaimRaw(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := http.Post("http://"+base+"/claim", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /claim: %v", err)
	}
	return resp
}

// TestBrokerRestartReattachesLiveInstance is E5's payoff, end to end: claim an
// instance, kill the broker WITHOUT releasing it, bring a new broker back on the
// same address over the same state_dir, and watch the surviving instance
// reattach — after which the client WebSocket works again.
//
// It is deliberately built out of the real pieces: the real journal, the real
// spawn-key derivation, the real recovery pass, and a stub instance running the
// same reconnect-with-backoff loop the nexus.io.broker plugin runs. Nothing about
// the reattach is simulated except the engine at the far end.
func TestBrokerRestartReattachesLiveInstance(t *testing.T) {
	// The stub must survive its broker and keep dialing, exactly as the real
	// plugin does. Without this it exits on the first read error and there would be
	// nothing left to reclaim.
	t.Setenv("STUB_RECONNECT", "1")
	stubBin := buildStubInstance(t)

	stateDir := t.TempDir()
	addr := freeAddr(t)

	// --- broker #1: claim an instance -------------------------------------
	first := startStubBrokerHandle(t, stubBin,
		withStateDir(stateDir), withListenAddr(addr))
	cr := postClaimJSON(t, first.base, `{"config":"engine:\n  name: stub\n"}`)
	if cr.LeaseID == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	pid := first.registry.PID(cr.LeaseID)
	if pid == 0 {
		t.Fatal("claimed lease has no pid recorded")
	}
	killInstanceOnCleanup(t, pid)

	// --- the broker dies, the instance does not ---------------------------
	// stop() closes the server and cancels the pumps; it releases nothing, so the
	// stub process is still running and its lease is still recorded as live.
	first.stop()
	if !processAlive(pid) {
		t.Fatal("stopping the broker killed its instance; there is nothing to reclaim")
	}

	// --- broker #2: same address, same state_dir --------------------------
	second := startStubBrokerHandle(t, stubBin,
		withStateDir(stateDir), withListenAddr(addr),
		// Long enough that a slow reconnect is not mistaken for a failure to
		// reattach; the test asserts on reattach, not on the reaper.
		withReattachWindow(30*time.Second))

	if len(second.restored) != 1 || second.restored[0] != cr.LeaseID {
		t.Fatalf("restored = %v, want [%s]", second.restored, cr.LeaseID)
	}
	// The slot came back with it, so the restarted broker cannot over-admit.
	if got := second.registry.SlotsInUse(); got != 1 {
		t.Errorf("slots_in_use after recovery = %d, want 1", got)
	}
	// And so did the pid, which is what the reaper would act on.
	if got := second.registry.PID(cr.LeaseID); got != pid {
		t.Errorf("restored pid = %d, want %d", got, pid)
	}

	// The instance is still dialing on its backoff; give it time to land. Until it
	// presents the derived spawn secret the lease reads as `spawning`.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if second.registry.InstanceConn(cr.LeaseID) != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if second.registry.InstanceConn(cr.LeaseID) == nil {
		t.Fatal("the surviving instance never reattached to the restarted broker")
	}

	// --- the lease is usable again ----------------------------------------
	snap := second.registry.Snapshot()
	if len(snap.Leases) != 1 || snap.Leases[0].State != surfaceStateActive {
		t.Fatalf("restored lease snapshot = %+v, want one lease in state %q",
			snap.Leases, surfaceStateActive)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws://" + second.base + ClientWSPath(cr.LeaseID)
	client, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial the restored lease at %s: %v", wsURL, err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")

	out, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: cr.LeaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"after":"restart"}`),
	})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := client.Write(ctx, websocket.MessageText, out); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	echo, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echo.Signal != brokerframe.SignalIO || string(echo.Payload) != `{"after":"restart"}` {
		t.Fatalf("unexpected echo through the restored lease: %+v", echo)
	}

	// And the lease can still be released normally, which proves it is an ordinary
	// lease now and not a special case.
	resp, err := http.Post("http://"+second.base+"/release/"+cr.LeaseID, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release of a restored lease = %d, want 200", resp.StatusCode)
	}
	if second.registry.Has(cr.LeaseID) {
		t.Error("a restored lease survived its release")
	}
}

// TestBrokerRestartReapsUnreattachedLease is the other half of the contract: an
// instance that does NOT come back does not hold a capacity slot forever. Its
// stub is spawned without STUB_RECONNECT, so it exits when the first broker dies
// — but the journal still records a lease, and a pid that the OS may hand to
// something else. The restarted broker must close it out either way.
func TestBrokerRestartReapsUnreattachedLease(t *testing.T) {
	stubBin := buildStubInstance(t)
	stateDir := t.TempDir()
	addr := freeAddr(t)

	first := startStubBrokerHandle(t, stubBin,
		withStateDir(stateDir), withListenAddr(addr))
	cr := postClaimJSON(t, first.base, `{"config":"engine:\n  name: stub\n"}`)
	if cr.LeaseID == "" {
		t.Fatalf("incomplete claim response: %+v", cr)
	}
	pid := first.registry.PID(cr.LeaseID)
	first.stop()

	// The non-reconnecting stub exits when its broker goes away. Wait for that so
	// the restart sees a settled state rather than a racing one.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(20 * time.Millisecond)
	}

	second := startStubBrokerHandle(t, stubBin,
		withStateDir(stateDir), withListenAddr(addr),
		withReattachWindow(200*time.Millisecond),
		withReleaseGrace(200*time.Millisecond))

	// Either the pid was already gone at boot (dropped outright by recovery) or it
	// lingered long enough to be restored and then reaped by the window. Both are
	// correct; what matters is that no lease and no slot survives.
	waitForNoLease := time.Now().Add(20 * time.Second)
	for time.Now().Before(waitForNoLease) {
		if !second.registry.Has(cr.LeaseID) && second.registry.SlotsInUse() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("an unreattached lease survived the reattach window: has=%v slots=%d",
		second.registry.Has(cr.LeaseID), second.registry.SlotsInUse())
}

// TestRestoredLeaseRefusesAWrongSecretEndToEnd proves the identity gate over the
// real wire: a dialer that knows a restored lease id but not its spawn secret is
// refused at /instance, even though this broker has no `auth:` block at all.
func TestRestoredLeaseRefusesAWrongSecretEndToEnd(t *testing.T) {
	t.Setenv("STUB_RECONNECT", "1")
	stubBin := buildStubInstance(t)
	stateDir := t.TempDir()
	addr := freeAddr(t)

	first := startStubBrokerHandle(t, stubBin, withStateDir(stateDir), withListenAddr(addr))
	cr := postClaimJSON(t, first.base, `{"config":"engine:\n  name: stub\n"}`)
	killInstanceOnCleanup(t, first.registry.PID(cr.LeaseID))
	first.stop()

	second := startStubBrokerHandle(t, stubBin,
		withStateDir(stateDir), withListenAddr(addr), withReattachWindow(30*time.Second))
	if len(second.restored) != 1 {
		t.Fatalf("restored = %v, want the claimed lease", second.restored)
	}

	// Race the genuine instance's reconnect: dial /instance directly with the right
	// lease id and a wrong secret, and require a refusal. The lease id alone is not
	// enough even though the broker is unauthenticated.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+second.base+instanceWSPath, nil)
	if err != nil {
		t.Fatalf("dial /instance: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	frame, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: cr.LeaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  "definitely-not-the-derived-secret",
	})
	if err != nil {
		t.Fatalf("encode register: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write register: %v", err)
	}
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("a register frame with the wrong secret was accepted for a restored lease")
	}
}

// brokerWiring is the complete description of a stub broker under test: its
// Config plus the commandRunner that spawns instances. Options mutate it before
// startStubBrokerWithRegistry builds anything, so a test can vary both the
// config (grace, caps, auth) and the spawn seam (a counting wrapper) through one
// mechanism rather than two.
type brokerWiring struct {
	cfg    Config
	runner commandRunner
}

// stubBrokerOption tweaks the broker wiring used by startStubBrokerWithRegistry.
type stubBrokerOption func(*brokerWiring)

// withReleaseGrace overrides the release grace period for a stub broker.
func withReleaseGrace(d time.Duration) stubBrokerOption {
	return func(w *brokerWiring) { w.cfg.ReleaseGrace = d }
}

// withIdleTimeout overrides the idle timeout for a stub broker, arming the idle
// sweeper. A non-positive value disables idle reaping.
func withIdleTimeout(d time.Duration) stubBrokerOption {
	return func(w *brokerWiring) { w.cfg.IdleTimeout = d }
}

// withMaxConcurrent caps the number of live instances for a stub broker. A
// non-positive value means unlimited.
func withMaxConcurrent(n int) stubBrokerOption {
	return func(w *brokerWiring) { w.cfg.MaxConcurrent = n }
}

// withQueueWaitTimeout sets how long an over-capacity claim parks in the FIFO
// queue before timing out. A non-positive value disables waiting.
func withQueueWaitTimeout(d time.Duration) stubBrokerOption {
	return func(w *brokerWiring) { w.cfg.QueueWaitTimeout = d }
}

// withAuthFromYAML enables client authentication on a stub broker by loading a
// broker `auth:` block through the REAL config path — LoadConfigFromBytes, which
// hands the block to nexusauth.ChainFromMap. Going through the loader rather
// than hand-assembling a Chain is deliberate: it proves that YAML an operator
// could actually write results in enforcement, so a regression in the
// config → chain wiring cannot hide behind a chain the test built itself.
func withAuthFromYAML(t *testing.T, yaml string) stubBrokerOption {
	t.Helper()
	loaded := mustLoadConfig(t, yaml)
	if !loaded.AuthChain.Enabled() {
		t.Fatalf("withAuthFromYAML: the supplied YAML produced a DISABLED chain; the test would prove nothing:\n%s", yaml)
	}
	return func(w *brokerWiring) {
		w.cfg.Auth = loaded.Auth
		w.cfg.AuthChain = loaded.AuthChain
		// The operator scope comes from the same load, so a fixture that sets
		// `admin_scope:` is honoured and one that does not gets the real default.
		w.cfg.AdminScope = loaded.AdminScope
	}
}

// withStateDir points a stub broker at a lease journal directory, enabling
// durability and — on a second broker started over the same directory — restart
// recovery.
func withStateDir(dir string) stubBrokerOption {
	return func(w *brokerWiring) { w.cfg.StateDir = dir }
}

// withListenAddr pins the address a stub broker binds to, instead of letting the
// kernel pick one. It exists for the restart test: a restarted broker must come
// back on the SAME address, because that is what the surviving instance's
// reconnect loop is dialing.
func withListenAddr(addr string) stubBrokerOption {
	return func(w *brokerWiring) { w.cfg.ListenAddr = addr }
}

// withReattachWindow bounds how long a restored lease waits for its instance
// before being reaped.
func withReattachWindow(d time.Duration) stubBrokerOption {
	return func(w *brokerWiring) { w.cfg.ReattachWindow = d }
}

// withCommandRunner replaces the spawn seam. It exists so a test can count (or
// refuse) instance spawns while still exec()ing the real stub binary underneath
// — see countingRunner.
func withCommandRunner(r commandRunner) stubBrokerOption {
	return func(w *brokerWiring) { w.runner = r }
}

// countingRunner wraps a commandRunner and counts every start() call.
//
// It is the seam that makes "no instance was spawned" an assertion rather than
// an assumption. handleClaim reaches the OS only through commandRunner.start, so
// a zero count is exhaustive proof that no stub-instance process was created —
// and a middleware that returned 401 only AFTER spawning would show a count of 1
// while still answering 401, which a status-code-only test cannot distinguish.
type countingRunner struct {
	inner  commandRunner
	starts atomic.Int64
}

// start records the attempt and delegates to the wrapped runner. The counter is
// incremented BEFORE delegating so a spawn that fails to exec still counts as an
// attempt: the property under test is "the handler was reached", not "a process
// survived".
func (r *countingRunner) start(ctx context.Context, spec spawnSpec) (processHandle, error) {
	r.starts.Add(1)
	return r.inner.start(ctx, spec)
}

// count returns the number of spawn attempts observed so far.
func (r *countingRunner) count() int64 { return r.starts.Load() }

// startStubBroker binds a real listener, wires the gateway + claim handler over
// it pointing nexus_binary_path at the stub, serves it, and returns the broker's
// base host:port. The server is torn down via t.Cleanup.
func startStubBroker(t *testing.T, stubBin string) string {
	base, _ := startStubBrokerWithRegistry(t, stubBin)
	return base
}

// startStubBrokerWithRegistry is startStubBroker plus the shared registry, so a
// test can assert lease presence directly. It also wires the release endpoint.
func startStubBrokerWithRegistry(t *testing.T, stubBin string, opts ...stubBrokerOption) (string, *Registry) {
	h := startStubBrokerHandle(t, stubBin, opts...)
	return h.base, h.registry
}

// brokerHandle is a running stub broker a test can stop mid-test. It exists for
// the restart story: every other test lets t.Cleanup tear the broker down at the
// end, but a restart test has to stop one broker and start another over the same
// state_dir and the same address while its instance is still running.
type brokerHandle struct {
	base     string
	registry *Registry

	// restored is what recovery brought back at this broker's boot — nil for a
	// cold start.
	restored []string

	// stop shuts the broker down WITHOUT releasing its leases, the way a crash or
	// a SIGKILL would. It is idempotent and also runs from t.Cleanup.
	stop func()
}

// startStubBrokerHandle wires and serves one stub broker, mirroring run()'s
// topology, and returns a handle that can stop it independently of t.Cleanup.
func startStubBrokerHandle(t *testing.T, stubBin string, opts ...stubBrokerOption) *brokerHandle {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	wiring := brokerWiring{
		cfg: Config{
			NexusBinaryPath: stubBin,
			ReleaseGrace:    defaultReleaseGrace,
			ReattachWindow:  defaultReattachWindow,
			AdminScope:      defaultAdminScope,
		},
		runner: execRunner{},
	}
	for _, opt := range opts {
		opt(&wiring)
	}

	// Bind a real listener before wiring the claim handler (it needs the address
	// to build the instance dial-back URL). An option may have pinned the address;
	// otherwise the kernel picks one.
	bindAddr := wiring.cfg.ListenAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		t.Fatalf("listen on %s: %v", bindAddr, err)
	}
	wiring.cfg.ListenAddr = ln.Addr().String()

	cfg := wiring.cfg
	registry := NewRegistry(logger, cfg.MaxConcurrent)

	// Lease durability + restart recovery, wired exactly as run() does. With no
	// state_dir the store is nil and every one of these is inert.
	var leaseStore LeaseStore
	var restored []string
	var spawnSecretKey spawnKey
	if cfg.StateDir != "" {
		var brokerID string
		leaseStore, brokerID, err = openLeaseStore(logger, cfg)
		if err != nil {
			t.Fatalf("open lease store: %v", err)
		}
		spawnSecretKey, err = loadSpawnKey(logger, cfg.StateDir)
		if err != nil {
			t.Fatalf("load spawn key: %v", err)
		}
		registry.useLeaseStore(leaseStore, brokerID, cfg.AdvertiseAddr)
		restored = recoverLeases(logger, registry, leaseStore, brokerID, spawnSecretKey)
	}

	// The guard is built before the gateway, exactly as run() does: the client WS
	// route is not registered through Guard, so the gateway resolves that route's
	// credential itself in order to enforce lease ownership.
	guard := newAuthGuard(logger, cfg.AuthChain)
	// Ticket wiring mirrors run() too: the store's inert/live state is decided by
	// the guard, and the registry invalidates a lease's tickets when it tears the
	// lease down.
	tickets := newTicketStore(logger, guard.enabled())
	registry.useTicketStore(tickets)
	gateway := NewGateway(logger, registry, guard, tickets)
	claims := NewClaimServer(logger, registry, cfg, wiring.runner, tickets)
	// Spawn secrets are derived from the broker's key when a state_dir is
	// configured, so a restarted broker can recompute what a surviving instance
	// still holds. Nil key = random per spawn, the pre-existing behaviour.
	claims.useSpawnKey(spawnSecretKey)
	claims.readyTimeout = 15 * time.Second
	releases := NewReleaseServer(logger, registry, cfg.ReleaseGrace)
	leases := NewLeasesServer(logger, registry, guard, cfg.AdminScope)
	ticketsServer := NewTicketServer(logger, registry, tickets)

	// Mirror run()'s route topology exactly, because middleware ORDERING is the
	// property the auth integration tests exist to catch: healthz and the
	// WebSocket routes on the raw mux, the client-facing control plane behind the
	// guard. With no `auth:` configured, Guard returns the mux unchanged, so an
	// auth-absent test sees byte-for-byte the wiring it saw before auth existed.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	gateway.Register(mux)
	guarded := guard.Guard(mux)
	claims.Register(guarded)
	releases.Register(guarded)
	leases.Register(guarded)
	ticketsServer.Register(guarded)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	// Arm the idle sweeper exactly as main.go does when an idle timeout is set.
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	sweeper := newIdleSweeper(logger, registry, cfg.IdleTimeout, cfg.ReleaseGrace)
	go sweeper.Run(sweepCtx)
	// And the reattach reaper, which bounds how long the leases recovery just
	// restored may wait for their instances.
	go reapUnreattached(sweepCtx, logger, registry, restored, cfg.ReattachWindow, cfg.ReleaseGrace)

	// stop tears the SERVER down and leaves every lease (and every spawned
	// instance) exactly where it is — a crash, not a drain. That is the whole
	// point for the restart test: releasing on the way out would kill the very
	// instance the next broker is supposed to reclaim.
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			stopSweep()
			_ = srv.Close()
			gateway.Shutdown()
			if leaseStore != nil {
				_ = leaseStore.Close()
			}
		})
	}
	t.Cleanup(stop)

	return &brokerHandle{
		base:     ln.Addr().String(),
		registry: registry,
		restored: restored,
		stop:     stop,
	}
}

// killInstanceOnCleanup makes sure a spawned stub does not outlive the test.
//
// It matters only for the restart tests: those deliberately stop a broker WITHOUT
// releasing its leases, so nothing in the normal teardown path kills the child —
// and a reconnecting stub inherits the test binary's stderr, which keeps `go
// test` waiting on its own output pipe long after the test has passed.
func killInstanceOnCleanup(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() {
		if pid > 0 && processAlive(pid) {
			_ = (&adoptedProcess{id: pid}).kill()
		}
	})
}

// freeAddr reserves and immediately releases a loopback port, returning the
// address. It is how the restart test pins an address BOTH brokers can bind: the
// second broker must come back where the surviving instance is dialing, so the
// address cannot be whatever the kernel happens to hand out the second time.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return addr
}

// postClaimJSON posts a claim body to the broker and returns the decoded
// success response, failing the test on any non-200 status.
func postClaimJSON(t *testing.T, base, body string) claimResponse {
	t.Helper()
	resp, err := http.Post("http://"+base+"/claim", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /claim: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d, want 200", resp.StatusCode)
	}
	var cr claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	return cr
}

// buildStubInstance compiles the testdata stub instance to a temp binary and
// returns its path.
func buildStubInstance(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stubinstance")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/stubinstance")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub instance: %v\n%s", err, out)
	}
	return bin
}
