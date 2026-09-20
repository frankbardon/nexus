package agui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// captureCancelRequests records every cancel.request that reaches the harness
// bus so a test can assert both its presence and its contents.
func captureCancelRequests(h *contract.ContractHarness) func() []events.CancelRequest {
	return captureCancelsOnBus(h.Bus())
}

// captureCancelsOnBus is captureCancelRequests for the socket-level tests,
// which run against a bare engine bus rather than the contract harness.
func captureCancelsOnBus(bus engine.EventBus) func() []events.CancelRequest {
	var mu sync.Mutex
	var seen []events.CancelRequest
	bus.Subscribe("cancel.request", func(e engine.Event[any]) {
		if c, ok := e.Payload.(events.CancelRequest); ok {
			mu.Lock()
			seen = append(seen, c)
			mu.Unlock()
		}
	})
	return func() []events.CancelRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]events.CancelRequest, len(seen))
		copy(out, seen)
		return out
	}
}

// failAsHandlerExit reproduces what server.go does on every handler exit: the
// request context is cancelled, the watcher fails the run, and only a fail that
// actually terminated the run is treated as stream death. Driving the same two
// steps here keeps the test honest about the gate rather than calling
// streamDied unconditionally.
func failAsHandlerExit(p *Plugin, r *run) bool {
	closed := r.fail("client disconnected")
	if closed {
		p.streamDied(r)
	}
	return closed
}

// TestContract_StreamDeathCancelsLiveTurn is the E2-S1 acceptance test: a
// client that goes away while its turn is still running has that turn cancelled
// through the control.cancel capability, rather than leaving it to run to
// completion for nobody.
func TestContract_StreamDeathCancelsLiveTurn(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}))

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}
	cancels := captureCancelRequests(h)

	run, started := p.startRun(runInput{
		threadID: "thread-disconnect",
		runID:    "run-disconnect",
		messages: []agui.Message{{ID: "m1", Role: "user", Content: "work"}},
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	t.Cleanup(func() { p.endRun(run) })

	// The agent is provably mid-turn: a turn started and never ended.
	emitTurn(t, h, "agent.turn.start", "turn-disconnect")

	if !failAsHandlerExit(p, run) {
		t.Fatal("fail did not terminate a live run; the disconnect gate never opened")
	}

	got := cancels()
	if len(got) != 1 {
		t.Fatalf("cancel.request emissions = %d, want 1: %+v", len(got), got)
	}
	if got[0].TurnID != "turn-disconnect" {
		t.Errorf("cancel.request TurnID = %q, want turn-disconnect", got[0].TurnID)
	}
	if got[0].Source != "agui" {
		t.Errorf("cancel.request Source = %q, want agui", got[0].Source)
	}
	if got[0].SchemaVersion != events.CancelRequestVersion {
		t.Errorf("cancel.request SchemaVersion = %d, want %d",
			got[0].SchemaVersion, events.CancelRequestVersion)
	}

	// The emission has to be declared, or the contract harness is entitled to
	// fail the suite for it.
	h.AssertNoUndeclaredEmissions()
}

// TestContract_StreamDeathDoesNotCancelWhatItShouldNotCancel covers the three
// exits that end an SSE stream WITHOUT orphaning a live turn. The HITL park is
// the trap: it ends the stream, so the handler returns and the disconnect
// watcher fires exactly as a real disconnect does. Wired without the guards,
// every parked turn would cancel itself.
//
// E2-S2 owns the full cancel matrix (client-tool suspend, the real HTTP
// round-trip, the resumed-turn case); these are the minimum needed to show the
// discrimination works at all.
func TestContract_StreamDeathDoesNotCancelWhatItShouldNotCancel(t *testing.T) {
	tests := []struct {
		name string
		// arrange runs after the run is registered and returns whether a
		// fail-on-handler-exit is still expected to terminate the run.
		arrange       func(t *testing.T, h *contract.ContractHarness, p *Plugin, r *run)
		wantFailClose bool
		// forceStreamDied additionally calls streamDied outright, to show the
		// case is suppressed by a guard INSIDE it (a deliberate suspension, or
		// no turn to cancel) and not merely by the fail gate outside it — which
		// is what a disconnect winning the race against a park would bypass. A
		// completed turn is deliberately not forced: the one-shot close IS its
		// only discriminator, since a finished turn's id is still stamped on
		// the run.
		forceStreamDied bool
	}{
		{
			name: "parked for hitl",
			arrange: func(t *testing.T, h *contract.ContractHarness, p *Plugin, r *run) {
				emitTurn(t, h, "agent.turn.start", "turn-parked")
				h.Inject("hitl.requested", events.HITLRequest{
					SchemaVersion: events.HITLRequestVersion,
					ID:            "req-park",
					TurnID:        "turn-parked",
					Prompt:        "which one?",
				})
				if !r.isSuspended() {
					t.Fatal("run not marked suspended after a hitl park")
				}
			},
			// interrupt already closed the run, so the watcher's fail is a
			// no-op racing in behind it.
			wantFailClose:   false,
			forceStreamDied: true,
		},
		{
			name: "no turn ever started",
			arrange: func(*testing.T, *contract.ContractHarness, *Plugin, *run) {
			},
			wantFailClose:   true,
			forceStreamDied: true,
		},
		{
			name: "turn completed normally",
			arrange: func(t *testing.T, h *contract.ContractHarness, p *Plugin, r *run) {
				emitTurn(t, h, "agent.turn.start", "turn-done")
				emitTurn(t, h, "agent.turn.end", "turn-done")
			},
			// agent.turn.end finished the run and its SSE stream.
			wantFailClose: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
				"bind": freeAddr(t),
			}))
			p, ok := h.Plugin().(*Plugin)
			if !ok {
				t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
			}
			cancels := captureCancelRequests(h)

			run, started := p.startRun(runInput{
				threadID: "thread-quiet",
				runID:    "run-quiet",
				messages: []agui.Message{{ID: "m1", Role: "user", Content: "work"}},
			})
			if !started {
				t.Fatal("startRun rejected on a fresh plugin")
			}
			t.Cleanup(func() { p.endRun(run) })

			tc.arrange(t, h, p, run)

			if got := failAsHandlerExit(p, run); got != tc.wantFailClose {
				t.Errorf("fail-on-handler-exit terminated the run = %v, want %v", got, tc.wantFailClose)
			}
			if tc.forceStreamDied {
				// Even if the disconnect HAD won the race to terminate the
				// run, the guards inside streamDied must still hold.
				p.streamDied(run)
			}

			if got := cancels(); len(got) != 0 {
				t.Errorf("cancel.request emitted %d time(s) on a stream that orphaned nothing: %+v",
					len(got), got)
			}
		})
	}
}

