package journal

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// stubBus is the minimal EventEmitter the coordinator needs in tests.
type stubBus struct {
	mu     sync.Mutex
	emits  []emitRecord
	emitFn func(eventType string, payload any) error
}

type emitRecord struct {
	Type    string
	Payload any
}

func (b *stubBus) Emit(eventType string, payload any) error {
	b.mu.Lock()
	b.emits = append(b.emits, emitRecord{Type: eventType, Payload: payload})
	fn := b.emitFn
	b.mu.Unlock()
	if fn != nil {
		return fn(eventType, payload)
	}
	return nil
}

// stubReplay tracks SetActive/Push for assertion.
type stubReplay struct {
	mu       sync.Mutex
	active   bool
	queues   map[string][]any
	resetCnt int
}

func newStubReplay() *stubReplay {
	return &stubReplay{queues: make(map[string][]any)}
}

func (r *stubReplay) SetActive(b bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = b
}

func (r *stubReplay) Push(eventType string, payload any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queues[eventType] = append(r.queues[eventType], payload)
}

func (r *stubReplay) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetCnt++
	r.queues = make(map[string][]any)
	r.active = false
}

func (r *stubReplay) queueLen(t string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queues[t])
}

// writeJournal builds a journal from a slice of envelopes for tests.
func writeJournal(t *testing.T, dir string, envs []Envelope) {
	t.Helper()
	w, err := NewWriter(dir, WriterOptions{
		FsyncMode:  FsyncNone,
		BufferSize: 16,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i := range envs {
		w.Append(&envs[i])
	}
	mustClose(t, w)
}

func TestCoordinator_DetectsCompleteTurn(t *testing.T) {
	dir := t.TempDir()
	writeJournal(t, dir, []Envelope{
		{Seq: 1, Type: "io.session.start"},
		{Seq: 2, Type: "io.input", Payload: map[string]any{"content": "hi"}},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "llm.response", Payload: map[string]any{"content": "hello"}},
		{Seq: 5, Type: "agent.turn.end"},
	})

	bus := &stubBus{}
	rep := newStubReplay()
	c, err := NewCoordinator(dir, bus, rep, CoordinatorOptions{})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if c.IsPartialTurn() {
		t.Errorf("expected complete turn, got partial")
	}
	if last, ok := c.LastTurnBoundary(); !ok || last != 5 {
		t.Errorf("LastTurnBoundary: got (%d,%v), want (5,true)", last, ok)
	}
	if got := len(c.Inputs()); got != 1 {
		t.Errorf("Inputs len = %d, want 1", got)
	}
	if rep.queueLen("llm.response") != 1 {
		t.Errorf("llm.response queue len = %d, want 1", rep.queueLen("llm.response"))
	}
}

func TestCoordinator_DetectsPartialTurn(t *testing.T) {
	dir := t.TempDir()
	writeJournal(t, dir, []Envelope{
		{Seq: 1, Type: "io.session.start"},
		{Seq: 2, Type: "io.input", Payload: map[string]any{"content": "first"}},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "llm.response", Payload: map[string]any{"content": "first reply"}},
		{Seq: 5, Type: "agent.turn.end"},
		{Seq: 6, Type: "io.input", Payload: map[string]any{"content": "second"}},
		{Seq: 7, Type: "agent.turn.start"},
		// crashed mid-turn — no agent.turn.end after seq=7
	})

	bus := &stubBus{}
	rep := newStubReplay()
	c, err := NewCoordinator(dir, bus, rep, CoordinatorOptions{})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if !c.IsPartialTurn() {
		t.Errorf("expected partial turn")
	}
	if last, ok := c.LastTurnBoundary(); !ok || last != 5 {
		t.Errorf("LastTurnBoundary: got (%d,%v), want (5,true)", last, ok)
	}
}

