//go:build integration

package integration

import (
	"context"
	"encoding/json"
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
)

// TestJournalCrashResume_RefiresPartialInput simulates a session that
// crashed mid-turn: the journal ends with agent.turn.start and a
// tool.invoke but no tool.result and no agent.turn.end. Booting with
// recall must detect the partial turn and re-emit the io.input that
// started it so the live ReAct loop restarts.
//
// Asserts:
//
//  1. After Boot, an io.input matching the partial turn's content lands
//     on the bus (the crash-resume hook fired).
//  2. ReplayState was NOT activated — the partial turn must run live, not
//     consume from a stash.
func TestJournalCrashResume_RefiresPartialInput(t *testing.T) {
	sessionsRoot := t.TempDir()
	sessionID := "crash-recovered-session"
	sessionDir := filepath.Join(sessionsRoot, sessionID)

	// Hand-build the session workspace the engine's recall path expects.
	if err := os.MkdirAll(filepath.Join(sessionDir, "context"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sessionDir, "metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sessionDir, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sessionDir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Minimal session.json so LoadSessionWorkspace + SessionMetadata work.
	meta := map[string]any{
		"id":         sessionID,
		"started_at": time.Now().Format(time.RFC3339Nano),
		"plugins":    []string{},
		"labels":     map[string]string{},
		"status":     "completed", // simulates a previously-ended (crashed) session
	}
	metaData, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(sessionDir, "metadata", "session.json"), metaData, 0o644); err != nil {
		t.Fatal(err)
	}

	// Hand-craft a journal: one completed turn, then a partial turn.
	journalDir := filepath.Join(sessionDir, "journal")
	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode:  journal.FsyncEveryEvent,
		BufferSize: 16,
		SessionID:  sessionID,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	envelopes := []journal.Envelope{
		{Seq: 1, Type: "io.session.start"},
		{Seq: 2, Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "first message"}},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "llm.response", Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "first reply", FinishReason: "end_turn"}},
		{Seq: 5, Type: "agent.turn.end"},
		// crash mid-turn — partial turn below
		{Seq: 6, Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "ASK PARTIAL TURN"}},
		{Seq: 7, Type: "agent.turn.start"},
		{Seq: 8, Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "stuck", Name: "shell", Arguments: map[string]any{"command": "ls"}}},
		// no tool.result, no agent.turn.end
	}
	for i := range envelopes {
		w.Append(&envelopes[i])
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := w.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Config: minimal — just a stub plugin set so Boot succeeds. We do not
	// activate the agent or shell here; the goal is to assert the engine's
	// crash-resume hook fires the io.input. A subscription on the bus
	// captures the re-emit.
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
	eng.RecallSessionID = sessionID

	// Capture io.input emits before Boot so we see the crash-resume one.
	var (
		mu     sync.Mutex
		inputs []events.UserInput
	)
	preBootSub := eng.Bus.Subscribe("io.input", func(ev engine.Event[any]) {
		if u, ok := ev.Payload.(events.UserInput); ok {
			mu.Lock()
			inputs = append(inputs, u)
			mu.Unlock()
		}
	})
	defer preBootSub()

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer bootCancel()
	if err := eng.Boot(bootCtx); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = eng.Stop(stopCtx)

	// 1. Crash-resume hook re-emitted the partial turn's io.input.
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, in := range inputs {
		if in.Content == "ASK PARTIAL TURN" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected re-emitted io.input %q, collected = %+v", "ASK PARTIAL TURN", inputs)
	}

	// 2. ReplayState must NOT be active after Boot — the partial turn runs
	// live, not via stash.
	if eng.Replay.Active() {
		t.Error("ReplayState still active after crash resume — partial turn would consume from a non-existent stash")
	}
}

// TestJournalCrashResume_NoPartialIsNoop verifies that a clean recall
// (journal ends with agent.turn.end) does not re-emit any io.input.
func TestJournalCrashResume_NoPartialIsNoop(t *testing.T) {
	sessionsRoot := t.TempDir()
	sessionID := "clean-recalled-session"
	sessionDir := filepath.Join(sessionsRoot, sessionID)

	for _, sub := range []string{"context", "metadata", "files", "plugins"} {
		_ = os.MkdirAll(filepath.Join(sessionDir, sub), 0o755)
	}
	meta := map[string]any{
		"id":         sessionID,
		"started_at": time.Now().Format(time.RFC3339Nano),
		"plugins":    []string{},
		"labels":     map[string]string{},
		"status":     "completed",
	}
	metaData, _ := json.Marshal(meta)
	_ = os.WriteFile(filepath.Join(sessionDir, "metadata", "session.json"), metaData, 0o644)

	journalDir := filepath.Join(sessionDir, "journal")
	w, _ := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode: journal.FsyncEveryEvent, BufferSize: 16, SessionID: sessionID,
	})
	w.Append(&journal.Envelope{Seq: 1, Type: "io.session.start"})
	w.Append(&journal.Envelope{Seq: 2, Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "complete"}})
	w.Append(&journal.Envelope{Seq: 3, Type: "agent.turn.start"})
	w.Append(&journal.Envelope{Seq: 4, Type: "agent.turn.end"})
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	_ = w.Close(closeCtx)

	cfgYAML := fmt.Sprintf(`
core:
  log_level: warn
  tick_interval: 5s
  models:
    default: mock
    mock:
      provider: nexus.llm.anthropic
      model: mock
  sessions: {root: %s, retention: 30d, id_format: timestamp}
plugins: {active: []}
`, sessionsRoot)

	eng, _ := engine.NewFromBytes([]byte(cfgYAML))
	allplugins.RegisterAll(eng.Registry)
	eng.RecallSessionID = sessionID

	var (
		mu     sync.Mutex
		inputs int
	)
	unsub := eng.Bus.Subscribe("io.input", func(_ engine.Event[any]) {
		mu.Lock()
		inputs++
		mu.Unlock()
	})
	defer unsub()

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer bootCancel()
	if err := eng.Boot(bootCtx); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = eng.Stop(stopCtx)

	mu.Lock()
	defer mu.Unlock()
	if inputs > 0 {
		t.Errorf("clean recall should not re-emit any io.input; got %d", inputs)
	}
}
