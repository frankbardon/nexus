package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
)

// liveCaseConfig is a mock-mode config for RunLive: like the replay-only
// cases under tests/eval/cases, plugins.active has no IO transport — that's
// what overlayLiveInputs is for, it adds nexus.io.test itself. The
// nexus.io.test config block is authored here (mock_responses so no real
// LLM/API key is needed, matching pkg/eval/protocol's own test pattern —
// see protocol/runner_test.go's minimalMockConfig) even though the id isn't
// (yet) in plugins.active; overlayLiveInputs only edits `active` +
// `inputs`, leaving the rest of the block alone.
func liveCaseConfig(sessionsRoot, mockContent string) string {
	return fmt.Sprintf(`core:
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

journal:
  fsync: none
  retain_days: 30
  rotate_size_mb: 4

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

  nexus.io.test:
    input_delay: 10ms
    approval_mode: approve
    timeout: 10s
    mock_responses:
      - content: %q
`, sessionsRoot, mockContent)
}

// buildLiveCase writes a self-contained case bundle for RunLive: config
// (via liveCaseConfig), a single scripted input, and a golden journal
// containing goldenEnvelopes (whatever a drift test needs it to contain —
// event_emitted/event_count_bounds assertions never read golden at all, so
// a bare-minimum golden is fine when a test only needs those kinds).
func buildLiveCase(t *testing.T, mockContent string, goldenEnvelopes []journal.Envelope, assertionsYAML string) (*evalcase.Case, string) {
	t.Helper()
	caseDir := t.TempDir()
	sessionsRoot := filepath.Join(caseDir, "_sessions")
	if err := os.MkdirAll(sessionsRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	journalDir := filepath.Join(caseDir, "journal")
	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode:  journal.FsyncEveryEvent,
		BufferSize: 16,
		SessionID:  "golden",
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i := range goldenEnvelopes {
		w.Append(&goldenEnvelopes[i])
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := w.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("Close journal: %v", err)
	}
	closeCancel()

	if err := os.MkdirAll(filepath.Join(caseDir, "input"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(caseDir, "input", "config.yaml"), liveCaseConfig(sessionsRoot, mockContent))
	mustWrite(t, filepath.Join(caseDir, "input", "inputs.yaml"), `inputs:
  - "hi there"
`)
	mustWrite(t, filepath.Join(caseDir, "case.yaml"), `name: live-case
description: synthetic in-test case for RunLive
tags: [test]
owner: test
freshness_days: 365
model_baseline: mock
`)
	mustWrite(t, filepath.Join(caseDir, "assertions.yaml"), assertionsYAML)

	c, err := evalcase.Load(caseDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c, sessionsRoot
}

// TestRunLive_ProducesWellFormedResult drives a live-callable case (a real
// engine boot, mock LLM responses via nexus.io.test's mock_responses —
// never engine.Replay/a journal stash) and confirms RunLive produces a
// well-formed Result: a live-observed io.input and llm.response actually
// happened, the case's own deterministic assertions (event_emitted,
// event_count_bounds — both golden-independent) pass, and the result
// carries the expected identity fields. Nothing in this path touches
// engine.Replay or journal.NewCoordinator — proves RunLive is independent
// of the replay/stash path Run uses.
func TestRunLive_ProducesWellFormedResult(t *testing.T) {
	assertionsYAML := `deterministic:
  - kind: event_emitted
    type: io.input
    count: { min: 1, max: 1 }
  - kind: event_emitted
    type: llm.response
    count: { min: 1 }
  - kind: event_count_bounds
    bounds:
      agent.turn.start: { min: 1, max: 1 }
      agent.turn.end:   { min: 1, max: 1 }
`
	// A minimal, otherwise-irrelevant golden journal: the assertions above
	// never read it (see doc comment on buildLiveCase).
	golden := []journal.Envelope{
		{Seq: 1, Type: "io.session.start", Payload: map[string]any{"session_id": "golden"}},
	}
	c, sessionsRoot := buildLiveCase(t, "Hello from live mock", golden, assertionsYAML)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := RunLive(ctx, c, Options{SessionsRoot: sessionsRoot})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if res.CaseID != c.ID {
		t.Errorf("CaseID=%q want %q", res.CaseID, c.ID)
	}
	if res.JournalDir != c.JournalDir {
		t.Errorf("JournalDir=%q want %q", res.JournalDir, c.JournalDir)
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
	if res.Counts["io.input"] != 1 {
		t.Errorf("io.input count=%d want 1", res.Counts["io.input"])
	}
	if res.Counts["llm.response"] != 1 {
		t.Errorf("llm.response count=%d want 1", res.Counts["llm.response"])
	}
	if res.StartedAt.IsZero() || res.EndedAt.IsZero() {
		t.Errorf("expected non-zero StartedAt/EndedAt, got %v / %v", res.StartedAt, res.EndedAt)
	}
}

// TestRunLive_ToolInvocationDriftFailsAssertions proves the story's central
// claim: even though RunLive never replays a stash, it still evaluates
// Assertions.Deterministic against (live-observed, golden), so a live run
// whose tool-call sequence diverges from golden still fails
// ToolInvocationParity (and EventSequenceDistance). The golden journal
// records a read_file tool.invoke that this case's live run — mock
// LLM response with no ToolCalls — will never produce.
func TestRunLive_ToolInvocationDriftFailsAssertions(t *testing.T) {
	assertionsYAML := `deterministic:
  - kind: tool_invocation_parity
    count_tolerance: 0
    arg_keys: false
  - kind: event_sequence_distance
    threshold: 0.0
    filter:
      - io.input
      - agent.turn.start
      - agent.turn.end
      - llm.response
      - tool.invoke
      - tool.result
`
	golden := []journal.Envelope{
		{Seq: 1, Type: "io.session.start", Payload: map[string]any{"session_id": "golden"}},
		{Seq: 2, Type: "io.input", Payload: map[string]any{"content": "hi there"}},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "tool.invoke", Payload: map[string]any{"name": "read_file", "id": "golden_tc_0"}},
		{Seq: 5, Type: "tool.result", Payload: map[string]any{"id": "golden_tc_0", "name": "read_file"}},
		{Seq: 6, Type: "llm.response", Payload: map[string]any{"content": "golden reply"}},
		{Seq: 7, Type: "agent.turn.end"},
	}
	c, sessionsRoot := buildLiveCase(t, "live reply, no tool call", golden, assertionsYAML)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := RunLive(ctx, c, Options{SessionsRoot: sessionsRoot})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if res.Pass {
		t.Fatalf("expected fail due to tool-invocation drift, got pass. counts=%v", res.Counts)
	}

	var sawParityFail, sawDistanceFail bool
	for _, a := range res.Assertions {
		switch a.Kind {
		case "tool_invocation_parity":
			if !a.Pass {
				sawParityFail = true
			}
		case "event_sequence_distance":
			if !a.Pass {
				sawDistanceFail = true
			}
		}
	}
	if !sawParityFail {
		t.Errorf("expected tool_invocation_parity to fail on live/golden drift; assertions=%+v", res.Assertions)
	}
	if !sawDistanceFail {
		t.Errorf("expected event_sequence_distance to fail on live/golden drift; assertions=%+v", res.Assertions)
	}
}

// TestRunLive_NilCase mirrors TestRun_NilCase — same guard, same error
// shape, on the live path.
func TestRunLive_NilCase(t *testing.T) {
	_, err := RunLive(context.Background(), nil, Options{})
	if err == nil {
		t.Fatal("expected error")
	}
}

// liveCaseConfigWithExtra is liveCaseConfig plus one extra plugin id in
// plugins.active — used to prove Options.ExtraPlugins registers and the
// engine actually Inits it on the live path, the same proof
// TestRun_ExtraPluginBootsAndReplays makes for the replay path.
func liveCaseConfigWithExtra(sessionsRoot, mockContent, extraID string) string {
	return fmt.Sprintf(`core:
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

journal:
  fsync: none
  retain_days: 30
  rotate_size_mb: 4

plugins:
  active:
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.memory.capped
    - %s

  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"

  nexus.agent.react:
    system_prompt: "Test."

  nexus.memory.capped:
    max_messages: 10
    persist: false

  nexus.io.test:
    input_delay: 10ms
    approval_mode: approve
    timeout: 10s
    mock_responses:
      - content: %q
`, sessionsRoot, extraID, mockContent)
}

// TestRunLive_AppliesExtraPlugins mirrors TestRun_ExtraPluginBootsAndReplays
// on the live path: an embedder-authored plugin registered via
// Options.ExtraPlugins, named in plugins.active, boots and Inits under
// RunLive exactly as it does under Run — proving ExtraPlugins is applied
// identically on both paths.
func TestRunLive_AppliesExtraPlugins(t *testing.T) {
	const extraID = "acme.tool.echo"
	caseDir := t.TempDir()
	sessionsRoot := filepath.Join(caseDir, "_sessions")
	if err := os.MkdirAll(sessionsRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	journalDir := filepath.Join(caseDir, "journal")
	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode:  journal.FsyncEveryEvent,
		BufferSize: 16,
		SessionID:  "golden",
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	golden := journal.Envelope{Seq: 1, Type: "io.session.start", Payload: map[string]any{"session_id": "golden"}}
	w.Append(&golden)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := w.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("Close journal: %v", err)
	}
	closeCancel()

	if err := os.MkdirAll(filepath.Join(caseDir, "input"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(caseDir, "input", "config.yaml"), liveCaseConfigWithExtra(sessionsRoot, "hi from mock", extraID))
	mustWrite(t, filepath.Join(caseDir, "input", "inputs.yaml"), `inputs:
  - "hi there"
`)
	mustWrite(t, filepath.Join(caseDir, "case.yaml"), `name: live-case-extra
description: synthetic in-test case for RunLive with an embedder-authored plugin
tags: [test]
owner: test
freshness_days: 365
model_baseline: mock
`)
	mustWrite(t, filepath.Join(caseDir, "assertions.yaml"), `deterministic:
  - kind: event_emitted
    type: io.input
    count: { min: 1, max: 1 }
`)

	c, err := evalcase.Load(caseDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	fake := &fakeExtraPlugin{id: extraID}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := RunLive(ctx, c, Options{
		SessionsRoot: sessionsRoot,
		ExtraPlugins: map[string]engine.PluginFactory{
			extraID: func() engine.Plugin { return fake },
		},
	})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if !res.Pass {
		t.Fatalf("expected pass, got fail. assertions=%+v counts=%v", res.Assertions, res.Counts)
	}
	if !fake.initCalled {
		t.Error("expected ExtraPlugins-registered plugin to be Init'd by the engine")
	}
}
