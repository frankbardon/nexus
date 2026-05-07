package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestReplayState_NilReceiverSafe(t *testing.T) {
	var r *ReplayState
	if r.Active() {
		t.Error("nil ReplayState.Active() should be false")
	}
	r.SetActive(true) // must not panic
	r.Push("any", "x")
	if got, ok := r.Pop("any"); ok || got != nil {
		t.Errorf("nil Pop should return (nil,false); got (%v,%v)", got, ok)
	}
	if r.Remaining("any") != 0 {
		t.Error("nil Remaining should be 0")
	}
	r.Reset() // must not panic
	r.SetToolCache(nil)
	if r.ToolCache() != nil {
		t.Error("nil ToolCache should be nil")
	}
}

func TestReplayState_ActiveToggle(t *testing.T) {
	r := NewReplayState()
	if r.Active() {
		t.Error("fresh state should be inactive")
	}
	r.SetActive(true)
	if !r.Active() {
		t.Error("SetActive(true) did not propagate")
	}
	r.SetActive(false)
	if r.Active() {
		t.Error("SetActive(false) did not propagate")
	}
}

func TestReplayState_PushPopFIFO(t *testing.T) {
	r := NewReplayState()
	r.Push("evt", "a")
	r.Push("evt", "b")
	r.Push("evt", "c")

	if r.Remaining("evt") != 3 {
		t.Fatalf("Remaining = %d, want 3", r.Remaining("evt"))
	}

	for i, want := range []string{"a", "b", "c"} {
		got, ok := r.Pop("evt")
		if !ok {
			t.Fatalf("Pop[%d] returned ok=false", i)
		}
		if got != want {
			t.Errorf("Pop[%d] = %v, want %v", i, got, want)
		}
	}
	if _, ok := r.Pop("evt"); ok {
		t.Error("Pop after drain should return ok=false")
	}
}

func TestReplayState_QueuesIsolatedPerEventType(t *testing.T) {
	r := NewReplayState()
	r.Push("a", 1)
	r.Push("b", 2)

	if v, ok := r.Pop("b"); !ok || v != 2 {
		t.Errorf("Pop b = (%v,%v), want (2,true)", v, ok)
	}
	if v, ok := r.Pop("a"); !ok || v != 1 {
		t.Errorf("Pop a = (%v,%v), want (1,true)", v, ok)
	}
}

func TestReplayState_Reset_ClearsQueuesAndDeactivates(t *testing.T) {
	r := NewReplayState()
	r.SetActive(true)
	r.Push("evt", "x")

	r.Reset()

	if r.Active() {
		t.Error("Reset did not deactivate")
	}
	if r.Remaining("evt") != 0 {
		t.Error("Reset did not clear queues")
	}
}

func TestReplayState_SetToolCache_RoundTrip(t *testing.T) {
	r := NewReplayState()
	if r.ToolCache() != nil {
		t.Error("fresh state should have no cache")
	}
	c := NewToolCache(t.TempDir(), nil)
	r.SetToolCache(c)
	if r.ToolCache() != c {
		t.Error("SetToolCache did not retain handle")
	}
}

func TestWithReplay_AndIsReplay(t *testing.T) {
	// The replay context helpers explicitly document tolerance for a nil
	// receiver context; exercise that path via a typed-nil variable so
	// staticcheck's SA1012 (no literal nil ctx) is not provoked.
	var nilCtx context.Context
	if IsReplay(nilCtx) {
		t.Error("IsReplay(nil ctx) should be false")
	}
	if IsReplay(context.Background()) {
		t.Error("IsReplay(plain ctx) should be false")
	}
	rctx := WithReplay(context.Background())
	if !IsReplay(rctx) {
		t.Error("IsReplay on replay-tagged ctx should be true")
	}
	rnil := WithReplay(nilCtx)
	if !IsReplay(rnil) {
		t.Error("WithReplay(nil ctx) should still produce a replay ctx")
	}
}

func TestReplayToolShortCircuit_NilReplayReturnsFalse(t *testing.T) {
	bus := NewEventBus()
	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "x", Name: "shell"}
	if ReplayToolShortCircuit(nil, bus, tc, nil) {
		t.Error("nil replay should return false")
	}
}

func TestReplayToolShortCircuit_InactiveReturnsFalse(t *testing.T) {
	bus := NewEventBus()
	state := NewReplayState() // inactive by default

	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "x", Name: "shell"}
	if ReplayToolShortCircuit(state, bus, tc, nil) {
		t.Error("inactive replay should return false")
	}
}

