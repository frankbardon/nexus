package agui

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// parkedTools advertises two client tools.
const parkedTools = `"tools":[` +
	`{"name":"read_doc","description":"Read the document","parameters":{"type":"object"}},` +
	`{"name":"explain_doc","description":"Explain the document","parameters":{"type":"object"}}]`

// readRun POSTs body and reads the whole SSE stream of the run.
func readRun(t *testing.T, url, body string) []agui.Event {
	t.Helper()
	resp := post(t, url, "", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	evs, err := agui.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	return evs
}

// finishedOf is the run's RunFinished, and its interrupt when it is one.
func finishedOf(t *testing.T, evs []agui.Event) (*agui.RunFinishedEvent, *agui.Interrupt) {
	t.Helper()
	var fin *agui.RunFinishedEvent
	for _, e := range evs {
		if fe, ok := e.(*agui.RunFinishedEvent); ok {
			fin = fe
		}
	}
	if fin == nil {
		t.Fatalf("no RunFinished (events=%v)", eventTypes(evs))
	}
	if fin.Outcome != agui.OutcomeInterrupt {
		return fin, nil
	}
	var ip agui.Interrupt
	if err := json.Unmarshal(fin.Result, &ip); err != nil || ip.InterruptID == "" {
		t.Fatalf("decode interrupt %s: %v", fin.Result, err)
	}
	return fin, &ip
}

// toolCallsOf lists the tool call ids whose ToolCallStart streamed.
func toolCallsOf(evs []agui.Event) []string {
	var ids []string
	for _, e := range evs {
		if s, ok := e.(*agui.ToolCallStartEvent); ok {
			ids = append(ids, s.ToolCallID)
		}
	}
	return ids
}

// resultsCollector gathers tool.result events by call id.
type resultsCollector struct {
	mu  sync.Mutex
	got map[string]events.ToolResult
}

func collectResults(bus engine.EventBus) *resultsCollector {
	c := &resultsCollector{got: map[string]events.ToolResult{}}
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok {
			c.mu.Lock()
			c.got[r.ID] = r
			c.mu.Unlock()
		}
	})
	return c
}

func (c *resultsCollector) wait(t *testing.T, ids ...string) map[string]events.ToolResult {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		ok := true
		for _, id := range ids {
			if _, seen := c.got[id]; !seen {
				ok = false
			}
		}
		if ok {
			out := make(map[string]events.ToolResult, len(c.got))
			for k, v := range c.got {
				out[k] = v
			}
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("tool results %v never all arrived", ids)
	return nil
}

func clientCall(id, name string) events.ToolCall {
	return events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: id, Name: name, Arguments: map[string]any{}, TurnID: "turn-1"}
}

// startTurn runs the scripted agent body on the first io.input.
func startTurn(bus engine.EventBus, body func()) {
	var once sync.Once
	bus.Subscribe("io.input", func(engine.Event[any]) {
		once.Do(func() { go body() })
	})
}

// Two client tools in one LLM response: the agent dispatches both back to
// back, the first parks the run, and the second is delivered on the
// continuation run as its own interrupt rather than lost ("unknown tool").
// While parked, the deferred tool stays offered in the catalog.
func TestParkedTurnDeliversSecondClientTool(t *testing.T) {
	p, bus, url := newTestPlugin(t)
	results := collectResults(bus)
	offeredWhileParked := make(chan bool, 1)
	done := make(chan struct{})
	turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"}
	startTurn(bus, func() {
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("tool.invoke", clientCall("call-read", "read_doc"))
		q := &events.ToolCatalogQuery{SchemaVersion: events.ToolCatalogQueryVersion}
		_ = bus.Emit("tool.catalog.query", q)
		found := false
		for _, td := range q.Tools {
			found = found || td.Name == "explain_doc"
		}
		offeredWhileParked <- found
		_ = bus.Emit("tool.invoke", clientCall("call-explain", "explain_doc"))
		got := results.wait(t, "call-read", "call-explain")
		if got["call-read"].Output != "doc" || got["call-explain"].Output != "it reads" {
			t.Errorf("results = %+v", got)
		}
		_ = bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "done", Role: "assistant"})
		_ = bus.Emit("agent.turn.end", turn)
		close(done)
	})

	evs1 := readRun(t, url, `{"threadId":"th","runId":"run-1","messages":[{"id":"m1","role":"user","content":"explain"}],`+parkedTools+`}`)
	_, ip1 := finishedOf(t, evs1)
	if ip1 == nil {
		t.Fatalf("run1 did not interrupt (events=%v)", eventTypes(evs1))
	}
	if ids := toolCallsOf(evs1); len(ids) != 1 || ids[0] != "call-read" {
		t.Fatalf("run1 tool calls = %v, want [call-read]", ids)
	}
	if !<-offeredWhileParked {
		t.Error("explain_doc was not offered while the turn was parked")
	}
	waitFor(t, func() bool { return p.currentRun() == nil })

	evs2 := readRun(t, url, `{"threadId":"th","runId":"run-2",`+parkedTools+`,"resume":[{"interruptId":"`+ip1.InterruptID+`","status":"resolved","payload":{"output":"doc"}}]}`)
	_, ip2 := finishedOf(t, evs2)
	if ip2 == nil {
		t.Fatalf("run2 did not interrupt for the deferred tool (events=%v)", eventTypes(evs2))
	}
	if ids := toolCallsOf(evs2); len(ids) != 1 || ids[0] != "call-explain" {
		t.Fatalf("run2 tool calls = %v, want [call-explain]", ids)
	}
	waitFor(t, func() bool { return p.currentRun() == nil })

	evs3 := readRun(t, url, `{"threadId":"th","runId":"run-3",`+parkedTools+`,"resume":[{"interruptId":"`+ip2.InterruptID+`","status":"resolved","payload":{"output":"it reads"}}]}`)
	if fin, ip3 := finishedOf(t, evs3); ip3 != nil {
		t.Fatalf("run3 interrupted again: %+v", fin)
	}
	<-done
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.parked == nil
	})
}

