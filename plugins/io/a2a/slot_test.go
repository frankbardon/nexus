package a2a

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file pins the lifetime of the plugin's single in-flight task slot: when
// it is returned relative to the response a client reads, and which refusal a
// client is told while it is genuinely held.
//
// Both were once implied rather than stated. The slot was released AFTER the
// terminal frame had already been queued for the HTTP goroutine, so a client
// doing the obvious thing — send, await COMPLETED, send again on the same
// contextId — could be refused TASK_ALREADY_IN_FLIGHT against the slot of the
// turn it had just watched finish (issue #153). And the refusals were checked
// slot-before-context, so a client naming a context this process does not serve
// was told a task was in flight, which is the wrong reason and the wrong shape:
// transient-sounding advice for a permanent mistake.
//
// Neither defect was a data race — every p.active access is under p.mu — so
// -race did not report them; it only widened the window enough to make the
// first one fail in CI while passing 40/40 on an idle machine. That is why the
// tests below do NOT lean on timing to produce the bug. Each one holds the slot
// deterministically, so it fails on the ordering itself rather than on how
// loaded the machine happens to be. A gate that needs CPU contention to notice
// a regression is the thing that let this read as flakiness for months.

// slotProbeWindow is how long a test waits before concluding that a response
// the guarantee requires to be WITHHELD has genuinely not been written.
//
// It bounds an absence, so it can only ever be a wait. It is generous because a
// false FAILURE is impossible in the direction that matters: with the release
// ordering correct the response cannot be written at all while this test holds
// the slot open, so the window is dead time rather than a coin flip. Sized for
// the opposite reading — a broken ordering writes the response within
// microseconds of the terminal frame, so anything above a millisecond or two
// catches it even under heavy load.
const slotProbeWindow = 250 * time.Millisecond

// ---- The sequential-send contract ----

// TestSendAfterATerminalResponseIsAccepted states the guarantee directly: a
// client that has read a terminal task may immediately send again on the same
// contextId. There is nothing for it to wait for and nothing for it to retry,
// because the slot is back before the response it just read was written.
//
// This is the contract TestContextIDBindsToTheSession depends on and was losing
// a real race against. It is named here so a regression reports the guarantee
// it broke rather than a context-binding assertion three lines further down.
func TestSendAfterATerminalResponseIsAccepted(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, scriptedTurn("ok"))

	// Repeated, because the guarantee is about every turn boundary and not just
	// the first: each iteration's send is issued the instant the previous one's
	// terminal response was read.
	for i := 0; i < 5; i++ {
		resp := jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("turn", "ctx-sequential"))
		if resp.Error != nil {
			t.Fatalf("send %d after a terminal response was refused: %+v", i+1, resp.Error)
		}
		var result a2a.SendMessageResponse
		if err := resp.DecodeResult(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if result.Task == nil || result.Task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("send %d returned %+v, want a COMPLETED task", i+1, result.Task)
		}
		// The mechanism behind the guarantee, asserted where a regression would
		// show first: reading a terminal task means the slot that task held is
		// already back, so the next send has nothing to collide with.
		if r := p.currentRun(); r != nil {
			t.Fatalf("send %d read a %s task while task %q still held the slot",
				i+1, result.Task.Status.State, r.taskID)
		}
	}
}

// TestSendAfterAClosedStreamIsAccepted is the same contract on the streaming
// path, where the response ends when pumpStream returns on the specification
// section 11.7 stream close rather than on a written result.
func TestSendAfterAClosedStreamIsAccepted(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, scriptedTurn("ok"))

	for i := 0; i < 5; i++ {
		rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
			jsonrpcBody(t, a2a.MethodSendStreamingMessage, sendMessageParams("turn", "ctx-sequential-stream")))
		if rec.Code != http.StatusOK {
			t.Fatalf("stream %d: status = %d: %s", i+1, rec.Code, rec.Body)
		}
		// A refusal is answered before any stream opens, so it arrives as an
		// ordinary JSON-RPC error rather than as SSE. Naming it that way makes a
		// regression say TASK_ALREADY_IN_FLIGHT instead of "malformed SSE".
		if ct := rec.Header().Get("Content-Type"); ct != a2a.ContentTypeSSE {
			resp := rpcResponse(t, rec.Body.Bytes())
			t.Fatalf("stream %d after a closed stream was refused: %+v", i+1, resp.Error)
		}
		fs := frames(t, rec.Body.Bytes())
		if got := states(fs); len(got) == 0 || got[len(got)-1] != a2a.TaskStateCompleted {
			t.Fatalf("stream %d states = %v, want a COMPLETED terminal", i+1, got)
		}
		if r := p.currentRun(); r != nil {
			t.Fatalf("stream %d closed on a terminal frame while task %q still held the slot", i+1, r.taskID)
		}
	}
}

