package a2a

import (
	"net/http"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// withExtensions sets the A2A-Extensions service parameter on a request.
func withExtensions(uris ...string) func(*http.Request) {
	return func(r *http.Request) {
		for _, uri := range uris {
			r.Header.Add(a2a.HeaderExtensions, uri)
		}
	}
}

// nexusEvents decodes the Nexus extension payloads carried on a frame sequence.
func nexusEvents(t *testing.T, fs []a2a.StreamResponse) []a2a.NexusEvent {
	t.Helper()
	var out []a2a.NexusEvent
	for _, f := range fs {
		if f.Kind() != a2a.StreamPayloadStatusUpdate {
			continue
		}
		event, present, err := a2a.NexusEventFromMetadata(f.StatusUpdate.Metadata)
		if err != nil {
			t.Fatalf("a status update carries an undecodable extension payload: %v", err)
		}
		if present {
			out = append(out, *event)
		}
	}
	return out
}

// kinds renders the event kinds a payload sequence carries.
func kinds(events []a2a.NexusEvent) []a2a.NexusEventKind {
	out := make([]a2a.NexusEventKind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

// hasKind reports whether a sequence carries an event of the given kind.
func hasKind(events []a2a.NexusEvent, want a2a.NexusEventKind) bool {
	for _, e := range events {
		if e.Kind == want {
			return true
		}
	}
	return false
}

// telemetryTurn plays a turn that produces one of everything the extension
// carries: a reasoning step, a tool call and its result, a delegated run, and
// token accounting.
func telemetryTurn(answer string) func(engine.EventBus, events.UserInput) {
	return func(bus engine.EventBus, in events.UserInput) {
		turnID := "turn-" + in.Content
		_ = bus.Emit("agent.turn.start", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: turnID, SessionID: in.SessionID,
		})
		_ = bus.Emit("thinking.step", events.ThinkingStep{
			SchemaVersion: events.ThinkingStepVersion,
			TurnID:        turnID, Content: "I should search first.", Phase: "reasoning", Index: 1,
		})
		_ = bus.Emit("tool.invoke", events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            "call-1", Name: "web_search", TurnID: turnID,
			Arguments: map[string]any{"query": "nexus"},
		})
		_ = bus.Emit("tool.result", events.ToolResult{
			SchemaVersion: events.ToolResultVersion,
			ID:            "call-1", Name: "web_search", Output: "three results", TurnID: turnID,
		})
		_ = bus.Emit("subagent.started", events.SubagentStarted{
			SchemaVersion: events.SubagentStartedVersion,
			SpawnID:       "spawn-1", Task: "summarize", ParentTurnID: turnID,
		})
		_ = bus.Emit("subagent.iteration", events.SubagentIteration{
			SchemaVersion: events.SubagentIterationVersion,
			SpawnID:       "spawn-1", Iteration: 1, Content: "reading", ParentTurnID: turnID,
		})
		_ = bus.Emit("subagent.complete", events.SubagentComplete{
			SchemaVersion: events.SubagentCompleteVersion,
			SpawnID:       "spawn-1", Result: "summarized", Iterations: 1, ParentTurnID: turnID,
		})
		_ = bus.Emit("llm.response", events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion, Content: answer, FinishReason: "end_turn",
			Model: "test-model",
			Usage: events.Usage{PromptTokens: 120, CompletionTokens: 40, TotalTokens: 160},
		})
		_ = bus.Emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion, Content: answer, Role: "assistant", TurnID: turnID,
		})
		_ = bus.Emit("agent.turn.end", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: turnID,
		})
	}
}

// ---- declaration ----

