package events

import "time"

// Schema-version constants for tool.* payloads. See doc.go.
const (
	ToolCallVersion         = 1
	ToolCatalogQueryVersion = 1
	ToolTimeoutVersion      = 1
	ToolResultVersion       = 1
)

// ToolCall represents a tool invocation.
type ToolCall struct {
	SchemaVersion int `json:"_schema_version"`

	ID        string
	Name      string
	Arguments map[string]any
	// TurnID correlates this call's tool.result back to the loop that
	// issued it. Nested loops (subagent, delegate) use a private turn
	// ("subagent_<id>", "delegate_<id>") here so their results never
	// collide with the parent's — a turn no transport ever saw start.
	TurnID string
	// ParentTurnID, when set, is the turn this call is BOUND to for
	// anything a transport scopes by turn — chiefly a HITL request raised
	// on the call (approval_policy, ask_user). Nested loops set it to the
	// turn they serve, because a hitl.requested carrying the private
	// TurnID above is dropped by turn-scoped transports (nexus.io.agui)
	// and the parent run never sees the approval. Empty for calls issued
	// directly by a top-level loop, where TurnID already is that turn.
	// Read it through BindingTurnID, never directly.
	ParentTurnID string
	// ParentCallID, when set, marks this call as internal to another tool
	// (e.g. dispatched by a run_code script). Conversation history omits
	// these so the LLM never sees tool_use_ids it didn't originate — gates
	// and observers still fire on the bus as usual.
	ParentCallID string
	// Sequence is the LLM-returned position of this call within its batch
	// (0-based). Set by the agent when a single LLM response contains
	// multiple tool calls; lets observers reorder completion-order events
	// back into request order under parallel dispatch.
	Sequence int
}

// BindingTurnID is the turn a HITL request (or any other turn-scoped,
// transport-visible emission) raised on this call should carry:
// ParentTurnID when a nested loop set it, otherwise TurnID. It is NOT the
// key for matching tool results — that stays TurnID.
func (tc ToolCall) BindingTurnID() string {
	if tc.ParentTurnID != "" {
		return tc.ParentTurnID
	}
	return tc.TurnID
}

// ToolCatalogQuery is a synchronous request for the currently registered
// tool definitions. Emitted as a pointer payload on "tool.catalog.query";
// the handler fills Tools in place before Emit returns. Same pattern as
// HistoryQuery — no correlation IDs, no round-trip via a response event.
type ToolCatalogQuery struct {
	SchemaVersion int `json:"_schema_version"`

	// Tools is filled by the handler. Caller should treat it as nil on
	// input; a nil result after Emit means no catalog plugin answered.
	Tools []ToolDef
}

// ToolTimeout signals that a tool invocation exceeded its configured
// per-call deadline. Emitted by nexus.gate.tool_timeout alongside a
// synthetic tool.result error so observers can distinguish a tool's own
// timeout from a gate-enforced one. Override names the exact YAML key the
// operator should set to lift the limit.
type ToolTimeout struct {
	SchemaVersion int `json:"_schema_version"`

	ToolName string
	CallID   string
	Timeout  time.Duration
	Override string // e.g. "nexus.gate.tool_timeout.per_tool.<name>"
	TurnID   string
}

// ToolResult carries the outcome of a tool invocation.
type ToolResult struct {
	SchemaVersion int `json:"_schema_version"`

	ID         string // matches ToolCall.ID
	Name       string
	Output     string
	Error      string
	OutputFile string // optional: file written to session workspace
	OutputData []byte // optional: binary data
	// OutputStructured carries typed result data when the tool declared an
	// OutputSchema in its ToolDef. Consumers that care about types (e.g.
	// run_code's typed bindings) prefer this over parsing Output.
	// Content should validate against the tool's declared OutputSchema.
	OutputStructured map[string]any

	// OutputParts carries multimodal content the tool produced (image,
	// audio, document, video). When non-empty, the memory plugin copies
	// these into the resulting tool-role Message.Parts so the next LLM
	// request includes the multimodal content alongside (or in place of)
	// Output. Empty for text-only tools.
	//
	// Large payloads should be stored via pkg/engine/blobs and referenced
	// here as `MessagePart{URI: "nexus-blob:<sha>", MimeType: "..."}` so
	// the journal stays compact; small payloads can ride inline as Data.
	// Provider plugins resolve nexus-blob URIs at request-assembly time.
	OutputParts []MessagePart

	TurnID string
}