func TestReplayToolShortCircuit_EmptyStashEmitsErrorSentinel(t *testing.T) {
	state := NewReplayState()
	state.SetActive(true)
	defer state.SetActive(false)

	// No cache attached; queue empty.
	bus := NewEventBus()
	var got []events.ToolResult
	bus.Subscribe("tool.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			got = append(got, r)
		}
	})

	tc := events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "live-empty",
		Name:          "shell",
		TurnID:        "turn-7",
		Arguments:     map[string]any{"k": "v"},
	}
	if !ReplayToolShortCircuit(state, bus, tc, nil) {
		t.Fatal("empty-stash short-circuit should still return true (sentinel emitted)")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 sentinel emit, got %d", len(got))
	}
	if got[0].Error == "" {
		t.Error("empty-stash sentinel must carry an error string")
	}
	if got[0].ID != "live-empty" {
		t.Errorf("sentinel must carry live invoke ID, got %q", got[0].ID)
	}
	if got[0].Name != "shell" {
		t.Errorf("sentinel must carry tool name, got %q", got[0].Name)
	}
	if got[0].TurnID != "turn-7" {
		t.Errorf("sentinel must carry TurnID, got %q", got[0].TurnID)
	}
}

func TestReplayToolShortCircuit_PreservesCorrelationOnFIFOFallback(t *testing.T) {
	state := NewReplayState()
	state.SetActive(true)
	defer state.SetActive(false)

	// Journaled result has stale ID + blank Name/TurnID. The short-circuit
	// rewrites ID unconditionally and backfills Name/TurnID when blank.
	// Non-blank Name/TurnID on the journaled record are preserved.
	state.Push("tool.result", events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "OLD-id",
		Name:          "",
		Output:        "FIFO output",
		TurnID:        "",
	})

	bus := NewEventBus()
	var got events.ToolResult
	bus.Subscribe("tool.result", func(ev Event[any]) {
		got = ev.Payload.(events.ToolResult)
	})

	tc := events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "live-fifo",
		Name:          "shell",
		TurnID:        "live-turn",
		Arguments:     map[string]any{"a": 1},
	}
	if !ReplayToolShortCircuit(state, bus, tc, nil) {
		t.Fatal("expected short-circuit true")
	}
	if got.ID != "live-fifo" {
		t.Errorf("ID not rewritten: %q", got.ID)
	}
	if got.Name != "shell" {
		t.Errorf("blank Name not backfilled: %q", got.Name)
	}
	if got.TurnID != "live-turn" {
		t.Errorf("blank TurnID not backfilled: %q", got.TurnID)
	}
	if got.Output != "FIFO output" {
		t.Errorf("Output should pass through: %q", got.Output)
	}
}

func TestReplayToolShortCircuit_PreservesNonBlankFIFOMetadata(t *testing.T) {
	state := NewReplayState()
	state.SetActive(true)
	defer state.SetActive(false)

	// When the journaled record already carries Name/TurnID, those win
	// over the live invoke values — only ID is force-rewritten.
	state.Push("tool.result", events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "OLD-id",
		Name:          "journaled-name",
		Output:        "out",
		TurnID:        "journaled-turn",
	})

	bus := NewEventBus()
	var got events.ToolResult
	bus.Subscribe("tool.result", func(ev Event[any]) {
		got = ev.Payload.(events.ToolResult)
	})

	tc := events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "live-id",
		Name:          "live-name",
		TurnID:        "live-turn",
	}
	if !ReplayToolShortCircuit(state, bus, tc, nil) {
		t.Fatal("expected short-circuit true")
	}
	if got.ID != "live-id" {
		t.Errorf("ID not rewritten: %q", got.ID)
	}
	if got.Name != "journaled-name" {
		t.Errorf("non-blank journaled Name should win, got %q", got.Name)
	}
	if got.TurnID != "journaled-turn" {
		t.Errorf("non-blank journaled TurnID should win, got %q", got.TurnID)
	}
}

func TestReplayToolShortCircuit_EmptyStashSentinelDoesNotConsumeFIFO(t *testing.T) {
	state := NewReplayState()
	state.SetActive(true)
	defer state.SetActive(false)
	cache := NewToolCache(filepath.Join(t.TempDir(), "cache"), nil)
	state.SetToolCache(cache)

	// Cache empty; FIFO empty too — sentinel path. Asserts no panic / no
	// negative-len slice when both lookups miss.
	bus := NewEventBus()
	var emitted int
	bus.Subscribe("tool.result", func(ev Event[any]) { emitted++ })

	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "x", Name: "shell"}
	if !ReplayToolShortCircuit(state, bus, tc, nil) {
		t.Fatal("expected true even on empty stash")
	}
	if emitted != 1 {
		t.Errorf("expected exactly 1 sentinel emit, got %d", emitted)
	}
}

func TestRewind_SessionJournalDir_EmptyIDError(t *testing.T) {
	cfg := *DefaultConfig()
	if _, err := sessionJournalDir(cfg, ""); err == nil {
		t.Error("expected error for empty session id")
	}
	if _, err := sessionRootDir(cfg, ""); err == nil {
		t.Error("expected error for empty session id (root dir)")
	}
}
