package events

// CodeExecRequest signals that a run_code script is about to execute.
// Emitted by nexus.tool.code_exec before the Yaegi interpreter runs the script.
type CodeExecRequest struct {
	CallID  string // matches the originating ToolCall.ID
	TurnID  string
	Script  string   // full Go source
	Imports []string // import paths referenced by the script
	Skills  []string // active skill names whose helpers are loaded
}

// CodeExecResult reports the outcome of a run_code script.
// Emitted by nexus.tool.code_exec after the script returns (or errors out).
type CodeExecResult struct {
	CallID    string
	TurnID    string
	Output    string // stdout (capped at max_output_bytes)
	Result    string // JSON-marshaled return value of Run()
	Error     string // first error encountered (compile, AST reject, runtime, timeout)
	Duration  int64  // execution wall time in milliseconds
	Truncated bool   // stdout was truncated
}
