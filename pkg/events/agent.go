package events

// TurnInfo describes a single agent turn.
type TurnInfo struct {
	TurnID    string
	Iteration int
	SessionID string
}

// Plan represents a multi-step execution plan.
type Plan struct {
	Steps  []PlanStep
	TurnID string
}

// PlanStep is a single step within a plan.
type PlanStep struct {
	Description string
	Status      string // "pending", "active", "completed", "failed"
}

// SubagentSpawn requests spawning a new subagent.
type SubagentSpawn struct {
	SpawnID      string
	Task         string   // what the subagent should accomplish
	SystemPrompt string   // optional system prompt override
	Tools        []string // allowed tool names (empty = all available)
	ModelRole    string   // model role override (empty = plugin default)
	ParentTurnID string
}

// SubagentStarted signals that a subagent has begun execution.
type SubagentStarted struct {
	SpawnID      string
	Task         string
	ParentTurnID string
}

// SubagentIteration reports a single subagent iteration for observability.
type SubagentIteration struct {
	SpawnID   string
	Iteration int
	Content   string           // assistant text content, if any
	ToolCalls []ToolCallRequest // tool calls made this iteration
}

// SubagentComplete signals that a subagent has finished execution.
type SubagentComplete struct {
	SpawnID      string
	Result       string
	Error        string
	Iterations   int
	TokensUsed   Usage
	ParentTurnID string
}
