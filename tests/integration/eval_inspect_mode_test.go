//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestEvalInspect_Success drives `nexus eval --inspect-mode` end-to-end:
// build the binary, pipe a request JSON to stdin, parse the response from
// stdout, verify the wire format. No API key required — the inline config
// uses nexus.io.test mock_responses to short-circuit the LLM provider.
func TestEvalInspect_Success(t *testing.T) {
	repoRoot := projectFile(t, "")
	binPath := filepath.Join(t.TempDir(), "nexus-inspect")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/nexus")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	configInline := `core:
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
      - content: "inspect-mode answer"
  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"
  nexus.agent.react:
    system_prompt: "Test."
  nexus.memory.capped:
    max_messages: 10
    persist: false
`

	req := map[string]any{
		"schema":        1,
		"config_inline": configInline,
		"user_input":    "hello",
		"metadata": map[string]any{
			"case_id": "inspect-success",
		},
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	cmd := exec.Command(binPath, "eval", "--inspect-mode")
	cmd.Stdin = bytes.NewReader(reqBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = repoRoot
	if err := cmd.Run(); err != nil {
		t.Fatalf("run failed: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	var resp struct {
		Schema                int            `json:"schema"`
		SessionID             string         `json:"session_id"`
		FinalAssistantMessage string         `json:"final_assistant_message"`
		ToolCalls             []any          `json:"tool_calls"`
		Tokens                struct{ Input, Output int } `json:"tokens"`
		LatencyMs             int64          `json:"latency_ms"`
		Metadata              map[string]any `json:"metadata"`
		Error                 *struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	if resp.Schema != 1 {
		t.Errorf("schema=%d want 1", resp.Schema)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
	if resp.SessionID == "" {
		t.Error("session_id empty")
	}
	if resp.FinalAssistantMessage != "inspect-mode answer" {
		t.Errorf("final_assistant_message=%q want %q",
			resp.FinalAssistantMessage, "inspect-mode answer")
	}
	if got := resp.Metadata["case_id"]; got != "inspect-success" {
		t.Errorf("metadata round-trip lost: %v", got)
	}
	if resp.ToolCalls == nil {
		t.Error("tool_calls should be non-nil (empty array ok)")
	}
}

// TestEvalInspect_InvalidRequest checks that a missing user_input surfaces
// as a structured INVALID_REQUEST response with a non-zero exit code, not a
// crash or a freeform stderr-only diagnostic.
func TestEvalInspect_InvalidRequest(t *testing.T) {
	repoRoot := projectFile(t, "")
	binPath := filepath.Join(t.TempDir(), "nexus-inspect")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/nexus")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// Missing user_input — wire-format violation.
	req := map[string]any{
		"schema":        1,
		"config_inline": "core: {}",
	}
	reqBytes, _ := json.Marshal(req)

	cmd := exec.Command(binPath, "eval", "--inspect-mode")
	cmd.Stdin = bytes.NewReader(reqBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = repoRoot
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit on invalid request")
	}

	var resp struct {
		Schema int                              `json:"schema"`
		Error  *struct{ Code, Message string } `json:"error"`
	}
	if jerr := json.Unmarshal(stdout.Bytes(), &resp); jerr != nil {
		t.Fatalf("decode response: %v\nstdout=%s\nstderr=%s", jerr, stdout.String(), stderr.String())
	}
	if resp.Schema != 1 {
		t.Errorf("schema=%d want 1", resp.Schema)
	}
	if resp.Error == nil {
		t.Fatalf("expected error, got nil; stdout=%s", stdout.String())
	}
	if resp.Error.Code != "INVALID_REQUEST" {
		t.Errorf("code=%q want INVALID_REQUEST", resp.Error.Code)
	}
}
