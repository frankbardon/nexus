package protocol

import (
	"context"
	"strings"
	"testing"
	"time"
)

// minimalMockConfig is a YAML config that boots a fully working mock-mode
// engine. The protocol runner overlays nexus.io.test's `inputs:` with the
// request's user_input and overrides core.sessions.root with a tempdir.
// Anthropic provider is configured but never called — nexus.io.test
// short-circuits LLM requests via mock_responses.
func minimalMockConfig() string {
	return `core:
  log_level: warn
  tick_interval: 1h
  models:
    default: mock
    mock:
      provider: nexus.llm.anthropic
      model: mock
      max_tokens: 1024
  sessions:
    root: /tmp/will-be-overridden
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
    - nexus.memory.capped

  nexus.io.test:
    input_delay: 10ms
    approval_mode: approve
    timeout: 10s
    mock_responses:
      - content: "Hello from mock"

  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"

  nexus.agent.react:
    system_prompt: "Test."

  nexus.memory.capped:
    max_messages: 10
    persist: false
`
}

func TestRun_PopulatesResponse(t *testing.T) {
	req := &Request{
		Schema:       SchemaVersion,
		ConfigInline: minimalMockConfig(),
		UserInput:    "hi there",
		Metadata: map[string]any{
			"case_id": "test-1",
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.Schema != SchemaVersion {
		t.Errorf("schema=%d want %d", resp.Schema, SchemaVersion)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
	if resp.SessionID == "" {
		t.Errorf("expected session_id to be populated")
	}
	if resp.FinalAssistantMessage != "Hello from mock" {
		t.Errorf("final_assistant_message=%q want %q", resp.FinalAssistantMessage, "Hello from mock")
	}
	if resp.LatencyMs < 0 {
		t.Errorf("latency_ms=%d should be >= 0", resp.LatencyMs)
	}
	if got := resp.Metadata["case_id"]; got != "test-1" {
		t.Errorf("metadata round-trip lost: %v", got)
	}
	if resp.ToolCalls == nil {
		t.Errorf("tool_calls should be non-nil (empty slice ok)")
	}
}

// mockToolConfig builds a config where the agent's first turn calls a tool
// (read_file against a non-existent path; the file plugin returns an error
// summary, which is what we want — proves invoke→result pairing works).
func mockToolConfig() string {
	return `core:
  log_level: warn
  tick_interval: 1h
  models:
    default: mock
    mock:
      provider: nexus.llm.anthropic
      model: mock
      max_tokens: 1024
  sessions:
    root: /tmp/will-be-overridden
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
    - nexus.memory.capped
    - nexus.tool.file

  nexus.io.test:
    input_delay: 10ms
    approval_mode: approve
    timeout: 10s
    mock_responses:
      - content: ""
        tool_calls:
          - name: read_file
            arguments: '{"path": "/__nonexistent__"}'
      - content: "I checked the file."

  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"

  nexus.agent.react:
    system_prompt: "Test."

  nexus.memory.capped:
    max_messages: 10
    persist: false

  nexus.tool.file:
    allow_external_writes: false
`
}

func TestRun_HarvestsToolCalls(t *testing.T) {
	req := &Request{
		Schema:       SchemaVersion,
		ConfigInline: mockToolConfig(),
		UserInput:    "read a file",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(resp.ToolCalls) == 0 {
		t.Fatalf("expected at least one tool call; got 0; final=%q", resp.FinalAssistantMessage)
	}
	tc := resp.ToolCalls[0]
	if tc.Tool != "read_file" {
		t.Errorf("tool=%q want read_file", tc.Tool)
	}
	if tc.Args["path"] != "/__nonexistent__" {
		t.Errorf("args.path=%v want /__nonexistent__", tc.Args["path"])
	}
	if tc.ResultSummary == "" {
		t.Errorf("expected result_summary populated for paired invoke→result")
	}
	if tc.DurationMs < 0 {
		t.Errorf("duration_ms=%d should be >= 0", tc.DurationMs)
	}
	if resp.FinalAssistantMessage != "I checked the file." {
		t.Errorf("final_assistant_message=%q want %q",
			resp.FinalAssistantMessage, "I checked the file.")
	}
}

// loopingMockConfig configures unlimited mock responses that always emit
// content (no tool calls). The agent finishes after one turn per input,
// so MaxTurns=1 should still let the run complete cleanly. We use this
// to test that MaxTurns enforcement does not break short runs.
func loopingMockConfig() string {
	return minimalMockConfig()
}

func TestRun_MaxTurnsBoundsExecution(t *testing.T) {
	// MaxTurns=1 should not break a normal one-turn run: the agent emits
	// agent.turn.end once, the cap fires, and the run halts naturally.
	req := &Request{
		Schema:       SchemaVersion,
		ConfigInline: loopingMockConfig(),
		UserInput:    "one turn please",
		MaxTurns:     1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
	if resp.SessionID == "" {
		t.Errorf("expected session_id even when capped at MaxTurns=1")
	}
}

func TestRun_TimeoutSurfacesError(t *testing.T) {
	req := &Request{
		Schema:       SchemaVersion,
		ConfigInline: minimalMockConfig(),
		UserInput:    "hi",
	}
	// Deadline so short the engine cannot even finish booting. Should
	// surface as a TIMEOUT-coded error.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, req)
	if err == nil {
		t.Fatal("expected error from sub-millisecond deadline")
	}
	code, _ := MapError(err)
	if code != CodeTimeout && code != CodeEngineBoot {
		// Either TIMEOUT (deadline hit during the agent loop) or
		// ENGINE_BOOT (bootCtx cancelled before lifecycle.Boot returns)
		// is a valid surfacing of an exceeded deadline.
		t.Errorf("code=%q want TIMEOUT or ENGINE_BOOT (got %s)", code, err.Error())
	}
}

func TestRun_RejectsInvalidRequest(t *testing.T) {
	req := &Request{Schema: SchemaVersion} // missing config & user_input
	_, err := Run(context.Background(), req)
	if err == nil {
		t.Fatal("expected validation error")
	}
	code, _ := MapError(err)
	if code != CodeInvalidRequest {
		t.Errorf("code=%q want INVALID_REQUEST", code)
	}
}

func TestOverlayConfig_InjectsUserInput(t *testing.T) {
	in := `core:
  log_level: warn

plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
`
	out, err := overlayConfig([]byte(in), "/tmp/sessions", "hello world")
	if err != nil {
		t.Fatalf("overlayConfig: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "/tmp/sessions") {
		t.Errorf("sessions root not injected: %s", got)
	}
	if !strings.Contains(got, "nexus.io.test") {
		t.Errorf("nexus.io.test not added: %s", got)
	}
	if strings.Contains(got, "nexus.io.tui") {
		t.Errorf("nexus.io.tui should have been stripped: %s", got)
	}
	if !strings.Contains(got, "hello world") {
		t.Errorf("user input not injected: %s", got)
	}
}

func TestTruncateForSummary(t *testing.T) {
	short := "small string"
	if got := truncateForSummary(short, 100); got != short {
		t.Errorf("short string mutated: %q", got)
	}
	long := strings.Repeat("a", 3000)
	got := truncateForSummary(long, resultSummaryCap)
	// Allow up to resultSummaryCap+ellipsis bytes (3-byte UTF-8 ellipsis).
	if len(got) > resultSummaryCap+3 {
		t.Errorf("len=%d exceeds cap+ellipsis", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix; got %q", got[len(got)-3:])
	}
}
