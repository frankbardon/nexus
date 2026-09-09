package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"github.com/frankbardon/nexus/pkg/eval/runner"
)

// TestE2E_RunLiveTranscriptFeedsJudge is the whole point of this effort,
// proven end to end: runner.RunLive produces a rendered Result.Transcript
// (E2-S2); that transcript feeds protocol.Request.UserInput alongside a
// rubric in a judge config.yaml naming a cheap/mock model role and a real
// structured-output schema (nexus.gate.json_schema — the same belt-and-
// suspenders pattern documented at docs/src/guides/structured-output.md,
// scenario 5); protocol.Run (real, unmodified) returns a structured JSON
// verdict via the EXISTING, UNMODIFIED Response.FinalAssistantMessage
// field; and this test parses it.
//
// Both the subject run and the judge run are driven entirely by
// nexus.io.test's mock_responses (see plugins/io/test/plugin.go) — no real
// LLM call, no API key, no network access anywhere in this test.
//
// Critically, this test adds no field to protocol.Request or
// protocol.Response: the judge's rubric and structured-output schema live
// entirely in the judge's own config.yaml (UserInput carries the rubric +
// transcript as plain text; the schema is config, not wire format). If
// this test needs a new Request/Response field to pass, that would mean a
// resolved design decision from this effort's own interview was wrong —
// see E2-S3.md's own explicit warning.
func TestE2E_RunLiveTranscriptFeedsJudge(t *testing.T) {
	// -- Step 1: RunLive a small fixture case, get its rendered transcript. --
	c, subjectSessionsRoot := buildJudgeSubjectCase(t, "The capital of France is Paris.")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	subjectRes, err := runner.RunLive(ctx, c, runner.Options{SessionsRoot: subjectSessionsRoot})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if subjectRes.Transcript == "" {
		t.Fatal("expected RunLive to produce a non-empty Transcript")
	}

	// -- Step 2: build a judge protocol.Request: a rubric + the transcript --
	// as UserInput, against a config naming a cheap/mock model role and a
	// structured-output schema ({"score": number, "reasoning": string}).
	const rubric = "You are grading an AI assistant's transcript against a rubric: " +
		"did the assistant correctly answer the user's geography question? " +
		"Respond with ONLY a JSON object matching the required schema."

	const mockVerdictJSON = `{"score": 9, "reasoning": "The assistant correctly identified Paris as the capital of France."}`

	judgeSessionsRoot := t.TempDir()
	req := &Request{
		Schema:       SchemaVersion,
		ConfigInline: judgeConfig(judgeSessionsRoot, mockVerdictJSON),
		UserInput:    rubric + "\n\n" + subjectRes.Transcript,
		Metadata: map[string]any{
			"judged_case_id": c.ID,
		},
	}

	// -- Step 3: run the judge through the real, unmodified protocol.Run, --
	// with the judge's own mocked/scripted provider response (nexus.io.test
	// mock_responses) returning valid JSON matching the schema.
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("Run (judge): %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("judge run returned an error response: %+v", resp.Error)
	}

	// -- Step 4: FinalAssistantMessage parses as valid JSON matching the --
	// judge schema shape.
	var verdict struct {
		Score     float64 `json:"score"`
		Reasoning string  `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(resp.FinalAssistantMessage), &verdict); err != nil {
		t.Fatalf("FinalAssistantMessage did not parse as the judge schema: %v\nraw: %q",
			err, resp.FinalAssistantMessage)
	}
	if verdict.Score != 9 {
		t.Errorf("verdict.Score=%v want 9", verdict.Score)
	}
	if verdict.Reasoning == "" {
		t.Errorf("verdict.Reasoning is empty; expected a non-empty rationale")
	}

	// Round-trip sanity: Metadata still flows through unmodified.
	if got := resp.Metadata["judged_case_id"]; got != c.ID {
		t.Errorf("metadata round-trip lost judged_case_id: %v", got)
	}
}

// buildJudgeSubjectCase writes a minimal, self-contained case bundle for
// runner.RunLive: a mock-mode config (no real LLM call, matching
// pkg/eval/protocol's own test pattern — see runner_test.go's
// minimalMockConfig), a single scripted input, and a bare golden journal
// (the case carries no deterministic assertions, so the golden journal's
// content is irrelevant — only its presence is required for evalcase.Load
// to succeed).
func buildJudgeSubjectCase(t *testing.T, mockContent string) (*evalcase.Case, string) {
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
	mustWriteFile(t, filepath.Join(caseDir, "input", "config.yaml"), fmt.Sprintf(`core:
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
`, sessionsRoot, mockContent))
	mustWriteFile(t, filepath.Join(caseDir, "input", "inputs.yaml"), `inputs:
  - "What is the capital of France?"
`)
	mustWriteFile(t, filepath.Join(caseDir, "case.yaml"), `name: judge-subject
description: synthetic in-test case whose live transcript feeds a judge (E2-S3)
tags: [test]
owner: test
freshness_days: 365
model_baseline: mock
`)
	mustWriteFile(t, filepath.Join(caseDir, "assertions.yaml"), `deterministic: []
`)

	c, err := evalcase.Load(caseDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c, sessionsRoot
}

// judgeConfig builds the judge's own inline config: a cheap/mock model
// role (mocked entirely via nexus.io.test, so no API key is required) plus
// a real structured-output schema enforced by nexus.gate.json_schema — the
// same "schema declaration as config, validated as a safety net" pattern
// documented at docs/src/guides/structured-output.md (scenario 5,
// "Belt-and-Suspenders with json_schema Gate"). mockVerdictJSON is the
// scripted LLM response text; it must already be valid JSON matching the
// schema below so the gate's single pass validates it without a retry.
func judgeConfig(sessionsRoot, mockVerdictJSON string) string {
	return fmt.Sprintf(`core:
  log_level: warn
  tick_interval: 1h
  models:
    default: judge
    judge:
      provider: nexus.llm.anthropic
      model: mock-judge
      max_tokens: 512
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
    - nexus.io.test
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.gate.json_schema
    - nexus.memory.capped

  nexus.io.test:
    input_delay: 10ms
    approval_mode: approve
    timeout: 10s
    mock_responses:
      - content: %q

  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"

  nexus.agent.react:
    system_prompt: "You are a judge. Score the transcript against the rubric. Respond with ONLY the required JSON."

  nexus.gate.json_schema:
    schema: '{"type":"object","required":["score","reasoning"],"properties":{"score":{"type":"number"},"reasoning":{"type":"string"}}}'
    max_retries: 0

  nexus.memory.capped:
    max_messages: 10
    persist: false
`, sessionsRoot, mockVerdictJSON)
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