// ---- The ordering that makes the contract hold ----

// TestATerminalResponseIsWithheldUntilTheSlotIsBack is the deterministic gate
// under the two tests above. They assert the guarantee; this asserts the
// mechanism, without depending on which goroutine wins a race.
//
// The slot is pinned open by holding p.mu, which is the only thing endTurn needs,
// and the task is then terminated directly rather than through the bus — a bus
// handler resolves the active run under the same mutex, so driving termination
// through it would block before the terminal sequence started and prove nothing.
// With the release wedged, a response handler that reports the ending anyway is
// reporting a task whose slot is still out, which is precisely the defect.
func TestATerminalResponseIsWithheldUntilTheSlotIsBack(t *testing.T) {
	for name, method := range map[string]string{
		"blocking":  a2a.MethodSendMessage,
		"streaming": a2a.MethodSendStreamingMessage,
	} {
		t.Run(name, func(t *testing.T) {
			// No agent is wired: nothing on the bus ends this turn, so the only
			// thing that terminates the task is this test, at the moment it
			// chooses.
			p, _ := newTestPlugin(t, nil)

			body := jsonrpcBody(t, method, sendMessageParams("go", "ctx-withheld"))
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				answered <- do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), body)
			}()

			r := awaitActiveRun(t, p)

			// endTurn cannot complete until this unlocks, so the terminal
			// sequence is held between "the terminal frame is queued" and "the
			// slot is returned" — the exact window the client used to be
			// answered from.
			p.mu.Lock()
			go r.complete()

			select {
			case <-answered:
				p.mu.Unlock()
				t.Fatal("the response ended while the task's slot was still held: a client acting on it would be refused TASK_ALREADY_IN_FLIGHT by the turn it just watched finish")
			case <-time.After(slotProbeWindow):
			}
			p.mu.Unlock()

			// Withheld, not lost: the client still gets its answer once the slot
			// is genuinely back.
			rec := <-answered
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			if r := p.currentRun(); r != nil {
				t.Fatalf("the slot was never returned; task %q still holds it", r.taskID)
			}
		})
	}
}

// TestATerminalRunReleasesItsSlotBeforeItReportsSettled pins the ordering inside
// the terminal sequence itself, with no goroutines and nothing to time.
//
// run.done is what a response handler waits on before it reports an ending, so
// it has to carry one meaning: this task is finished AND its slot is released. A
// close that happens before the release hands that reader a channel which says
// only "finished", which is the weaker statement the defect was built on.
func TestATerminalRunReleasesItsSlotBeforeItReportsSettled(t *testing.T) {
	p, _ := newTestPlugin(t, nil)

	var r *run
	released := false
	settledAtRelease := false
	r = newRun(runConfig{
		taskID:    "task-ordering",
		contextID: "ctx-ordering",
		logger:    discardLogger(),
		artifacts: p.cfg.artifacts,
		// This is the callback that returns the plugin's slot. What it observes
		// about its own run is the whole assertion.
		onTerminal: func() {
			released = true
			settledAtRelease = r.terminated()
		},
	})
	r.onOutput(events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion, Content: "the answer", Role: "assistant",
	})

	r.complete()

	if !released {
		t.Fatal("a terminal run never released its slot")
	}
	if settledAtRelease {
		t.Fatal("the run reported itself settled BEFORE it released its slot: every reader of run.done, including blockOnTask and pumpStream, would then answer a client while the slot was still out")
	}
	// The other half of the same meaning: the wait does end.
	r.awaitSettled(t.Context())
}

// ---- Which refusal a genuinely concurrent send is told ----

