package agui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// This file is E1-S2: the lifetime matrix for the session identity AG-UI binds
// (_principal_id plus the _header.* namespace). One rule is under test
// throughout:
//
//	Identity is bound per run and released by the agent.turn.end of the TURN it
//	was bound for. A HITL park, a client disconnect or an early handler return
//	does not release it; Shutdown is the only backstop.
//
// Every case here is one where the HTTP request's lifetime and the turn's
// diverge — which is the whole bug: endRun used to clear identity, and endRun
// is deferred on the handler. contract_test.go covers the bus-level pairs
// (turn-end-while-draining, the late-turn race); this file covers the paths
// that need a real socket to reach, plus the Shutdown backstop.
//
// Note that every test emits agent.turn.start before agent.turn.end. That is
// not decoration: identityTurn is stamped at agent.turn.start, so a fake agent
// that skips it would never clear identity at all and each "still bound"
// assertion would pass for the wrong reason. react, planexec and orchestrator
// all emit the pair, so the fakes match the real emitters.

// assertIdentityBound asserts _principal_id and the _header.* namespace both
// hold exactly what a given run bound. They are asserted together everywhere
// in this file so a future change cannot split the two lifetimes apart again:
// they describe one caller, and a half-cleared identity is worse than either
// whole state.
func assertIdentityBound(t *testing.T, session *engine.SessionWorkspace, when, wantPrincipal string, wantHeaders map[string]string) {
	t.Helper()
	got, ok := session.PrincipalID()
	if !ok || got != wantPrincipal {
		t.Errorf("%s: _principal_id = %q (present=%v), want %q", when, got, ok, wantPrincipal)
	}
	headers, err := session.RequestHeaders()
	if err != nil {
		t.Fatalf("%s: RequestHeaders: %v", when, err)
	}
	for k, want := range wantHeaders {
		if headers[k] != want {
			t.Errorf("%s: _header.%s = %q, want %q (all: %v)", when, k, headers[k], want, headers)
		}
	}
	if len(headers) != len(wantHeaders) {
		t.Errorf("%s: request headers = %v, want exactly %v", when, headers, wantHeaders)
	}
}

// assertIdentityCleared is assertIdentityBound's other half: both labels gone,
// together.
func assertIdentityCleared(t *testing.T, session *engine.SessionWorkspace, when string) {
	t.Helper()
	if got, ok := session.PrincipalID(); ok {
		t.Errorf("%s: _principal_id still bound as %q, want cleared", when, got)
	}
	headers, err := session.RequestHeaders()
	if err != nil {
		t.Fatalf("%s: RequestHeaders: %v", when, err)
	}
	if len(headers) != 0 {
		t.Errorf("%s: request headers still bound: %v, want cleared", when, headers)
	}
}

// identityCleared is the waitFor predicate matching assertIdentityCleared: the
// turn-end clear runs on whatever goroutine emitted agent.turn.end, so a test
// that emits it from a fake-agent goroutine polls rather than assumes.
func identityCleared(session *engine.SessionWorkspace) func() bool {
	return func() bool {
		if _, ok := session.PrincipalID(); ok {
			return false
		}
		headers, err := session.RequestHeaders()
		return err == nil && len(headers) == 0
	}
}

