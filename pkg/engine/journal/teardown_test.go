package journal

import (
	"slices"
	"testing"
)

func TestTeardownFilter_Classifies(t *testing.T) {
	envs := []Envelope{
		{Seq: 1, Type: "core.boot"},
		{Seq: 2, Type: "io.input"},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "llm.response"},
		{Seq: 5, Type: "agent.turn.end"},
		{Seq: 6, Type: "core.shutdown"},
		{Seq: 7, Type: "tool.result"},
		{Seq: 8, Type: "agent.turn.start"}, // starts inside the window
		{Seq: 9, Type: "agent.turn.end"},
		{Seq: 10, Type: "core.boot"}, // a recalled run appends here
		{Seq: 11, Type: "llm.response"},
	}
	var f TeardownFilter
	var got []uint64
	for _, e := range envs {
		if f.Observe(e) {
			got = append(got, e.Seq)
		}
	}
	if want := []uint64{6, 7, 8, 9}; !slices.Equal(got, want) {
		t.Fatalf("teardown seqs = %v, want %v", got, want)
	}
}

// A turn still open when core.shutdown fires and finishing in the drain is
// session work, through its closing agent.turn.end; what follows is teardown.
func TestTeardownFilter_TurnOpenAtShutdownIsNotTeardown(t *testing.T) {
	envs := []Envelope{
		{Seq: 1, Type: "io.input"},
		{Seq: 2, Type: "agent.turn.start"},
		{Seq: 3, Type: "core.shutdown"},
		{Seq: 4, Type: "llm.response"},
		{Seq: 5, Type: "agent.turn.end"},
		{Seq: 6, Type: "llm.response"},
	}
	var f TeardownFilter
	var got []uint64
	for _, e := range envs {
		if f.Observe(e) {
			got = append(got, e.Seq)
		}
	}
	if want := []uint64{3, 6}; !slices.Equal(got, want) {
		t.Fatalf("teardown seqs = %v, want %v", got, want)
	}
}

// Responses journaled after core.shutdown, once no turn is open, are not
// stashed: they would sit at the head of the FIFO and be popped by the wrong
// replayed request. A recalled run's responses after the next core.boot are.
func TestCoordinator_DoesNotStashTeardownResponses(t *testing.T) {
	dir := t.TempDir()
	writeJournal(t, dir, []Envelope{
		{Seq: 1, Type: "core.boot"},
		{Seq: 2, Type: "io.input", Payload: map[string]any{"content": "one"}},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "llm.response", Payload: map[string]any{"content": "reply one"}},
		{Seq: 5, Type: "agent.turn.end"},
		{Seq: 6, Type: "core.shutdown"},
		{Seq: 7, Type: "llm.response", Payload: map[string]any{"content": "stale"}},
		{Seq: 8, Type: "tool.result", Payload: map[string]any{"output": "stale"}},
		{Seq: 9, Type: "hitl.responded", Payload: map[string]any{"answer": "stale"}},
		{Seq: 10, Type: "core.boot"},
		{Seq: 11, Type: "io.input", Payload: map[string]any{"content": "two"}},
		{Seq: 12, Type: "agent.turn.start"},
		{Seq: 13, Type: "llm.response", Payload: map[string]any{"content": "reply two"}},
		{Seq: 14, Type: "agent.turn.end"},
	})

	rep := newStubReplay()
	c, err := NewCoordinator(dir, &stubBus{}, rep, CoordinatorOptions{})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	var contents []any
	for _, p := range rep.queues["llm.response"] {
		contents = append(contents, p.(map[string]any)["content"])
	}
	if want := []any{"reply one", "reply two"}; !slices.Equal(contents, want) {
		t.Fatalf("llm.response stash = %v, want %v", contents, want)
	}
	if n := len(rep.queues["tool.result"]); n != 0 {
		t.Fatalf("tool.result stash len = %d, want 0", n)
	}
	if n := len(rep.queues["hitl.responded"]); n != 0 {
		t.Fatalf("hitl.responded stash len = %d, want 0", n)
	}
	if c.IsPartialTurn() {
		t.Fatal("complete journal reads as partial turn")
	}
	if got := len(c.Inputs()); got != 2 {
		t.Fatalf("Inputs len = %d, want 2", got)
	}
}

// A turn that finishes during the shutdown drain keeps its answer in the
// stash: replaying that turn's input makes the same request again.
func TestCoordinator_StashesResponsesOfTurnFinishingInDrain(t *testing.T) {
	dir := t.TempDir()
	writeJournal(t, dir, []Envelope{
		{Seq: 1, Type: "io.input", Payload: map[string]any{"content": "one"}},
		{Seq: 2, Type: "agent.turn.start"},
		{Seq: 3, Type: "core.shutdown"},
		{Seq: 4, Type: "llm.response", Payload: map[string]any{"content": "reply one"}},
		{Seq: 5, Type: "agent.turn.end"},
		{Seq: 6, Type: "llm.response", Payload: map[string]any{"content": "stale"}},
	})

	rep := newStubReplay()
	c, err := NewCoordinator(dir, &stubBus{}, rep, CoordinatorOptions{})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if c.IsPartialTurn() {
		t.Fatal("turn that ended in the drain reads as partial")
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	q := rep.queues["llm.response"]
	if len(q) != 1 || q[0].(map[string]any)["content"] != "reply one" {
		t.Fatalf("llm.response stash = %v, want only [reply one]", q)
	}
}
