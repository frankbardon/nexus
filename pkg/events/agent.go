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
type PlanStep struct {
	Description string
	Status      string // "pending", "active", "completed", "failed"
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
type SubagentIteration struct {
	SchemaVersion int `json:"_schema_version"`

	SpawnID   string
	Iteration int
	Content   string            // assistant text content, if any
	ToolCalls []ToolCallRequest // tool calls made this iteration
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