// TestCardDeclaresTheNexusExtension pins the discovery half: a client cannot opt
// into an extension it was never told about.
func TestCardDeclaresTheNexusExtension(t *testing.T) {
	cfg, err := parseConfig(testConfig(t, nil))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	card, err := buildCard(cfg)
	if err != nil {
		t.Fatalf("buildCard: %v", err)
	}
	exts := card.card.Capabilities.Extensions
	if len(exts) != 1 {
		t.Fatalf("card declares %d extensions, want exactly the Nexus one", len(exts))
	}
	if exts[0].URI != a2a.NexusExtensionURI {
		t.Errorf("declared extension URI = %q, want %q", exts[0].URI, a2a.NexusExtensionURI)
	}
	if exts[0].Required {
		t.Error("the Nexus extension is declared as required; everything it carries is supplementary")
	}
	if exts[0].Description == "" {
		t.Error("the declaration carries no description")
	}
}

// ---- opt-in ----

// TestExtensionTelemetryReachesAnOptedInStream is the acceptance criterion taken
// literally: a client that asked for the extension is shown the thinking steps,
// tool calls, subagent progress and token counts a turn produced.
func TestExtensionTelemetryReachesAnOptedInStream(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, telemetryTurn("all done"))

	rec := do(t, p.server, http.MethodPost, "/a2a",
		withVersion("1.0"), withExtensions(a2a.NexusExtensionURI),
		jsonrpcBody(t, a2a.MethodSendStreamingMessage, sendMessageParams("do work", "ctx-ext")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// Section 8.4: the agent answers with what it actually activated, so a client
	// that asked for several can tell which one it got.
	if got := rec.Header().Get(a2a.HeaderExtensions); got != a2a.NexusExtensionURI {
		t.Errorf("A2A-Extensions echo = %q, want %q", got, a2a.NexusExtensionURI)
	}

	fs := frames(t, rec.Body.Bytes())
	payloads := nexusEvents(t, fs)
	for _, want := range []a2a.NexusEventKind{
		a2a.NexusEventKindThinking,
		a2a.NexusEventKindToolCall,
		a2a.NexusEventKindToolResult,
		a2a.NexusEventKindSubagent,
		a2a.NexusEventKindUsage,
	} {
		if !hasKind(payloads, want) {
			t.Errorf("no %q event in the stream; kinds seen: %v", want, kinds(payloads))
		}
	}

	// The payloads name the task they belong to and carry a monotonic sequence,
	// which is what lets a client reassemble ordering across carriers.
	last := 0
	for _, e := range payloads {
		if e.TaskID != fs[0].Task.ID {
			t.Errorf("event names task %q, want %q", e.TaskID, fs[0].Task.ID)
		}
		if e.Sequence <= last {
			t.Errorf("sequence %d does not advance past %d", e.Sequence, last)
		}
		last = e.Sequence
	}
}

// TestExtensionIsNotForcedOnAClientThatDidNotAsk is the other half of section
// 8.4, and the reason opt-in exists at all.
func TestExtensionIsNotForcedOnAClientThatDidNotAsk(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, telemetryTurn("all done"))

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendStreamingMessage, sendMessageParams("do work", "ctx-noext")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get(a2a.HeaderExtensions); got != "" {
		t.Errorf("A2A-Extensions echo = %q for a client that asked for nothing", got)
	}

	fs := frames(t, rec.Body.Bytes())
	if payloads := nexusEvents(t, fs); len(payloads) != 0 {
		t.Fatalf("a client that did not opt in received %d extension payloads: %v",
			len(payloads), kinds(payloads))
	}
	// The canonical stream is complete regardless: the turn's states and its
	// artifacts are all there.
	want := []a2a.TaskState{a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted}
	if got := states(fs); !equalStates(got, want) {
		t.Errorf("canonical states = %v, want %v", got, want)
	}
}

