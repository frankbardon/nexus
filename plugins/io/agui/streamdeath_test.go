package agui

import (
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// captureCancelRequests records every cancel.request that reaches the bus so a
// test can assert both its presence and its contents.
func captureCancelRequests(h *contract.ContractHarness) func() []events.CancelRequest {
	var mu sync.Mutex
	var seen []events.CancelRequest
	h.Bus().Subscribe("cancel.request", func(e engine.Event[any]) {
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