// TestE2E_ClientDisconnectMidTurnKeepsIdentityUntilTurnEnd is the reported
// bug's own repro, and the case no previous test could reach: the client
// cancels the request while the agent is still working. The HTTP handler
// returns, its deferred endRun frees the run slot — and the turn, which runs
// detached on engine goroutines, keeps going under an identity that must still
// be there. Before the fix the disconnect de-authenticated it.
//
// Both halves are asserted on the same timeline: identity survives the
// disconnect, and goes ONLY when the turn that owns it finally ends.
func TestE2E_ClientDisconnectMidTurnKeepsIdentityUntilTurnEnd(t *testing.T) {
	p, bus, session, url := newSessionTestPlugin(t, staticAuthConfig(map[string]string{
		"tok-disco": "principal-disco",
	}))

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	// working closes once the fake agent is demonstrably mid-turn; release lets
	// the test decide when the turn ends, long after the client is gone.
	working := make(chan struct{})
	release := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-disconnect"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion, Content: "working", Role: "assistant",
		})
		close(working)
		<-release
		_ = bus.Emit("agent.turn.end", turn)
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := `{"threadId":"thread-disco","runId":"run-disco","messages":[{"id":"m1","role":"user","content":"long job"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tok-disco")
	req.Header.Set("X-Nexus-Tenant-Id", "acme")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Read one event so the stream is provably live before we kill it —
	// otherwise a disconnect could race the run registration itself and this
	// would be testing the rejected-run path instead.
	if ev, err := agui.NewSSEReader(resp.Body).Next(); err != nil {
		resp.Body.Close()
		t.Fatalf("read first sse event: %v", err)
	} else if ev.EventType() != agui.EventRunStarted {
		resp.Body.Close()
		t.Fatalf("first event = %s, want RunStarted", ev.EventType())
	}
	<-working

	// The client vanishes mid-turn. server.go's watcher fails the run, the
	// drain loop returns, and the deferred endRun frees the slot.
	cancel()
	resp.Body.Close()
	waitFor(t, func() bool { return p.currentRun() == nil })
	if p.currentRun() != nil {
		t.Fatal("run slot never freed after the client disconnected")
	}

	// The turn is still running. Its identity must still be bound — this is
	// the assertion that fails when the clear rides endRun.
	assertIdentityBound(t, session, "across the client disconnect", "principal-disco",
		map[string]string{"tenant-id": "acme"})

	// ...and only now, when the work itself ends, does it go.
	close(release)
	<-ended
	waitFor(t, identityCleared(session))
	assertIdentityCleared(t, session, "after the disconnected turn ended")
}

// TestE2E_HITLParkKeepsHeadersWithIdentity is the park half of the same rule,
// and the one that needs no error path at all to reach: handleHITLRequested
// ends the SSE stream and calls endRun with the agent still blocked in-process
// on the human's answer. It is the designed happy path, which is why it is the
// most likely to regress silently.
//
// TestE2E_ResumeRebindsToNewPrincipalNotOriginal already covers the principal
// across a park and the resume's rebind. This one covers the _header.*
// namespace on exactly that timeline — park, rebind, clear — so the two
// reserved namespaces cannot drift back onto different lifetimes.
func TestE2E_HITLParkKeepsHeadersWithIdentity(t *testing.T) {
	p, bus, session, url := newSessionTestPlugin(t, staticAuthConfig(map[string]string{
		"tok-park": "principal-park",
	}))

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})
	responded := make(chan events.HITLResponse, 1)
	bus.Subscribe("hitl.responded", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.HITLResponse); ok {
			select {
			case responded <- r:
			default:
			}
		}
	})

	// boundDuringResume snapshots the labels the instant hitl.responded
	// unblocks the parked agent: resumeRun binds synchronously BEFORE emitting
	// it, and nothing clears until agent.turn.end below, so the read is
	// guaranteed to observe the continuation's own bind.
	type snapshot struct {
		principal string
		headers   map[string]string
	}
	boundDuringResume := make(chan snapshot, 1)
	ended := make(chan struct{})

	go func() {
		defer close(ended)
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-park"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("hitl.requested", events.HITLRequest{
			SchemaVersion: events.HITLRequestVersion,
			ID:            "req-park",
			SessionID:     "thread-park",
			TurnID:        "turn-park",
			Mode:          events.HITLModeChoices,
			Prompt:        "Approve deploy?",
			Choices:       []events.HITLChoice{{ID: "allow", Label: "Allow", Kind: events.ChoiceAllow}},
		})
		<-responded
		snap := snapshot{}
		if id, ok := session.PrincipalID(); ok {
			snap.principal = id
		}
		if h, err := session.RequestHeaders(); err == nil {
			snap.headers = h
		}
		boundDuringResume <- snap
		_ = bus.Emit("agent.turn.end", turn)
	}()

	// --- Run 1: POST, park at the HITL question. ---
	body1 := `{"threadId":"thread-park","runId":"run-1","messages":[{"id":"m1","role":"user","content":"deploy"}]}`
	resp1 := postWithHeaders(t, url, body1, map[string]string{
		"Authorization":     "Bearer tok-park",
		"X-Nexus-Tenant-Id": "acme",
	})
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("run1 status = %d, want 200", resp1.StatusCode)
	}
	interruptID := readInterruptID(t, resp1)
	waitFor(t, func() bool { return p.currentRun() == nil })

	// The stream is over and the slot is freed, but the agent is parked
	// in-process: identity and headers alike must survive the whole park.
	assertIdentityBound(t, session, "across the hitl park", "principal-park",
		map[string]string{"tenant-id": "acme"})

	// --- Run 2: the continuation, carrying a DIFFERENT header value. ---
	body2 := `{"threadId":"thread-park","runId":"run-2","resume":[{"interruptId":"` + interruptID +
		`","status":"resolved","payload":{"choiceId":"allow"}}]}`
	resp2 := postWithHeaders(t, url, body2, map[string]string{
		"Authorization":     "Bearer tok-park",
		"X-Nexus-Tenant-Id": "globex",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("run2 status = %d, want 200", resp2.StatusCode)
	}
	if _, err := agui.NewSSEReader(resp2.Body).ReadAll(); err != nil {
		t.Fatalf("read run2 sse: %v", err)
	}

	select {
	case snap := <-boundDuringResume:
		if snap.principal != "principal-park" {
			t.Errorf("_principal_id while the resumed turn ran = %q, want principal-park", snap.principal)
		}
		if snap.headers["tenant-id"] != "globex" {
			t.Errorf("_header.tenant-id while the resumed turn ran = %q, want globex (the continuation's own header, not run 1's acme): %v",
				snap.headers["tenant-id"], snap.headers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the continuation's bound-identity snapshot")
	}

	// The continuation adopted the parked turn, so that turn's end releases
	// the resume's bind — both labels, together.
	<-ended
	waitFor(t, identityCleared(session))
	assertIdentityCleared(t, session, "after the resumed turn ended")
}

// readInterruptID reads an SSE stream expected to terminate in a RunFinished
// carrying an interrupt outcome, and returns its interruptId.
func readInterruptID(t *testing.T, resp *http.Response) string {
	t.Helper()
	evs, err := agui.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("no sse events")
	}
	fin, ok := evs[len(evs)-1].(*agui.RunFinishedEvent)
	if !ok {
		t.Fatalf("last event = %v, want RunFinished; all=%v", evs[len(evs)-1].EventType(), eventTypes(evs))
	}
	if fin.Outcome != agui.OutcomeInterrupt {
		t.Fatalf("outcome = %q, want interrupt", fin.Outcome)
	}
	var ip agui.Interrupt
	if err := json.Unmarshal(fin.Result, &ip); err != nil {
		t.Fatalf("decode interrupt payload: %v", err)
	}
	if ip.InterruptID == "" {
		t.Fatal("interrupt payload carried no interruptId")
	}
	return ip.InterruptID
}

// TestContract_ShutdownClearsIdentityOfTurnThatNeverEnds pins the backstop,
// and pins that it is the ONLY one. A turn can fail to emit agent.turn.end —
// a wedged provider, a killed sub-process — and nothing else in the plugin
// releases the bind, by design: any TTL or lease here would re-create the
// original bug for a legitimately long turn. So Shutdown must clear it
// unconditionally, without caring whether a run or a turn is outstanding.
func TestContract_ShutdownClearsIdentityOfTurnThatNeverEnds(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	run, started := p.startRun(runInput{
		threadID:    "thread-wedged",
		runID:       "run-wedged",
		principalID: "principal-wedged",
		headers:     map[string]string{"tenant-id": "acme"},
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	emitTurn(t, h, "agent.turn.start", "turn-wedged")
	// The handler returns; the turn never does.
	run.finish()
	p.endRun(run)

	assertIdentityBound(t, p.session, "with the turn still outstanding", "principal-wedged",
		map[string]string{"tenant-id": "acme"})

	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	assertIdentityCleared(t, p.session, "after Shutdown")
}

// TestContract_NewRunNeverInheritsAPriorRunsIdentity covers the other
// direction of the same rule: the bind is unconditional, so a run that
// resolves no principal and carries no headers CLEARS the labels rather than
// reading whatever the last caller left behind. Both orderings matter — the
// prior turn may have ended cleanly, or (the case with teeth) may still be
// running when the next request arrives.
func TestContract_NewRunNeverInheritsAPriorRunsIdentity(t *testing.T) {
	tests := []struct {
		name         string
		endFirstTurn bool
	}{
		{name: "first turn completed", endFirstTurn: true},
		{name: "first turn still running", endFirstTurn: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
				"bind": freeAddr(t),
			}), contract.WithSession())

			p, ok := h.Plugin().(*Plugin)
			if !ok {
				t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
			}

			first, started := p.startRun(runInput{
				threadID:    "thread-inherit",
				runID:       "run-auth",
				principalID: "principal-first",
				headers:     map[string]string{"tenant-id": "acme"},
			})
			if !started {
				t.Fatal("startRun rejected on a fresh plugin")
			}
			emitTurn(t, h, "agent.turn.start", "turn-first")
			first.finish()
			p.endRun(first)
			if tc.endFirstTurn {
				emitTurn(t, h, "agent.turn.end", "turn-first")
			} else {
				assertIdentityBound(t, p.session, "before the second run", "principal-first",
					map[string]string{"tenant-id": "acme"})
			}

			// A second run resolving no principal and carrying no headers.
			second, started := p.startRun(runInput{
				threadID: "thread-inherit",
				runID:    "run-anon",
			})
			if !started {
				t.Fatal("startRun rejected after the first run was released")
			}
			t.Cleanup(func() {
				second.finish()
				p.endRun(second)
			})

			assertIdentityCleared(t, p.session, "after an unauthenticated run bound")
		})
	}
}
