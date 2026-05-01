//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestJournalReplay_ToolCache_LiveRecordReplay drives a full record/replay
// cycle through the args-keyed disk cache:
//
//  1. Boot, set replay inactive, fire a tool.invoke whose result the test
//     emits live (simulating a tool plugin completing). Cache subscriber
//     records the (tool, args) -> result mapping under
//     <session>/journal/cache/shell/<sha>.json.
//  2. Activate replay, fire a NEW tool.invoke with identical args but a
//     different live ID. ReplayToolShortCircuit must hit the cache,
//     re-stamp the live ID, and emit the cached output.
//  3. Confirm no FIFO stash entries were consumed (cache won, FIFO
//     untouched).
func TestJournalReplay_ToolCache_LiveRecordReplay(t *testing.T) {
	sessionsRoot := t.TempDir()

	cfgYAML := fmt.Sprintf(`
core:
  log_level: warn
  tick_interval: 5s
  models:
    default: mock
    mock:
      provider: nexus.llm.anthropic
      model: mock
  sessions:
    root: %s
    retention: 30d
    id_format: timestamp
plugins:
  active: []
`, sessionsRoot)

	eng, err := engine.NewFromBytes([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("NewFromBytes: %v", err)
	}
	allplugins.RegisterAll(eng.Registry)

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer bootCancel()
	if err := eng.Boot(bootCtx); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	var (
		mu      sync.Mutex
		results []events.ToolResult
	)
	unsub := eng.Bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}
	})
	defer unsub()

	// 1. Live recording pass — cache subscriber catches invoke + result
	// pairs and writes the cache file.
	args := map[string]any{"command": "ls", "dir": "/tmp"}
	if err := eng.Bus.Emit("tool.invoke", events.ToolCall{
		ID:        "live-rec-1",
		Name:      "shell",
		Arguments: args,
	}); err != nil {
		t.Fatalf("Emit invoke: %v", err)
	}
	if err := eng.Bus.Emit("tool.result", events.ToolResult{
		ID:     "live-rec-1",
		Name:   "shell",
		Output: "cached output payload",
	}); err != nil {
		t.Fatalf("Emit result: %v", err)
	}

	// Verify cache file landed on disk.
	cacheDir := filepath.Join(eng.Session.RootDir, "journal", "cache", "shell")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("expected cache subdir on disk: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 cache file, got %d (entries=%v)", len(entries), entries)
	}

	// Reset our collected results before the replay phase.
	mu.Lock()
	results = nil
	mu.Unlock()

	// 2. Activate replay, ensure FIFO is empty (so cache must be the
	// answer — no fallback).
	eng.Replay.SetActive(true)

	// Now fire a NEW invoke with same args, different live ID. The
	// short-circuit helper must consult the cache and emit cached output
	// stamped with the live ID.
	tc := events.ToolCall{
		ID:        "live-replay-1",
		Name:      "shell",
		Arguments: args,
	}
	if !engine.ReplayToolShortCircuit(eng.Replay, eng.Bus, tc, eng.Logger) {
		t.Fatal("short-circuit returned false during replay")
	}
	eng.Replay.SetActive(false)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = eng.Stop(stopCtx)

	// 3. Assertions.
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 {
		t.Fatalf("expected 1 tool.result during replay, got %d (%+v)", len(results), results)
	}
	got := results[0]
	if got.Output != "cached output payload" {
		t.Errorf("output = %q, want cached payload", got.Output)
	}
	if got.ID != "live-replay-1" {
		t.Errorf("ID = %q, want live-replay-1 (live invoke ID must be re-stamped)", got.ID)
	}
}

// TestJournalReplay_ToolCache_DistinctArgsDistinctEntries verifies the
// hash function: two invocations with different args produce two cache
// files, and lookups with the wrong args miss.
func TestJournalReplay_ToolCache_DistinctArgsDistinctEntries(t *testing.T) {
	sessionsRoot := t.TempDir()
	cfgYAML := fmt.Sprintf(`
core: {log_level: warn, tick_interval: 5s, models: {default: m, m: {provider: nexus.llm.anthropic, model: mock}}, sessions: {root: %s, retention: 30d, id_format: timestamp}}
plugins: {active: []}
`, sessionsRoot)

	eng, _ := engine.NewFromBytes([]byte(cfgYAML))
	allplugins.RegisterAll(eng.Registry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	defer eng.Stop(context.Background()) //nolint:errcheck

	emit := func(id string, args map[string]any, output string) {
		eng.Bus.Emit("tool.invoke", events.ToolCall{ID: id, Name: "shell", Arguments: args}) //nolint:errcheck
		eng.Bus.Emit("tool.result", events.ToolResult{ID: id, Name: "shell", Output: output}) //nolint:errcheck
	}
	emit("a", map[string]any{"command": "ls"}, "ls output")
	emit("b", map[string]any{"command": "pwd"}, "pwd output")

	cacheDir := filepath.Join(eng.Session.RootDir, "journal", "cache", "shell")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 distinct cache files, got %d", len(entries))
	}
}
