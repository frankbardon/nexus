package agui

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestClientToolCycle is the core acceptance test for E2-S3: a client-executed
// (frontend) tool advertised via RunAgentInput.tools is surfaced to the agent,
// the agent calls it, the run ends interrupt-style awaiting the client's result,
// and a resume carrying the ToolCallResult feeds the result back so the agent
// continues and finishes in a fresh SSE stream.
func TestClientToolCycle(t *testing.T) {
	p, bus, url := newTestPlugin(t)

	// A mock agent: on io.input it queries the catalog (asserting the client tool
	// is visible), calls it via tool.invoke, blocks until the tool.result arrives,
	// then emits its final output and ends the turn.
	toolResult := make(chan events.ToolResult, 1)
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok && r.ID == "call-weather" {
			select {
			case toolResult <- r:
			default:
			}
		}
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

	sawClientTool := make(chan bool, 1)
	go func() {
		<-inputSeen

		// The client tool must be visible in the catalog snapshot the agent builds.
		tq := &events.ToolCatalogQuery{SchemaVersion: events.ToolCatalogQueryVersion}
		_ = bus.Emit("tool.catalog.query", tq)
		found := false
		for _, td := range tq.Tools {
			if td.Name == "get_weather" {
				found = true
			}
		}
		sawClientTool <- found

		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"}
		_ = bus.Emit("agent.turn.start", turn)
		// Call the client tool. No in-process handler produces a result — the run
		// ends interrupt-style awaiting the client.
		_ = bus.Emit("tool.invoke", events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            "call-weather",
			Name:          "get_weather",
			Arguments:     map[string]any{"city": "Paris"},
			TurnID:        "turn-1",
		})

		// Block until the client's result is fed back on resume.
		res := <-toolResult
		if res.Output != "sunny" {
			t.Errorf("tool.result output = %q, want sunny", res.Output)
		}

		out := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "It is sunny in Paris", Role: "assistant"}
		_ = bus.Emit("io.output", out)
		_ = bus.Emit("agent.turn.end", turn)
	}()

	// --- Run 1: POST with a client tool; read to the interrupt. ---
	body1 := `{"threadId":"thread-ct","runId":"run-1",` +
		`"messages":[{"id":"m1","role":"user","content":"weather in Paris?"}],` +
		`"tools":[{"name":"get_weather","description":"Get weather for a city",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	resp1 := post(t, url, "", "", body1)
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("run1 status = %d, want 200", resp1.StatusCode)
	}

	if !<-sawClientTool {
		t.Fatal("client tool get_weather was not surfaced in the catalog snapshot")
	}

	evs1, err := agui.NewSSEReader(resp1.Body).ReadAll()
	if err != nil {
		t.Fatalf("read run1 sse: %v", err)
	}

	// The ToolCall* sequence for the client tool must have streamed before the
	// interrupt.
	assertHasTypes(t, evs1, agui.EventToolCallStart, agui.EventToolCallArgs, agui.EventToolCallEnd)

	var fin1 *agui.RunFinishedEvent
	for _, e := range evs1 {
		if fe, ok := e.(*agui.RunFinishedEvent); ok {
			fin1 = fe
		}
	}
	if fin1 == nil {
		t.Fatalf("run1 produced no RunFinished (events=%v)", eventTypes(evs1))
	}
	if fin1.Outcome != agui.OutcomeInterrupt {
		t.Fatalf("run1 outcome = %q, want interrupt", fin1.Outcome)
	}
	var ip agui.Interrupt
	if err := json.Unmarshal(fin1.Result, &ip); err != nil {
		t.Fatalf("decode run1 interrupt: %v", err)
	}
	if ip.InterruptID == "" {
		t.Fatal("run1 interrupt missing interruptId")
	}

	waitFor(t, func() bool { return p.currentRun() == nil })

	// --- Run 2: resume with the client's ToolCallResult; read continuation. ---
	body2 := `{"threadId":"thread-ct","runId":"run-2","resume":[{"interruptId":"` + ip.InterruptID +
		`","status":"resolved","payload":{"output":"sunny"}}]}`
	resp2 := post(t, url, "", "", body2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("run2 status = %d, want 200", resp2.StatusCode)
	}
	evs2, err := agui.NewSSEReader(resp2.Body).ReadAll()
	if err != nil {
		t.Fatalf("read run2 sse: %v", err)
	}

	last := evs2[len(evs2)-1]
	fe2, ok := last.(*agui.RunFinishedEvent)
	if !ok {
		t.Fatalf("run2 last event = %s, want RunFinished", last.EventType())
	}
	if fe2.Outcome == agui.OutcomeInterrupt {
		t.Fatal("run2 should complete normally, not re-interrupt")
	}

	sawContent := false
	for _, e := range evs2 {
		if c, ok := e.(*agui.TextMessageContentEvent); ok && c.Delta == "It is sunny in Paris" {
			sawContent = true
		}
	}
	if !sawContent {
		t.Errorf("run2 did not stream the continuation content (events=%v)", eventTypes(evs2))
	}

	// The pending map must be empty after the client tool is resolved.
	waitFor(t, func() bool {
		p.pendingMu.Lock()
		defer p.pendingMu.Unlock()
		return len(p.pending) == 0
	})
}

// TestServerToolNotIntercepted asserts that a server-side (in-process) tool call
// is NOT confused with a client tool: it produces its ToolCall* + ToolCallResult
// sequence and the run completes normally without any interrupt, even when the
// same run also advertised an unrelated client tool.
func TestServerToolNotIntercepted(t *testing.T) {
	p, bus, url := newTestPlugin(t)

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	// A "server tool" plugin: it handles tool.invoke for "search" in-process and
	// emits a tool.result, exactly as a real Nexus catalog tool would.
	bus.Subscribe("tool.invoke", func(e engine.Event[any]) {
		tc, ok := e.Payload.(events.ToolCall)
		if !ok || tc.Name != "search" {
			return
		}
		_ = bus.Emit("tool.result", events.ToolResult{
			SchemaVersion: events.ToolResultVersion,
			ID:            tc.ID,
			Name:          tc.Name,
			Output:        "server result",
			TurnID:        tc.TurnID,
		})
	})

	go func() {
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"}
		_ = bus.Emit("agent.turn.start", turn)
		// Call the SERVER tool, not the advertised client tool.
		_ = bus.Emit("tool.invoke", events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            "call-search",
			Name:          "search",
			Arguments:     map[string]any{"q": "x"},
			TurnID:        "turn-1",
		})
		_ = bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "done", Role: "assistant"})
		_ = bus.Emit("agent.turn.end", turn)
	}()

	// The run advertises a client tool that is never called.
	body := `{"threadId":"thread-srv","runId":"run-1",` +
		`"messages":[{"id":"m1","role":"user","content":"search x"}],` +
		`"tools":[{"name":"get_weather","description":"unused client tool"}]}`
	resp := post(t, url, "", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	evs, err := agui.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}

	// Server tool produced the full sequence AND a result; the run completed
	// normally (no interrupt), proving the server tool was not intercepted.
	assertHasTypes(t, evs,
		agui.EventToolCallStart, agui.EventToolCallArgs, agui.EventToolCallEnd, agui.EventToolCallResult)

	last := evs[len(evs)-1]
	fe, ok := last.(*agui.RunFinishedEvent)
	if !ok {
		t.Fatalf("last event = %s, want RunFinished", last.EventType())
	}
	if fe.Outcome == agui.OutcomeInterrupt {
		t.Fatal("server tool call must not interrupt the run")
	}

	// No pending interrupt should have been recorded.
	p.pendingMu.Lock()
	n := len(p.pending)
	p.pendingMu.Unlock()
	if n != 0 {
		t.Fatalf("pending len = %d, want 0 (server tool must not suspend)", n)
	}
}