// TestARefusalDuringAnInFlightTaskNamesTheClientsMistake pins that the two
// refusals stay distinguishable while a slot is genuinely held, which is the
// only condition under which they compete.
//
// Both are UnsupportedOperationError -> FAILED_PRECONDITION, so the JSON-RPC
// code cannot tell them apart; the machine-readable detail token can, and it is
// what a client would branch on. The distinction is worth a test because the
// two refusals ask for opposite behaviour: TASK_ALREADY_IN_FLIGHT means come
// back when this turn is over, while CONTEXT_NOT_SERVED means no amount of
// waiting will help — this process serves one conversation for the life of its
// session and the caller has to dial a different instance.
func TestARefusalDuringAnInFlightTaskNamesTheClientsMistake(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	// The turn is driven to WORKING and then held there while the refusals are
	// collected, so the slot is genuinely out for all three of them. Bus
	// dispatch is synchronous, so this is deterministic: the run cannot
	// terminate until this handler emits the turn end.
	var sameContext, foreignContext, noContext a2a.Response
	playAgent(t, bus, func(bus engine.EventBus, in events.UserInput) {
		_ = bus.Emit("agent.turn.start", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "t1",
		})
		sameContext = jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("same", "ctx-a"))
		foreignContext = jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("foreign", "ctx-b"))
		noContext = jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("unnamed", ""))
		_ = bus.Emit("agent.turn.end", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "t1",
		})
	})

	first := jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("first", "ctx-a"))
	if first.Error != nil {
		t.Fatalf("the first turn was refused: %+v", first.Error)
	}

	// The client's context is right and its timing is wrong: the refusal must say
	// so, because waiting is what fixes it.
	if got := refusalDetail(t, sameContext.Error); got != reasonConcurrentTask {
		t.Errorf("a concurrent send on the bound context was refused %s, want %s", got, reasonConcurrentTask)
	}
	// A client that named no context is asking for whatever this process serves,
	// so its mistake is also timing.
	if got := refusalDetail(t, noContext.Error); got != reasonConcurrentTask {
		t.Errorf("a concurrent send naming no context was refused %s, want %s", got, reasonConcurrentTask)
	}
	// The client's context is wrong, and it being wrong does not depend on
	// anything being in flight. Reporting the in-flight refusal here would invite
	// a retry that can never succeed.
	if got := refusalDetail(t, foreignContext.Error); got != reasonForeignContext {
		t.Fatalf("a foreign context was refused %s while a task was in flight, want %s: %s",
			got, reasonForeignContext, foreignContext.Error.Message)
	}
	// And it names the context this process does serve, which is the one piece of
	// information the caller cannot work out for itself.
	if got := refusalMetadata(t, foreignContext.Error)["contextId"]; got != "ctx-a" {
		t.Errorf("the foreign-context refusal names contextId %v, want the bound context", got)
	}

	// A refused request leaves nothing behind: the context it named is not this
	// process's context, so the conversation is still the one the accepted turn
	// claimed.
	p.mu.Lock()
	bound := p.contextID
	p.mu.Unlock()
	if bound != "ctx-a" {
		t.Errorf("this listener is bound to %q; a refused request captured the context on its way out", bound)
	}
}

// ---- helpers ----

// awaitActiveRun blocks until a task is in flight and returns its run. startTurn
// emits its io.input from a goroutine, so a request being served is not
// observable synchronously from the test goroutine.
func awaitActiveRun(t *testing.T, p *Plugin) *run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r := p.currentRun(); r != nil {
			return r
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no task ever went in flight")
	return nil
}

// refusalMetadata returns the ErrorInfo metadata an A2A refusal carries.
func refusalMetadata(t *testing.T, err *a2a.RPCError) map[string]any {
	t.Helper()
	if err == nil {
		t.Fatal("the request was accepted; want a refusal")
	}
	for _, detail := range err.Data {
		if md, ok := detail["metadata"].(map[string]any); ok {
			return md
		}
	}
	t.Fatalf("the refusal carries no ErrorInfo metadata: %+v", err)
	return nil
}

// refusalDetail returns the machine-readable reason token a refusal carries.
// Two refusals sharing one JSON-RPC code are only distinguishable by it, which
// is exactly why a client would read it.
func refusalDetail(t *testing.T, err *a2a.RPCError) string {
	t.Helper()
	token, ok := refusalMetadata(t, err)["detail"].(string)
	if !ok {
		t.Fatalf("the refusal carries no detail token: %+v", err)
	}
	return token
}