// awaitCancel blocks until at least one cancel.request has been captured and
// returns the first one. The emissions this file asserts on cross a goroutine
// handoff (the server's disconnect watcher, or a fake agent), so the POSITIVE
// assertion polls rather than reading the slice once and hoping.
func awaitCancel(t *testing.T, get func() []events.CancelRequest) events.CancelRequest {
	t.Helper()
	waitFor(t, func() bool { return len(get()) > 0 })
	got := get()
	if len(got) == 0 {
		t.Fatal("no cancel.request was emitted for a stream that died under a live turn")
	}
	return got[0]
}

// assertCancelForTurn asserts the full payload contract of a stream-death
// cancellation: the turn it names, the source it claims, and the schema version
// nexus.control.cancel matches on.
func assertCancelForTurn(t *testing.T, c events.CancelRequest, wantTurn string) {
	t.Helper()
	if c.TurnID != wantTurn {
		t.Errorf("cancel.request TurnID = %q, want %q", c.TurnID, wantTurn)
	}
	if c.Source != "agui" {
		t.Errorf("cancel.request Source = %q, want agui", c.Source)
	}
	if c.SchemaVersion != events.CancelRequestVersion {
		t.Errorf("cancel.request SchemaVersion = %d, want %d", c.SchemaVersion, events.CancelRequestVersion)
	}
}

