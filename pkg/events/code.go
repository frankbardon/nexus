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

// CodeExecStdout carries a chunk of stdout produced by a run_code script
// while it is still executing. Emitted incrementally by nexus.tool.code_exec
// so IO plugins can show live output instead of waiting for the final
// CodeExecResult. Chunks are flushed on newline or when the buffered output
// exceeds an internal threshold; the last chunk (possibly empty) carries
// Final=true so UIs can mark the stream closed.
type CodeExecStdout struct {
	CallID    string
	TurnID    string
	Chunk     string // UTF-8 text; may contain newlines
	Final     bool   // true for the last chunk of this call
	Truncated bool   // true on the final chunk if the total exceeded max_output_bytes
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
