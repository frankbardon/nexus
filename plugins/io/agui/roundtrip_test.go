package agui

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newTestPlugin boots the plugin against a real engine bus on an ephemeral
// loopback port and returns the plugin, its bus, and the endpoint URL.
func newTestPlugin(t *testing.T) (*Plugin, engine.EventBus, string) {
	t.Helper()
	bus := engine.NewEventBus()
	addr := freeAddr(t)

	p := New().(*Plugin)
	ctx := engine.PluginContext{
		Config: map[string]any{"bind": addr},
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := p.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })

	// Wait for the listener.
	url := "http://" + addr + agentPath
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodOptions, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			return p, bus, url
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server did not come up")
	return nil, nil, ""
}

// TestRoundTrip drives the full inbound->bus->outbound cycle: a POSTed
// RunAgentInput becomes an io.input; a simulated agent turn's bus events stream
// back as canonical AG-UI SSE terminating at RunFinished.
func TestRoundTrip(t *testing.T) {
	_, bus, url := newTestPlugin(t)

	// Capture the emitted io.input so we can assert the inbound mapping and use
	// its arrival as the trigger to simulate an agent turn.
	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	// Simulate an agent turn once io.input arrives.
	go func() {
		in := <-inputSeen
		if in.Content != "hello there" {
			t.Errorf("io.input content = %q, want %q", in.Content, "hello there")
		}
		if in.SessionID != "thread-1" {
			t.Errorf("io.input session = %q, want thread-1", in.SessionID)
		}
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("llm.stream.chunk", events.StreamChunk{SchemaVersion: events.StreamChunkVersion, Content: "Hi ", TurnID: "turn-1"})
		_ = bus.Emit("llm.stream.chunk", events.StreamChunk{SchemaVersion: events.StreamChunkVersion, Content: "there", TurnID: "turn-1"})
		_ = bus.Emit("llm.stream.end", events.StreamEnd{SchemaVersion: events.StreamEndVersion, TurnID: "turn-1"})
		_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "tc1", Name: "search", Arguments: map[string]any{"q": "x"}, TurnID: "turn-1"})
		_ = bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "tc1", Name: "search", Output: "result", TurnID: "turn-1"})
		_ = bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, Content: "pondering", TurnID: "turn-1"})
		_ = bus.Emit("workflow.progress", map[string]any{"pct": 50})
		_ = bus.Emit("agent.turn.end", turn)
	}()

	body := `{"threadId":"thread-1","runId":"run-1","messages":[{"id":"m1","role":"user","content":"hello there"}]}`
	resp := post(t, url, "", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	evs, err := agui.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("no events")
	}

	// First and last event bracket the run.
	if evs[0].EventType() != agui.EventRunStarted {
		t.Fatalf("event[0] = %s, want RunStarted", evs[0].EventType())
	}
	if got := evs[len(evs)-1].EventType(); got != agui.EventRunFinished {
		t.Fatalf("last event = %s, want RunFinished", got)
	}

	types := map[agui.EventType]int{}
	var custom *agui.CustomEvent
	for _, e := range evs {
		types[e.EventType()]++
		if c, ok := e.(*agui.CustomEvent); ok {
			custom = c
		}
	}

	want := []agui.EventType{
		agui.EventStepStarted,
		agui.EventTextMessageStart,
		agui.EventTextMessageContent,
		agui.EventTextMessageEnd,
		agui.EventToolCallStart,
		agui.EventToolCallArgs,
		agui.EventToolCallEnd,
		agui.EventToolCallResult,
		agui.EventReasoningStart,
		agui.EventReasoningMessageContent,
		agui.EventReasoningEnd,
		agui.EventStepFinished,
	}
	for _, wt := range want {
		if types[wt] == 0 {
			t.Errorf("missing event type %s", wt)
		}
	}
	if types[agui.EventTextMessageContent] != 2 {
		t.Errorf("TextMessageContent count = %d, want 2", types[agui.EventTextMessageContent])
	}

	// The non-canonical workflow.progress event must ride a Custom event.
	if custom == nil {
		t.Fatal("workflow.progress did not ride a Custom event")
	}
	if custom.Name != "workflow.progress" {
		t.Errorf("custom name = %q, want workflow.progress", custom.Name)
	}
	if !strings.Contains(string(custom.Value), "50") {
		t.Errorf("custom value = %s, want it to carry the payload", custom.Value)
	}
}

// TestSecondRunRejectedWhileInFlight verifies the one-run-per-listener rule:
// while a run is in flight, a second POST returns a well-formed terminal
// RunStarted+RunError stream instead of interleaving. It exercises the plugin's
// run-slot arbitration directly so the assertion is deterministic and does not
// depend on HTTP keep-alive timing.
func TestSecondRunRejectedWhileInFlight(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{"bind": freeAddr(t)},
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	first, ok := p.startRun(runInput{threadID: "t1", runID: "r1"})
	if !ok {
		t.Fatal("first startRun rejected, want accepted")
	}

	// A second run must be rejected while the first is active.
	if _, ok := p.startRun(runInput{threadID: "t2", runID: "r2"}); ok {
		t.Fatal("second startRun accepted, want rejected while first in flight")
	}

	// After the first run ends, the slot frees and a new run is accepted.
	first.finish()
	p.endRun(first)
	second, ok := p.startRun(runInput{threadID: "t3", runID: "r3"})
	if !ok {
		t.Fatal("third startRun rejected after slot freed, want accepted")
	}
	second.finish()
	p.endRun(second)
}
