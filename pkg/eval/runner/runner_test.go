package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestRun_ReplaysSyntheticJournal builds a minimal case in-test, hand-crafts
// a 2-turn journal, and confirms the runner replays it deterministically and
// passes the bundled assertions. No API key. <1s.
func TestRun_ReplaysSyntheticJournal(t *testing.T) {
	caseDir := t.TempDir()
	sessionsRoot := filepath.Join(caseDir, "_sessions")
	if err := os.MkdirAll(sessionsRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Hand-craft a journal under <case>/journal/.
	journalDir := filepath.Join(caseDir, "journal")
	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode:  journal.FsyncEveryEvent,
		BufferSize: 16,
		SessionID:  "synthetic",
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	envelopes := []journal.Envelope{
		{Seq: 1, Type: "io.session.start", Payload: map[string]any{"session_id": "synthetic"}},
		{Seq: 2, Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "first message"}},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "llm.response", Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "first reply",
			Model:        "mock",
			FinishReason: "end_turn",
		}},
		{Seq: 5, Type: "agent.turn.end"},
		{Seq: 6, Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "second message"}},
		{Seq: 7, Type: "agent.turn.start"},
		{Seq: 8, Type: "llm.response", Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "second reply",
			Model:        "mock",
			FinishReason: "end_turn",
		}},
		{Seq: 9, Type: "agent.turn.end"},
	}
	for i := range envelopes {
		w.Append(&envelopes[i])
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := w.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("Close journal: %v", err)
	}
	closeCancel()

	// 2. Write minimal config + inputs + assertions.
	if err := os.MkdirAll(filepath.Join(caseDir, "input"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgYAML := fmt.Sprintf(`core:
  log_level: warn
  tick_interval: 1h
  models:
    default: mock
    mock:
      provider: nexus.llm.anthropic
      model: mock
      max_tokens: 1024
  sessions:
    root: %s
    retention: 30d
    id_format: timestamp

plugins:
  active:
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.memory.capped

  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"

  nexus.agent.react:
    system_prompt: "Test."

  nexus.memory.capped:
    max_messages: 10
    persist: false
`, sessionsRoot)
	mustWrite(t, filepath.Join(caseDir, "input", "config.yaml"), cfgYAML)
	mustWrite(t, filepath.Join(caseDir, "input", "inputs.yaml"), `inputs: []`)
	mustWrite(t, filepath.Join(caseDir, "case.yaml"), `name: synthetic
description: synthetic in-test case
tags: [test]
owner: test
freshness_days: 365
model_baseline: mock
`)
	mustWrite(t, filepath.Join(caseDir, "assertions.yaml"), `deterministic:
  - kind: event_emitted
    type: io.input
    count: { min: 2, max: 2 }
  - kind: event_emitted
    type: llm.response
    count: { min: 2, max: 2 }
  - kind: event_count_bounds
    bounds:
      agent.turn.start: { min: 2, max: 2 }
      agent.turn.end:   { min: 2, max: 2 }
`)

	c, err := evalcase.Load(caseDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Run(ctx, c, Options{SessionsRoot: sessionsRoot})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Pass {
		var diag []string
		for _, a := range res.Assertions {
			if !a.Pass {
				diag = append(diag, fmt.Sprintf("%s: %s", a.Kind, a.Message))
			}
		}
		t.Fatalf("expected pass, got fail. failures=%v counts=%v", diag, res.Counts)
	}
	if res.Counts["io.input"] != 2 {
		t.Errorf("io.input count=%d want 2", res.Counts["io.input"])
	}
	if res.Counts["llm.response"] != 2 {
		t.Errorf("llm.response count=%d want 2", res.Counts["llm.response"])
	}
	if len(res.Assertions) != 3 {
		t.Errorf("len(Assertions)=%d want 3", len(res.Assertions))
	}
}

func TestRun_NilCase(t *testing.T) {
	_, err := Run(context.Background(), nil, Options{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOverrideSessionsRoot_PreservesOtherKeys(t *testing.T) {
	in := `core:
  log_level: warn
  sessions:
    root: ~/.nexus/test-sessions
    retention: 30d
plugins:
  active:
    - nexus.io.test
`
	out, err := rewriteCoreSessionsRoot([]byte(in), "/abs/path")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "/abs/path") {
		t.Errorf("missing override: %s", got)
	}
	if !strings.Contains(got, "log_level") {
		t.Errorf("dropped sibling key: %s", got)
	}
	if !strings.Contains(got, "nexus.io.test") {
		t.Errorf("dropped plugins block: %s", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