// A client tool then an approval question in one response (a gate on the
// next tool call asks while the run is parked): the question is delivered
// on the continuation run, not dropped with the gate blocked forever.
func TestParkedTurnDeliversDeferredQuestion(t *testing.T) {
	p, bus, url := newTestPlugin(t)
	results := collectResults(bus)
	answered := make(chan events.HITLResponse, 1)
	bus.Subscribe("hitl.responded", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.HITLResponse); ok && r.RequestID == "req-run" {
			answered <- r
		}
	})
	done := make(chan struct{})
	turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"}
	startTurn(bus, func() {
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("tool.invoke", clientCall("call-read", "read_doc"))
		_ = bus.Emit("hitl.requested", events.HITLRequest{
			SchemaVersion: events.HITLRequestVersion, ID: "req-run", TurnID: "turn-1",
			Mode: events.HITLModeChoices, Prompt: "Run it?",
			Choices: []events.HITLChoice{{ID: "allow", Label: "Approve"}, {ID: "deny", Label: "Deny"}},
		})
		resp := <-answered
		if resp.ChoiceID != "allow" {
			t.Errorf("answer = %+v, want allow", resp)
		}
		results.wait(t, "call-read")
		_ = bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "ran", Role: "assistant"})
		_ = bus.Emit("agent.turn.end", turn)
		close(done)
	})

	evs1 := readRun(t, url, `{"threadId":"th","runId":"run-1","messages":[{"id":"m1","role":"user","content":"run"}],`+parkedTools+`}`)
	_, ip1 := finishedOf(t, evs1)
	if ip1 == nil {
		t.Fatalf("run1 did not interrupt")
	}
	// The question waits behind the client tool: run1 shows only the tool.
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.parked != nil && len(p.parked.queue) == 1
	})

	evs2 := readRun(t, url, `{"threadId":"th","runId":"run-2",`+parkedTools+`,"resume":[{"interruptId":"`+ip1.InterruptID+`","status":"resolved","payload":{"output":"doc"}}]}`)
	_, ip2 := finishedOf(t, evs2)
	if ip2 == nil || ip2.Prompt != "Run it?" {
		t.Fatalf("run2 interrupt = %+v, want the deferred question", ip2)
	}
	waitFor(t, func() bool { return p.currentRun() == nil })

	evs3 := readRun(t, url, `{"threadId":"th","runId":"run-3",`+parkedTools+`,"resume":[{"interruptId":"`+ip2.InterruptID+`","status":"resolved","payload":{"choiceId":"allow"}}]}`)
	if _, ip3 := finishedOf(t, evs3); ip3 != nil {
		t.Fatalf("run3 interrupted again")
	}
	<-done
}

// Cancelling the interrupt that parked the turn cancels what it deferred:
// the agent gets an error result for the second client tool and moves on.
func TestParkedTurnCancelCancelsDeferred(t *testing.T) {
	p, bus, url := newTestPlugin(t)
	results := collectResults(bus)
	done := make(chan struct{})
	turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"}
	startTurn(bus, func() {
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("tool.invoke", clientCall("call-read", "read_doc"))
		_ = bus.Emit("tool.invoke", clientCall("call-explain", "explain_doc"))
		got := results.wait(t, "call-read", "call-explain")
		if got["call-explain"].Error != "client tool call cancelled" {
			t.Errorf("deferred result = %+v, want cancelled", got["call-explain"])
		}
		_ = bus.Emit("agent.turn.end", turn)
		close(done)
	})

	evs1 := readRun(t, url, `{"threadId":"th","runId":"run-1","messages":[{"id":"m1","role":"user","content":"x"}],`+parkedTools+`}`)
	_, ip1 := finishedOf(t, evs1)
	if ip1 == nil {
		t.Fatalf("run1 did not interrupt")
	}
	waitFor(t, func() bool { return p.currentRun() == nil })
	evs2 := readRun(t, url, `{"threadId":"th","runId":"run-2",`+parkedTools+`,"resume":[{"interruptId":"`+ip1.InterruptID+`","status":"cancelled"}]}`)
	if _, ip2 := finishedOf(t, evs2); ip2 != nil {
		t.Fatalf("run2 interrupted after a cancel")
	}
	<-done
}
