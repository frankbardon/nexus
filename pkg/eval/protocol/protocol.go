// Package protocol implements the JSON request/response wire format used by
// `nexus eval --inspect-mode`. External harnesses (AISI Inspect AI,
// Braintrust, custom shims) drive Nexus headlessly by piping a single JSON
// request to stdin and parsing a single JSON response from stdout.
//
// The wire format is documented at docs/src/eval/inspect-protocol.md and
// pinned by the schema-stability snapshot test in this package. PRs that
// change the wire format MUST update the snapshot deliberately — a
// downstream shim is pinned to it.
package protocol

import (
	"encoding/json"
	"fmt"
	"io"
)

// SchemaVersion is the wire-format version. Bumping this is a deliberate
// breaking change; the snapshot test refuses silent migration.
const SchemaVersion = 1

// Request is the inspect-mode request envelope. Exactly one of ConfigPath
// or ConfigInline must be set.
type Request struct {
	// Schema is the wire-format version. Must equal SchemaVersion.
	Schema int `json:"schema"`
	// ConfigPath is the absolute or tilde-prefixed path to a YAML config.
	// Mutually exclusive with ConfigInline.
	ConfigPath string `json:"config_path,omitempty"`
	// ConfigInline is a YAML config body inline. Mutually exclusive with
	// ConfigPath.
	ConfigInline string `json:"config_inline,omitempty"`
	// UserInput is the single prompt to drive the agent with. Required.
	UserInput string `json:"user_input"`
	// MaxTurns caps the number of agent.turn.end events the runner will
	// observe before halting. 0 (or unset) means "no protocol-level cap"
	// — the agent's own iteration gate still bounds the run.
	MaxTurns int `json:"max_turns,omitempty"`
	// Metadata is opaque pass-through round-tripped on the response.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Response is the inspect-mode response envelope. On success, Error is
// nil. On failure, Error is populated and the process exits non-zero.
type Response struct {
	// Schema mirrors Request.Schema for symmetry.
	Schema int `json:"schema"`
	// SessionID is the new session's UUID. Empty when the engine never
	// reached session bootstrap (early validation errors).
	SessionID string `json:"session_id,omitempty"`
	// FinalAssistantMessage is the text of the final assistant llm.response
	// (FinishReason "end_turn" or equivalent). Empty when the agent never
	// produced one.
	FinalAssistantMessage string `json:"final_assistant_message"`
	// ToolCalls is the ordered list of tool invocations and their summarized
	// results. Order matches dispatch order.
	ToolCalls []ToolCall `json:"tool_calls"`
	// Tokens aggregates input/output usage across every llm.response in the
	// session.
	Tokens Tokens `json:"tokens"`
	// LatencyMs is the wall-clock between the first and last journaled
	// envelope of the session, in milliseconds.
	LatencyMs int64 `json:"latency_ms"`
	// Metadata is the round-tripped Request.Metadata.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Error is populated on failure. nil on success.
	Error *Error `json:"error"`
}

// ToolCall is one invoke→result pair surfaced in the response.
type ToolCall struct {
	// Tool is the tool name (e.g. "shell").
	Tool string `json:"tool"`
	// Args is the parsed argument map. Best-effort: when the agent emits
	// an unparseable JSON arguments string, Args is nil and the raw text
	// lives under Args["_raw"] (kept simple and forward-compatible).
	Args map[string]any `json:"args,omitempty"`
	// ResultSummary is a truncated (≤2KB) stringification of the tool's
	// output. Empty when no matching tool.result envelope was observed.
	ResultSummary string `json:"result_summary"`
	// DurationMs is result.Ts − invoke.Ts in milliseconds. 0 when no
	// matching result was observed.
	DurationMs int64 `json:"duration_ms"`
}

// Tokens aggregates per-session token usage.
type Tokens struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// Error is the structured failure envelope. Codes are documented in
// errors.go and the protocol reference.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ParseRequest reads exactly one JSON object from r, with strict
// "unknown fields rejected" parsing so a typo in the request surfaces
// as INVALID_REQUEST rather than a silent default.
//
// The caller is expected to terminate the request stream with EOF after
// the JSON object — a trailing newline is tolerated, additional content
// is rejected.
func ParseRequest(r io.Reader) (*Request, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("empty request: stdin EOF before any JSON object")
		}
		return nil, fmt.Errorf("decode request: %w", err)
	}
	// Reject trailing non-whitespace content.
	if dec.More() {
		return nil, fmt.Errorf("trailing data after request JSON")
	}
	return &req, nil
}

// WriteResponse writes resp to w as a single JSON object followed by a
// newline. The newline keeps the output friendly for shell pipelines and
// stream-based parsers (e.g. `... | jq .`).
//
// Pretty-printing is intentionally avoided: external harnesses parse
// machine-to-machine, and a compact encoding is the wire-format choice.
// Tests that want pretty output can re-marshal locally.
func WriteResponse(w io.Writer, resp *Response) error {
	if resp == nil {
		return fmt.Errorf("nil response")
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	return nil
}

// Validate enforces the request invariants documented on Request. It is
// invoked by Run; ParseRequest does not call it so that callers can
// inspect a structurally-valid-but-semantically-wrong request before
// dispatching.
func (r *Request) Validate() error {
	if r == nil {
		return ErrInvalidRequest("nil request")
	}
	if r.Schema != SchemaVersion {
		return ErrInvalidRequest(fmt.Sprintf("schema=%d, want %d", r.Schema, SchemaVersion))
	}
	pathSet := r.ConfigPath != ""
	inlineSet := r.ConfigInline != ""
	if pathSet && inlineSet {
		return ErrInvalidRequest("config_path and config_inline are mutually exclusive")
	}
	if !pathSet && !inlineSet {
		return ErrInvalidRequest("one of config_path or config_inline is required")
	}
	if r.UserInput == "" {
		return ErrInvalidRequest("user_input is required")
	}
	if r.MaxTurns < 0 {
		return ErrInvalidRequest("max_turns must be >= 0")
	}
	return nil
}
