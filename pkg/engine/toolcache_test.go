package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestCanonicalArgsHash_StableAcrossKeyOrder(t *testing.T) {
	a, err := canonicalArgsHash("shell", map[string]any{"a": 1, "b": "two", "c": []any{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonicalArgsHash("shell", map[string]any{"c": []any{1, 2, 3}, "b": "two", "a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("hash differs across key order: %s vs %s", a, b)
	}
}

func TestCanonicalArgsHash_StableAcrossNestedKeyOrder(t *testing.T) {
	a, _ := canonicalArgsHash("file", map[string]any{
		"path": "/x",
		"opts": map[string]any{"deep": true, "verbose": false},
	})
	b, _ := canonicalArgsHash("file", map[string]any{
		"opts": map[string]any{"verbose": false, "deep": true},
		"path": "/x",
	})
	if a != b {
		t.Errorf("nested key order leaks into hash: %s vs %s", a, b)
	}
}

func TestCanonicalArgsHash_DiffersAcrossToolID(t *testing.T) {
	a, _ := canonicalArgsHash("shell", map[string]any{"command": "ls"})
	b, _ := canonicalArgsHash("file", map[string]any{"command": "ls"})
	if a == b {
		t.Errorf("hash should differ across tool IDs (same args): both = %s", a)
	}
}

func TestToolCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := NewToolCache(filepath.Join(dir, "cache"), nil)

	// Manually trigger invoke + result via the bus handlers (no engine wired).
	bus := NewEventBus()
	defer bus.Drain(context.Background()) //nolint:errcheck
	unsubs := c.Install(bus)
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	tc := events.ToolCall{ID: "call-1", Name: "shell", Arguments: map[string]any{"command": "ls"}}
	if err := bus.Emit("tool.invoke", tc); err != nil {
		t.Fatal(err)
	}
	res := events.ToolResult{ID: "call-1", Name: "shell", Output: "files\n"}
	if err := bus.Emit("tool.result", res); err != nil {
		t.Fatal(err)
	}

	got, ok := c.Lookup("shell", map[string]any{"command": "ls"})
	if !ok {
		t.Fatal("cache miss after live recording")
	}
	if got.Output != "files\n" {
		t.Errorf("cached output = %q", got.Output)
	}
}

func TestToolCache_LookupMissOnUnknownArgs(t *testing.T) {
	dir := t.TempDir()
	c := NewToolCache(filepath.Join(dir, "cache"), nil)
	if _, ok := c.Lookup("shell", map[string]any{"command": "ls"}); ok {
		t.Error("expected miss on empty cache")
	}
}

func TestToolCache_OverwritesOnSameArgs(t *testing.T) {
	dir := t.TempDir()
	c := NewToolCache(filepath.Join(dir, "cache"), nil)
	bus := NewEventBus()
	for _, u := range c.Install(bus) {
		defer u()
	}

	tc := events.ToolCall{ID: "id-1", Name: "shell", Arguments: map[string]any{"cmd": "x"}}
	bus.Emit("tool.invoke", tc)                                                            //nolint:errcheck
	bus.Emit("tool.result", events.ToolResult{ID: "id-1", Name: "shell", Output: "first"}) //nolint:errcheck

	tc2 := events.ToolCall{ID: "id-2", Name: "shell", Arguments: map[string]any{"cmd": "x"}}
	bus.Emit("tool.invoke", tc2)                                                            //nolint:errcheck
	bus.Emit("tool.result", events.ToolResult{ID: "id-2", Name: "shell", Output: "second"}) //nolint:errcheck

	got, ok := c.Lookup("shell", map[string]any{"cmd": "x"})
	if !ok || got.Output != "second" {
		t.Errorf("expected most-recent overwrite; got ok=%v output=%q", ok, got.Output)
	}
}

func TestToolCache_FilesLandUnderToolSubdir(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	c := NewToolCache(cacheDir, nil)
	bus := NewEventBus()
	for _, u := range c.Install(bus) {
		defer u()
	}

	bus.Emit("tool.invoke", events.ToolCall{ID: "x", Name: "shell", Arguments: map[string]any{"a": 1}}) //nolint:errcheck
	bus.Emit("tool.result", events.ToolResult{ID: "x", Name: "shell", Output: "ok"})                    //nolint:errcheck

	entries, err := os.ReadDir(filepath.Join(cacheDir, "shell"))
	if err != nil {
		t.Fatalf("expected shell subdir under cache: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 cache file under shell/, got %d", len(entries))
	}
}

func TestReplayToolShortCircuit_PrefersCacheOverFIFO(t *testing.T) {
	dir := t.TempDir()
	cache := NewToolCache(filepath.Join(dir, "cache"), nil)
	state := NewReplayState()
	state.SetToolCache(cache)
	state.SetActive(true)
	defer state.SetActive(false)

	// Pre-populate cache via a recording pass.
	bus := NewEventBus()
	for _, u := range cache.Install(bus) {
		defer u()
	}
	bus.Emit("tool.invoke", events.ToolCall{ID: "rec", Name: "shell", Arguments: map[string]any{"k": "v"}}) //nolint:errcheck
	bus.Emit("tool.result", events.ToolResult{ID: "rec", Name: "shell", Output: "from-cache"})              //nolint:errcheck

	// Stuff something into FIFO so we can verify cache won.
	state.Push("tool.result", events.ToolResult{Output: "from-FIFO"})

	// Now do a replay short-circuit on a NEW invoke with the same args.
	var (
		emitted []events.ToolResult
	)
	emitterBus := NewEventBus()
	emitterBus.Subscribe("tool.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			emitted = append(emitted, r)
		}
	})

	tc := events.ToolCall{ID: "live-1", Name: "shell", Arguments: map[string]any{"k": "v"}}
	if !ReplayToolShortCircuit(state, emitterBus, tc, nil) {
		t.Fatal("short-circuit returned false")
	}
	if len(emitted) != 1 {
		t.Fatalf("expected 1 tool.result emit, got %d", len(emitted))
	}
	if emitted[0].Output != "from-cache" {
		t.Errorf("cache should win over FIFO: got %q", emitted[0].Output)
	}
	if emitted[0].ID != "live-1" {
		t.Errorf("live invoke ID must be re-stamped: got %q", emitted[0].ID)
	}
	// FIFO should still have its entry — cache hit must not consume it.
	if state.Remaining("tool.result") != 1 {
		t.Errorf("FIFO drained on cache hit: remaining = %d", state.Remaining("tool.result"))
	}
}

func TestReplayToolShortCircuit_FallsBackToFIFO(t *testing.T) {
	dir := t.TempDir()
	cache := NewToolCache(filepath.Join(dir, "cache"), nil)
	state := NewReplayState()
	state.SetToolCache(cache)
	state.SetActive(true)
	defer state.SetActive(false)

	// Cache empty. FIFO has the answer.
	state.Push("tool.result", events.ToolResult{Output: "from-FIFO"})

	emitterBus := NewEventBus()
	var emitted []events.ToolResult
	emitterBus.Subscribe("tool.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			emitted = append(emitted, r)
		}
	})

	tc := events.ToolCall{ID: "live-2", Name: "shell", Arguments: map[string]any{"k": "v"}}
	if !ReplayToolShortCircuit(state, emitterBus, tc, nil) {
		t.Fatal("short-circuit returned false")
	}
	if len(emitted) != 1 || emitted[0].Output != "from-FIFO" {
		t.Errorf("FIFO fallback failed; emitted = %+v", emitted)
	}
}
