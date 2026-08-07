//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
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
	// instance. The client WS is NOT behind the bearer guard (it is
	// ticket-authenticated in a later story), so it dials with no credential.
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
// TWO principals are configured, not one, even though E1-S3 only needs the
// first: E2-S2's cross-principal ownership test needs a second *valid* identity
// to be refused a lease it does not own, and minting it here means that story
// extends this fixture instead of forking it.
const (
	authedPrimaryToken     = "integration-primary-token"
	authedPrimaryPrincipal = "integration-primary"

	authedOtherToken     = "integration-other-token"
	authedOtherPrincipal = "integration-other"
)

// integrationAuthYAML is the broker `auth:` block the fixture loads. Static
// tokens are used on purpose: they are the one validator type that needs no
// network and no clock, which is what keeps this harness key-free and offline.
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

	// Token is the primary caller's bearer token and Principal the identity it
	// resolves to.
	Token     string
	Principal string

	// OtherToken is a second VALID credential resolving to OtherPrincipal, for
	// tests about one principal reaching another's resources.
	OtherToken     string
	OtherPrincipal string

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
		Base:           base,
		Registry:       reg,
		Token:          authedPrimaryToken,
		Principal:      authedPrimaryPrincipal,
		OtherToken:     authedOtherToken,
		OtherPrincipal: authedOtherPrincipal,
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

// SpawnCount is how many instance spawns the broker has attempted. Zero after a
// refused request is the proof that the refusal preceded the spawn.
func (b *authedStubBroker) SpawnCount() int64 { return b.spawns.count() }

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
	}
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
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Bind a real listener first so we know the broker's address before wiring
	// the claim handler (it needs it to build the instance dial-back URL).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	wiring := brokerWiring{
		cfg:    Config{ListenAddr: ln.Addr().String(), NexusBinaryPath: stubBin, ReleaseGrace: defaultReleaseGrace},
		runner: execRunner{},
	}
	for _, opt := range opts {
		opt(&wiring)
	}
	cfg := wiring.cfg
	registry := NewRegistry(logger, cfg.MaxConcurrent)
	gateway := NewGateway(logger, registry)
	claims := NewClaimServer(logger, registry, cfg, wiring.runner)
	claims.readyTimeout = 15 * time.Second
	releases := NewReleaseServer(logger, registry, cfg.ReleaseGrace)
	leases := NewLeasesServer(logger, registry)

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
	guarded := newAuthGuard(logger, cfg.AuthChain).Guard(mux)
	claims.Register(guarded)
	releases.Register(guarded)
	leases.Register(guarded)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	// Arm the idle sweeper exactly as main.go does when an idle timeout is set.
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	sweeper := newIdleSweeper(logger, registry, cfg.IdleTimeout, cfg.ReleaseGrace)
	go sweeper.Run(sweepCtx)

	t.Cleanup(func() {
		stopSweep()
		_ = srv.Close()
		gateway.Shutdown()
	})
	return ln.Addr().String(), registry
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