// TestRealToolInvokeSequence is the Task B regression test: a real agent tool
// call (emitted on tool.invoke, as the ReAct agent does — NOT tool.call) yields
// the full ToolCallStart/Args/End -> ToolCallResult sequence on the AG-UI stream.
func TestRealToolInvokeSequence(t *testing.T) {
	_, bus, url := newTestPlugin(t)

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	go func() {
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("tool.invoke", events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            "tc1",
			Name:          "search",
			Arguments:     map[string]any{"q": "x"},
			TurnID:        "turn-1",
		})
		_ = bus.Emit("tool.result", events.ToolResult{
			SchemaVersion: events.ToolResultVersion,
			ID:            "tc1",
			Name:          "search",
			Output:        "result",
			TurnID:        "turn-1",
		})
		_ = bus.Emit("agent.turn.end", turn)
	}()

	body := `{"threadId":"thread-b","runId":"run-1","messages":[{"id":"m1","role":"user","content":"search"}]}`
	resp := post(t, url, "", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	evs, err := agui.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}

	// The full ToolCall* sequence must appear — previously nothing did because the
	// plugin subscribed to tool.call while the agent emits tool.invoke.
	assertHasTypes(t, evs,
		agui.EventToolCallStart, agui.EventToolCallArgs, agui.EventToolCallEnd, agui.EventToolCallResult)
}

// assertHasTypes fails if any of the wanted event types is absent from evs.
func assertHasTypes(t *testing.T, evs []agui.Event, want ...agui.EventType) {
	t.Helper()
	present := map[agui.EventType]bool{}
	for _, e := range evs {
		present[e.EventType()] = true
	}
	for _, wt := range want {
		if !present[wt] {
			t.Errorf("missing event type %s (events=%v)", wt, eventTypes(evs))
		}
	}
}