// TestContract_ClientToolSuspendDoesNotCancelTurn closes the gap E2-S1 declared
// and E1-S2 flagged before it: clienttools.go is the THIRD endRun-adjacent
// caller, and the only suspend path neither story covered.
//
// It is the same trap as the HITL park, reached by a different door. The agent
// calls a frontend tool, suspendForClientTool ends the SSE stream and releases
// the run slot, and the agent stays parked in-process waiting for a tool.result
// that only the client's resume can produce. The handler then returns, the
// request context is cancelled and the disconnect watcher fires — exactly as a
// real disconnect does. Cancelling here would abort a turn that is waiting on
// the client by design, and the client's resume would then have nothing to
// resume into.
//
// The suspend is driven through its real entry point (a tool.invoke on the bus
// naming a tool the run advertised), not by calling suspendForClientTool, so
// handleToolInvoke's client-vs-server-tool discrimination is under test too.
func TestContract_ClientToolSuspendDoesNotCancelTurn(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}))

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}
	cancels := captureCancelRequests(h)

	run, started := p.startRun(runInput{
		threadID: "thread-clienttool",
		runID:    "run-clienttool",
		messages: []agui.Message{{ID: "m1", Role: "user", Content: "weather in Paris?"}},
		tools: []agui.Tool{{
			Name:        "get_weather",
			Description: "Get weather for a city",
		}},
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	t.Cleanup(func() { p.endRun(run) })

	// A turn is genuinely in flight: without this the run would have no turn to
	// name and the case would pass for the wrong reason (the "no turn ever
	// started" guard rather than the suspension guard).
	emitTurn(t, h, "agent.turn.start", "turn-clienttool")
	if run.boundTurn() != "turn-clienttool" {
		t.Fatalf("run turn = %q, want turn-clienttool: there would be nothing to cancel", run.boundTurn())
	}

	// The agent calls the client-executed tool. Dispatch is synchronous, so the
	// suspend has completed by the time Inject returns.
	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-weather",
		Name:          "get_weather",
		Arguments:     map[string]any{"city": "Paris"},
		TurnID:        "turn-clienttool",
	})

	// Prove the clienttools.go suspend is what happened, rather than some other
	// exit that would make the assertions below trivially true.
	if p.currentRun() != nil {
		t.Fatal("client-tool suspend did not release the run slot")
	}
	if !run.isSuspended() {
		t.Fatal("run not marked suspended after a client-tool call")
	}
	p.pendingMu.Lock()
	var parked int
	for _, pi := range p.pending {
		if pi.Kind == interruptClientTool && pi.ToolCallID == "call-weather" {
			parked++
		}
	}
	p.pendingMu.Unlock()
	if parked != 1 {
		t.Fatalf("pending client-tool interrupts for call-weather = %d, want 1", parked)
	}

	// Now the handler exits, exactly as it does after any terminal event.
	if failAsHandlerExit(p, run) {
		t.Error("the handler-exit fail terminated a run the client-tool suspend had already ended")
	}
	// And even if a disconnect HAD won the race to terminate the run, the
	// suspension flag inside streamDied must still hold it off — interrupt()
	// sets it before its one-shot close for exactly this ordering.
	p.streamDied(run)

	// The trigger chain above is synchronous end to end (bus dispatch, the
	// suspend, the forced streamDied), so "nothing captured here" is a real
	// negative rather than "nothing captured yet".
	if got := cancels(); len(got) != 0 {
		t.Errorf("cancel.request emitted %d time(s) for a run parked on a client tool: %+v", len(got), got)
	}
	h.AssertNotEmitted("cancel.request")

	// The emission has to be declared. AssertNoUndeclaredEmissions is the
	// harness-level check; the explicit membership assertion is the one with
	// teeth, since a bus.Emit from a plugin carries no Source for the harness
	// to attribute it by.
	h.AssertNoUndeclaredEmissions()
	if !slices.Contains(p.Emissions(), "cancel.request") {
		t.Errorf("Emissions() = %v, must declare cancel.request", p.Emissions())
	}
}