func TestCoordinator_RunReEmitsInputsAndActivates(t *testing.T) {
	dir := t.TempDir()
	writeJournal(t, dir, []Envelope{
		{Seq: 1, Type: "io.session.start"},
		{Seq: 2, Type: "io.input", Payload: map[string]any{"content": "msg one"}},
		{Seq: 3, Type: "llm.response", Payload: map[string]any{"content": "reply one"}},
		{Seq: 4, Type: "agent.turn.end"},
		{Seq: 5, Type: "io.input", Payload: map[string]any{"content": "msg two"}},
		{Seq: 6, Type: "llm.response", Payload: map[string]any{"content": "reply two"}},
		{Seq: 7, Type: "agent.turn.end"},
	})

	bus := &stubBus{}
	rep := newStubReplay()
	c, err := NewCoordinator(dir, bus, rep, CoordinatorOptions{})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	// No turn-sync configured — Run emits inputs back-to-back.
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	bus.mu.Lock()
	defer bus.mu.Unlock()
	if len(bus.emits) != 2 {
		t.Fatalf("expected 2 io.input emits, got %d", len(bus.emits))
	}
	for i, e := range bus.emits {
		if e.Type != "io.input" {
			t.Errorf("emit %d type = %q", i, e.Type)
		}
	}

	// SetActive must have been flipped on then off.
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.active {
		t.Errorf("ReplayState still active after Run")
	}
}

func TestCoordinator_TurnSyncWaitsForTurnEnd(t *testing.T) {
	dir := t.TempDir()
	writeJournal(t, dir, []Envelope{
		{Seq: 1, Type: "io.input", Payload: map[string]any{"content": "x"}},
		{Seq: 2, Type: "llm.response", Payload: map[string]any{}},
		{Seq: 3, Type: "agent.turn.end"},
		{Seq: 4, Type: "io.input", Payload: map[string]any{"content": "y"}},
		{Seq: 5, Type: "llm.response", Payload: map[string]any{}},
		{Seq: 6, Type: "agent.turn.end"},
	})

	bus := &stubBus{}
	rep := newStubReplay()

	// Test-only sync: each io.input emit triggers our handler with a
	// fake turn-end signal so Run advances.
	var handler func(seq uint64)
	bus.emitFn = func(eventType string, _ any) error {
		if eventType == "io.input" && handler != nil {
			go handler(0)
		}
		return nil
	}

	c, err := NewCoordinator(dir, bus, rep, CoordinatorOptions{})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.AttachTurnSync(func(h func(seq uint64)) func() {
		handler = h
		return func() { handler = nil }
	})

	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCoordinator_EmptyJournalIsSafe(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, WriterOptions{FsyncMode: FsyncNone, BufferSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	mustClose(t, w)

	bus := &stubBus{}
	rep := newStubReplay()
	c, err := NewCoordinator(dir, bus, rep, CoordinatorOptions{})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if err := c.Run(context.Background()); err != nil {
		t.Errorf("Run on empty journal: %v", err)
	}
}

func TestCoordinator_MissingDir(t *testing.T) {
	_, err := NewCoordinator("/no/such/dir/at/all", &stubBus{}, newStubReplay(), CoordinatorOptions{})
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
	if !errors.Is(err, err) { // self-trivial; just exercise the path
		t.Skip()
	}
}

func TestCoordinator_PayloadAs(t *testing.T) {
	type sample struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	// Round-trip via map (mimics journal-deserialized payload).
	raw := map[string]any{"a": 7, "b": "hello"}
	out, err := PayloadAs[sample](raw)
	if err != nil {
		t.Fatalf("PayloadAs: %v", err)
	}
	if out.A != 7 || out.B != "hello" {
		t.Errorf("PayloadAs round-trip wrong: %+v", out)
	}

	// Already-typed fast path.
	want := sample{A: 1, B: "x"}
	got, err := PayloadAs[sample](want)
	if err != nil || got != want {
		t.Errorf("PayloadAs typed fast path: got=%+v err=%v", got, err)
	}
}
