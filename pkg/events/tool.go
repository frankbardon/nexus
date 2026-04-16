package events

// ToolCall represents a tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	TurnID    string
}

// ToolResult carries the outcome of a tool invocation.
type ToolResult struct {
	ID         string // matches ToolCall.ID
	Name       string
	Output     string
	Error      string
	OutputFile string // optional: file written to session workspace
	OutputData []byte // optional: binary data
	TurnID     string
}
