package events

// Schema-version constants for agent.* payloads. See doc.go.
const (
	TurnInfoVersion          = 1
	PlanVersion              = 1
	SubagentSpawnVersion     = 1
	SubagentStartedVersion   = 1
	SubagentIterationVersion = 1
	AgentToolChoiceVersion   = 1
	SubagentCompleteVersion  = 1
)

// TurnInfo describes a single agent turn.
type TurnInfo struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID    string
	Iteration int
	SessionID string
}

// Plan represents a multi-step execution plan.
type Plan struct {
	SchemaVersion int `json:"_schema_version"`

	Steps  []PlanStep
	TurnID string
}

// PlanStep is a single step within a plan.
//
// SpawnID is set by agents that delegate steps to subagents (the
// orchestrator's parallel-worker flow); the UI uses it to fold per-worker
// progress (iteration count, tool calls, terminal totals) into the plan
// step instead of rendering a parallel "workers" panel. Empty for plans
// emitted by agents that execute steps inline (planexec, etc.).
type PlanStep struct {
	Description string
	Status      string // "pending", "active", "completed", "failed", "skipped"
	SpawnID     string
}

// SubagentSpawn requests spawning a new subagent.
type SubagentSpawn struct {
	SchemaVersion int `json:"_schema_version"`

	SpawnID      string
	Task         string   // what the subagent should accomplish
	SystemPrompt string   // optional system prompt override
	Tools        []string // allowed tool names (empty = all available)
	ModelRole    string   // model role override (empty = plugin default)
	ParentTurnID string
}

// SubagentStarted signals that a subagent has begun execution.
type SubagentStarted struct {
	SchemaVersion int `json:"_schema_version"`

	SpawnID      string
	Task         string
	ParentTurnID string
}

// SubagentIteration reports a single subagent iteration for observability.
//
// ParentTurnID echoes the spawning agent's turn so UI bridges can group
// iteration events with the started/complete events for the same worker.
// Without it, frontends that key panels by parent_turn_id end up
// fragmenting a single worker's lifecycle across multiple UI elements.
type SubagentIteration struct {
	SchemaVersion int `json:"_schema_version"`

	SpawnID      string
	Iteration    int
	Content      string            // assistant text content, if any
	ToolCalls    []ToolCallRequest // tool calls made this iteration
	ParentTurnID string
}

// AgentToolChoice is emitted by plugins to dynamically override the agent's
// tool choice for subsequent LLM requests.
type AgentToolChoice struct {
	SchemaVersion int `json:"_schema_version"`

	Mode     string // "auto" | "required" | "none" | "tool"
	ToolName string // when Mode == "tool"
	Duration string // "once" = next request only; "sticky" = until replaced
}

// SubagentComplete signals that a subagent has finished execution.
type SubagentComplete struct {
	SchemaVersion int `json:"_schema_version"`

	SpawnID      string
	Result       string
	Error        string
	Iterations   int
	TokensUsed   Usage
	CostUSD      float64
	ParentTurnID string
}