// TestE2E_ClientDisconnectCancelsTheOrphanedTurn drives the disconnect through
// a real POST on a real loopback listener, with the request context cancelled
// while the agent is provably mid-turn.
//
// TestContract_StreamDeathCancelsLiveTurn asserts the same outcome by calling
// streamDied directly; this one puts server.go's own wiring under test — the
// ctx.Done watcher, its fail gate, and the hand-off to the bridge — which no
// test in the effort otherwise exercises.
func TestE2E_ClientDisconnectCancelsTheOrphanedTurn(t *testing.T) {
	p, bus, _, url := newSessionTestPlugin(t, nil)
	cancels := captureCancelsOnBus(bus)

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	// A fake agent that is demonstrably mid-turn when the client vanishes and
	// keeps running afterwards, as a detached engine goroutine really would.
	working := make(chan struct{})
	release := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-live"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion, Content: "working", Role: "assistant",
		})
		close(working)
		// Stand in for the ReAct loop answering cancel.active: the turn ends
		// only once the test says the cancellation has landed.
		<-release
		_ = bus.Emit("agent.turn.end", turn)
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := `{"threadId":"thread-live","runId":"run-live",` +
		`"messages":[{"id":"m1","role":"user","content":"long job"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Read one event so the stream is provably live before killing it;
	// otherwise the disconnect could race the run registration and this would
	// be testing the rejected-run path instead.
	if ev, err := agui.NewSSEReader(resp.Body).Next(); err != nil {
		resp.Body.Close()
		t.Fatalf("read first sse event: %v", err)
	} else if ev.EventType() != agui.EventRunStarted {
		resp.Body.Close()
		t.Fatalf("first event = %s, want RunStarted", ev.EventType())
	}
	<-working

	// The client goes away mid-turn.
	cancel()
	resp.Body.Close()

	// The handler's deferred endRun frees the slot; the watcher it raced is
	// what asks for the cancellation.
	waitFor(t, func() bool { return p.currentRun() == nil })
	if p.currentRun() != nil {
		t.Fatal("run slot never freed after the client disconnected")
	}
	assertCancelForTurn(t, awaitCancel(t, cancels), "turn-live")

	close(release)
	<-ended
}

// TestE2E_DisconnectDuringResumedTurnCancelsTheAdoptedTurn covers the case the
// story's wording does not reach and E2-S1 left untested: a CONTINUATION
// stream dying mid-turn.
//
// A resumed turn emits no fresh agent.turn.start — the agent never left the
// turn, it was parked inside it — so the continuation run would have no turn to
// name and a disconnect on it would silently cancel nothing. resumeRun's
// adoptTurn is what makes the parked turn cancellable from the stream that is
// now carrying it, and this is the only test of that.
func TestE2E_DisconnectDuringResumedTurnCancelsTheAdoptedTurn(t *testing.T) {
	p, bus, _, url := newSessionTestPlugin(t, nil)
	cancels := captureCancelsOnBus(bus)

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

	// A fake agent that parks on a question inside its turn, and keeps working
	// inside the SAME turn once the human answers.
	resumedWorking := make(chan struct{})
	release := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-parked"}
		_ = bus.Emit("agent.turn.start", turn)
		// Parks the run: handleHITLRequested ends the SSE stream with an
		// interrupt outcome and frees the slot; this turn never ends here.
		_ = bus.Emit("hitl.requested", events.HITLRequest{
			SchemaVersion: events.HITLRequestVersion,
			ID:            "req-park",
			SessionID:     "thread-resume",
			TurnID:        "turn-parked",
			Prompt:        "which one?",
		})
		<-responded
		// Unblocked and back at work — still inside turn-parked, with no new
		// agent.turn.start.
		_ = bus.Emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion, Content: "continuing", Role: "assistant",
		})
		close(resumedWorking)
		<-release
		_ = bus.Emit("agent.turn.end", turn)
	}()

	// --- Run 1: park on the question. ---
	body1 := `{"threadId":"thread-resume","runId":"run-1",` +
		`"messages":[{"id":"m1","role":"user","content":"do the thing"}]}`
	resp1 := post(t, url, "", "", body1)
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("run1 status = %d, want 200", resp1.StatusCode)
	}
	evs1, err := agui.NewSSEReader(resp1.Body).ReadAll()
	if err != nil {
		t.Fatalf("read run1 sse: %v", err)
	}
	var fin1 *agui.RunFinishedEvent
	for _, e := range evs1 {
		if fe, ok := e.(*agui.RunFinishedEvent); ok {
			fin1 = fe
		}
	}
	if fin1 == nil || fin1.Outcome != agui.OutcomeInterrupt {
		t.Fatalf("run1 did not park on an interrupt (events=%v)", eventTypes(evs1))
	}
	var ip agui.Interrupt
	if err := json.Unmarshal(fin1.Result, &ip); err != nil {
		t.Fatalf("decode run1 interrupt: %v", err)
	}
	waitFor(t, func() bool { return p.currentRun() == nil })

	// --- Run 2: the continuation, killed mid-turn. ---
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body2 := `{"threadId":"thread-resume","runId":"run-2","resume":[{"interruptId":"` +
		ip.InterruptID + `","status":"resolved","payload":{"freeText":"the first one"}}]}`
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body2))
	if err != nil {
		t.Fatalf("new run2 request: %v", err)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("do run2 request: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		resp2.Body.Close()
		t.Fatalf("run2 status = %d, want 200", resp2.StatusCode)
	}
	if ev, err := agui.NewSSEReader(resp2.Body).Next(); err != nil {
		resp2.Body.Close()
		t.Fatalf("read first run2 sse event: %v", err)
	} else if ev.EventType() != agui.EventRunStarted {
		resp2.Body.Close()
		t.Fatalf("run2 first event = %s, want RunStarted", ev.EventType())
	}
	<-resumedWorking

	// The resuming client vanishes while the adopted turn is still running.
	cancel()
	resp2.Body.Close()

	// The cancellation must name the PARKED turn: it is the only turn there
	// has ever been on this thread, and nothing else could have stamped it on
	// the continuation run.
	assertCancelForTurn(t, awaitCancel(t, cancels), "turn-parked")

	close(release)
	<-ended
}

// deadSocket is an http.ResponseWriter whose writes fail once armed: a socket
// that broke under the handler without the request context ever being
// cancelled. It stands in for the half-open connection that makes sse.Write
// return an error, which is the OTHER stream-death exit in server.go and the
// one a client-initiated disconnect never reaches.
type deadSocket struct {
	mu     sync.Mutex
	header http.Header
	armed  bool
}

func newDeadSocket() *deadSocket { return &deadSocket{header: make(http.Header)} }

func (d *deadSocket) Header() http.Header { return d.header }

func (d *deadSocket) WriteHeader(int) {}

func (d *deadSocket) Write(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.armed {
		return 0, errors.New("write: broken pipe")
	}
	return len(b), nil
}

func (d *deadSocket) arm() {
	d.mu.Lock()
	d.armed = true
	d.mu.Unlock()
}

// TestSSEWriteFailureCancelsLiveTurn covers the second stream-death exit as a
// path in its own right: the socket breaks under a write while the request
// context is still very much alive, so the ctx.Done watcher never runs and the
// sse.Write failure branch is the only thing that can notice.
//
// The handler is driven directly over a ResponseWriter that starts failing on
// command, because a client-side disconnect cannot reach this branch
// deterministically — it would cancel the context and race the watcher.
func TestSSEWriteFailureCancelsLiveTurn(t *testing.T) {
	p, bus, _ := newTestPlugin(t)
	cancels := captureCancelsOnBus(bus)

	// A second Server sharing the plugin as its bridge: only the handler is
	// under test, so nothing is bound.
	s := NewServer(serverConfig{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		bridge: p,
	})

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	body := `{"threadId":"thread-sse","runId":"run-sse",` +
		`"messages":[{"id":"m1","role":"user","content":"long job"}]}`
	req := httptest.NewRequest(http.MethodPost, agentPath, strings.NewReader(body))
	req = req.WithContext(t.Context())
	w := newDeadSocket()

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handleRunAgent(w, req)
	}()

	<-inputSeen
	// Bus dispatch is synchronous, so the turn is stamped on the run by the
	// time Emit returns — the cancellation below has something to name.
	if err := bus.Emit("agent.turn.start", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion, TurnID: "turn-sse",
	}); err != nil {
		t.Fatalf("emit agent.turn.start: %v", err)
	}

	// The socket breaks. Every subsequent write fails, and the output below
	// guarantees there IS a subsequent write.
	w.arm()
	if err := bus.Emit("io.output", events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion, Content: "working", Role: "assistant",
	}); err != nil {
		t.Fatalf("emit io.output: %v", err)
	}

	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the sse writes started failing")
	}

	// The request was never cancelled: this cancellation can only have come
	// from the sse.Write failure branch.
	if err := req.Context().Err(); err != nil {
		t.Fatalf("request context was cancelled (%v); this no longer isolates the sse-write path", err)
	}
	assertCancelForTurn(t, awaitCancel(t, cancels), "turn-sse")
	if got := cancels(); len(got) != 1 {
		t.Errorf("cancel.request emissions = %d, want exactly 1: %+v", len(got), got)
	}
}
