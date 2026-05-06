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
	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/tools/shell"
)

// TestJournalReplay_ToolShortCircuit hand-crafts a journal containing a
// tool.invoke that the live shell tool would normally execute, plus a
// tool.result the replay should serve from stash. Asserts:
//
//  1. shell.LiveCalls() stays at 0 — the actual shell.Run path never fires.
//  2. The replayed tool.result lands on the bus with the live invoke's
//     correlation ID re-stamped on top of the journaled payload.
//  3. The output content matches what was journaled.
func TestJournalReplay_ToolShortCircuit(t *testing.T) {
	sessionsRoot := t.TempDir()
	sourceID := "src-tool-replay"
	sourceDir := filepath.Join(sessionsRoot, sourceID)

	if err := os.MkdirAll(filepath.Join(sourceDir, "metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	journalDir := filepath.Join(sourceDir, "journal")
	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode:  journal.FsyncEveryEvent,
		BufferSize: 16,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// One turn: io.input -> agent emits tool.invoke (we'll fake this in the
	// test by emitting tool.invoke ourselves) -> shell would respond with
	// tool.result. Coordinator stashes the journaled tool.result; live
	// emit of tool.invoke triggers shell's short-circuit.
	envelopes := []journal.Envelope{
		{Seq: 1, Type: "io.session.start"},
		{Seq: 2, Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "list files"}},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "call-1",
			Name:      "shell",
			Arguments: map[string]any{"command": "ls"},
		}},
		{Seq: 5, Type: "tool.result", Payload: events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "call-1",
			Name:   "shell",
			Output: "journaled output: foo bar baz",
		}},
		{Seq: 6, Type: "agent.turn.end"},
	}
	for i := range envelopes {
		w.Append(&envelopes[i])
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := w.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Config: shell tool active, no real agent. The test drives tool.invoke
	// directly to isolate the shell's short-circuit path.
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
  active:
    - nexus.tool.shell

  nexus.tool.shell:
    allowed_commands: [ls]
    timeout: 5s
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

	// Collect tool.result emits.
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

	// Activate replay state and seed the queue manually — the coordinator's
	// io.input drive isn't useful here (no agent loop), but the stash + the
	// shell short-circuit are.
	eng.Replay.SetActive(true)
	eng.Replay.Push("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "ignored-by-handler",
		Name:   "shell",
		Output: "journaled output: foo bar baz",
	})

	// Live tool.invoke — this would normally exec `ls`. With replay active,
	// shell's handler must short-circuit, pop the stash, and emit the
	// journaled result.
	if err := eng.Bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "live-call-id",
		Name:      "shell",
		Arguments: map[string]any{"command": "ls"},
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	eng.Replay.SetActive(false)

	// Locate shell plugin.
	var shellPlugin *shell.Plugin
	for _, p := range eng.Lifecycle.Plugins() {
		if p.ID() == "nexus.tool.shell" {
			if sp, ok := p.(*shell.Plugin); ok {
				shellPlugin = sp
			}
		}
	}
	if shellPlugin == nil {
		t.Fatal("shell plugin not found")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = eng.Stop(stopCtx)

	if got := shellPlugin.LiveCalls(); got != 0 {
		t.Errorf("shell.LiveCalls() = %d, want 0 — short-circuit failed", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) == 0 {
		t.Fatal("no tool.result emitted; replay short-circuit silently dropped invoke")
	}

	// First result must be the replayed one. ID must be the LIVE invoke's ID,
	// not the stashed payload's — agent correlates by ID.
	got := results[0]
	if got.ID != "live-call-id" {
		t.Errorf("tool.result.ID = %q, want %q (live invoke ID must win)", got.ID, "live-call-id")
	}
	if got.Output != "journaled output: foo bar baz" {
		t.Errorf("tool.result.Output = %q, want journaled content", got.Output)
	}
}
