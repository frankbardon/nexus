package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestSchemaStability_Request and TestSchemaStability_Response are
// SCHEMA-STABILITY SNAPSHOT TESTS. They marshal a fully-populated
// Request and Response to JSON and compare against the snapshots below.
//
// PRs that change the wire format MUST update the snapshot deliberately
// in the same change. External harnesses (Inspect AI shims, Braintrust
// integrations, custom CI tooling) pin to this exact byte layout — a
// drive-by rename of a JSON tag, the addition of a new required field,
// or a reorder of existing fields breaks them silently.
//
// When updating: increment SchemaVersion and the inspect-protocol.md
// migration notes in the same commit.
//
// The expected JSON is normalized via json.Compact before comparison so
// the snapshot is robust to local whitespace; the *field set* and
// *field names* are what's pinned.

const requestSnapshotV1 = `{
  "schema": 1,
  "config_path": "configs/coding.yaml",
  "user_input": "explain the build error in main.go",
  "max_turns": 8,
  "metadata": {
    "case_id": "swe-bench-1234"
  }
}`

const responseSnapshotV1 = `{
  "schema": 1,
  "session_id": "01HK...",
  "final_assistant_message": "the build error is in line 42",
  "tool_calls": [
    {
      "tool": "shell",
      "args": {
        "cmd": "go build ./..."
      },
      "result_summary": "main.go:42: undefined: Foo",
      "duration_ms": 412
    }
  ],
  "tokens": {
    "input": 6213,
    "output": 1102
  },
  "latency_ms": 18733,
  "metadata": {
    "case_id": "swe-bench-1234"
  },
  "error": null
}`

func TestSchemaStability_Request(t *testing.T) {
	req := &Request{
		Schema:     SchemaVersion,
		ConfigPath: "configs/coding.yaml",
		UserInput:  "explain the build error in main.go",
		MaxTurns:   8,
		Metadata: map[string]any{
			"case_id": "swe-bench-1234",
		},
	}
	got, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := mustCompact(t, requestSnapshotV1)
	if string(got) != want {
		t.Errorf("request JSON drift\n want: %s\n  got: %s\n\n"+
			"This test pins the wire format. If you intend to change it, "+
			"update requestSnapshotV1 in this file AND bump SchemaVersion.",
			want, got)
	}
}

func TestSchemaStability_Response(t *testing.T) {
	resp := &Response{
		Schema:                SchemaVersion,
		SessionID:             "01HK...",
		FinalAssistantMessage: "the build error is in line 42",
		ToolCalls: []ToolCall{
			{
				Tool:          "shell",
				Args:          map[string]any{"cmd": "go build ./..."},
				ResultSummary: "main.go:42: undefined: Foo",
				DurationMs:    412,
			},
		},
		Tokens:    Tokens{Input: 6213, Output: 1102},
		LatencyMs: 18733,
		Metadata: map[string]any{
			"case_id": "swe-bench-1234",
		},
		Error: nil,
	}
	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := mustCompact(t, responseSnapshotV1)
	if string(got) != want {
		t.Errorf("response JSON drift\n want: %s\n  got: %s\n\n"+
			"This test pins the wire format. If you intend to change it, "+
			"update responseSnapshotV1 in this file AND bump SchemaVersion.",
			want, got)
	}
}

// TestSchemaVersion_Constant guards the version itself. Bumping
// SchemaVersion is a deliberate event with documentation requirements.
func TestSchemaVersion_Constant(t *testing.T) {
	if SchemaVersion != 1 {
		t.Fatalf("SchemaVersion=%d; bumping requires updating "+
			"docs/src/eval/inspect-protocol.md migration notes and the "+
			"snapshots in this file.", SchemaVersion)
	}
}

func mustCompact(t *testing.T, raw string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		t.Fatalf("compact %q: %v", strings.TrimSpace(raw), err)
	}
	return buf.String()
}
