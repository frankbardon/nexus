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

// stubBrokerOption tweaks the broker wiring used by startStubBrokerWithRegistry.
type stubBrokerOption func(*Config)

// withReleaseGrace overrides the release grace period for a stub broker.
func withReleaseGrace(d time.Duration) stubBrokerOption {
	return func(c *Config) { c.ReleaseGrace = d }
}

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
	registry := NewRegistry(logger)
	gateway := NewGateway(logger, registry)

	// Bind a real listener first so we know the broker's address before wiring
	// the claim handler (it needs it to build the instance dial-back URL).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := Config{ListenAddr: ln.Addr().String(), NexusBinaryPath: stubBin, ReleaseGrace: defaultReleaseGrace}
	for _, opt := range opts {
		opt(&cfg)
	}
	claims := NewClaimServer(logger, registry, cfg, execRunner{})
	claims.readyTimeout = 15 * time.Second
	releases := NewReleaseServer(logger, registry, cfg.ReleaseGrace)

	mux := http.NewServeMux()
	gateway.Register(mux)
	claims.Register(mux)
	releases.Register(mux)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
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