// TestUnknownExtensionIsNotEchoed pins the negotiation's negative answer: an
// agent that does not speak an extension says so by not echoing it, rather than
// by refusing the request.
func TestUnknownExtensionIsNotEchoed(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, scriptedTurn("fine"))

	rec := do(t, p.server, http.MethodPost, "/a2a",
		withVersion("1.0"), withExtensions("https://example.test/extensions/unknown/v1"),
		jsonrpcBody(t, a2a.MethodSendMessage, sendMessageParams("hello", "ctx-unknown-ext")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get(a2a.HeaderExtensions); got != "" {
		t.Errorf("A2A-Extensions echo = %q for an extension this agent does not speak", got)
	}
}

// ---- telemetry is not task state ----

// TestTelemetryIsNotPersistedAsAStatusTransition pins the deliberate exception
// to the write-through rule. A stored telemetry frame would make GetTask replay
// a turn's reasoning as state changes the task never made.
func TestTelemetryIsNotPersistedAsAStatusTransition(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, telemetryTurn("all done"))

	rec := do(t, p.server, http.MethodPost, "/a2a",
		withVersion("1.0"), withExtensions(a2a.NexusExtensionURI),
		jsonrpcBody(t, a2a.MethodSendStreamingMessage, sendMessageParams("do work", "ctx-store")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	fs := frames(t, rec.Body.Bytes())
	taskID := fs[0].Task.ID

	rec2, found, err := p.tasks.For(nexusauth.Principal{}).Get(taskID)
	if err != nil || !found {
		t.Fatalf("reading the task back: %v (found=%v)", err, found)
	}
	// SUBMITTED, WORKING, COMPLETED — and nothing else, however much telemetry
	// crossed the wire.
	if len(rec2.StatusHistory) != 3 {
		t.Fatalf("stored status history = %d entries, want 3: %+v",
			len(rec2.StatusHistory), rec2.StatusHistory)
	}
	for _, status := range rec2.StatusHistory {
		if status.Message != nil && status.Message.Metadata[a2a.NexusExtensionURI] != nil {
			t.Error("a telemetry payload reached the store")
		}
	}
}

// TestTelemetryCarriesTheCurrentStateWhileParked pins that a telemetry frame
// emitted during an interruption does not tell the client the task went back to
// work.
func TestTelemetryCarriesTheCurrentStateWhileParked(t *testing.T) {
	p, _ := newTestPlugin(t, nil)
	r := p.startTurnForTest(t, "ctx-parked-telemetry")
	sub, _ := r.attach(streamOptions{nexusExtension: true})
	defer r.detach(sub)

	r.onTurnStart(events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"})
	drainStream(sub)
	if !r.park(parkedInput{requestID: "req-1"}, "which one?", 0, nil) {
		t.Fatal("the task did not park")
	}
	drainStream(sub)

	r.onThinking(events.ThinkingStep{
		SchemaVersion: events.ThinkingStepVersion, Content: "still waiting", Index: 1,
	})

	select {
	case frame := <-sub.frames:
		if frame.Kind() != a2a.StreamPayloadStatusUpdate {
			t.Fatalf("telemetry arrived as a %s frame", frame.Kind())
		}
		if got := frame.StatusUpdate.Status.State; got != a2a.TaskStateInputRequired {
			t.Errorf("telemetry frame reports state %s while the task is parked", got)
		}
	default:
		t.Fatal("no telemetry frame was delivered to an opted-in observer")
	}
	r.fail("test teardown")
}

// drainStream empties an observer's channel of whatever is queued so far.
func drainStream(s *stream) {
	for {
		select {
		case <-s.frames:
		default:
			return
		}
	}
}

// TestTelemetryIsSkippedWhenNobodyOptedIn pins the cheap path: a tool-heavy turn
// with no extension client must not marshal telemetry it will throw away.
func TestTelemetryIsSkippedWhenNobodyOptedIn(t *testing.T) {
	p, _ := newTestPlugin(t, nil)
	r := p.startTurnForTest(t, "ctx-skip")
	sub, _ := r.attach(streamOptions{})
	defer r.detach(sub)

	r.onTurnStart(events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"})
	drainStream(sub)

	r.onThinking(events.ThinkingStep{
		SchemaVersion: events.ThinkingStepVersion, Content: "thinking", Index: 1,
	})
	select {
	case frame := <-sub.frames:
		t.Fatalf("an observer that did not opt in received a %s frame", frame.Kind())
	default:
	}
	if r.seq != 0 {
		t.Errorf("sequence advanced to %d with no extension observer attached", r.seq)
	}
	r.fail("test teardown")
}
