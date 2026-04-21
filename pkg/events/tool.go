package events

// ToolCall represents a tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	TurnID    string
	// ParentCallID, when set, marks this call as internal to another tool
	// (e.g. dispatched by a run_code script). Conversation history omits
	// these so the LLM never sees tool_use_ids it didn't originate — gates
	// and observers still fire on the bus as usual.
	ParentCallID string
}

// ToolResult carries the outcome of a tool invocation.
type ToolResult struct {
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
	TurnID           string
}
