# Event Types Reference

Complete reference for all event types in Nexus, organized by domain.

## Core Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `core.boot` | `BootConfig` | Engine boot started |
| `core.ready` | *(none)* | All plugins initialized and ready |
| `core.shutdown` | `ShutdownReason` | Engine shutting down |
| `core.tick` | `TickInfo` | Periodic heartbeat |
| `core.error` | `ErrorInfo` | Error reported by a plugin |

### Payloads

**BootConfig**
| Field | Type | Description |
|-------|------|-------------|
| `ConfigPath` | string | Path to the config file |
| `Profile` | string | Profile name |

**ShutdownReason**
| Field | Type | Description |
|-------|------|-------------|
| `Reason` | string | Why: `"user"`, `"error"`, or `"signal"` |
| `Error` | error | Associated error (if any) |

**TickInfo**
| Field | Type | Description |
|-------|------|-------------|
| `Sequence` | int | Tick counter |
| `Time` | time.Time | When the tick occurred |

**ErrorInfo**
| Field | Type | Description |
|-------|------|-------------|
| `Source` | string | Plugin that reported the error |
| `Err` | error | The error |
| `Fatal` | bool | Whether this should trigger shutdown |

---

## I/O Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `io.input` | `UserInput` | User submitted a message |
| `io.output` | `AgentOutput` | Agent produced output |
| `io.output.stream` | `OutputChunk` | Streaming output chunk |
| `io.output.stream.end` | `StreamRef` | Streaming complete |
| `io.status` | `StatusUpdate` | Agent state changed |
| `io.approval.request` | `ApprovalRequest` | Approval needed for an action |
| `io.approval.response` | `ApprovalResponse` | User responded to approval |
| `io.ask` | `AskUser` | Agent asking user a question |
| `io.ask.response` | `AskUserResponse` | User answered the question |
| `io.history.replay` | `HistoryReplay` | Replaying conversation on resume |
| `io.session.start` | `SessionInfo` | Session started |
| `io.session.end` | *(none)* | Session ended |

### Payloads

**UserInput**
| Field | Type | Description |
|-------|------|-------------|
| `Content` | string | Message text |
| `Files` | []FileAttachment | Attached files |
| `SessionID` | string | Current session |

**FileAttachment**
| Field | Type | Description |
|-------|------|-------------|
| `Name` | string | Filename |
| `MimeType` | string | MIME type |
| `Data` | []byte | File contents |

**AgentOutput**
| Field | Type | Description |
|-------|------|-------------|
| `Content` | string | Output text |
| `Role` | string | `"assistant"`, `"system"`, or `"tool"` |
| `Metadata` | map[string]any | Additional context |
| `TurnID` | string | Associated turn |

**OutputChunk**
| Field | Type | Description |
|-------|------|-------------|
| `Content` | string | Chunk text |
| `TurnID` | string | Associated turn |
| `Index` | int | Chunk sequence number |

**StatusUpdate**
| Field | Type | Description |
|-------|------|-------------|
| `State` | string | `"idle"`, `"thinking"`, `"tool_running"`, `"streaming"` |
| `Detail` | string | Additional info |
| `ToolID` | string | Tool being run (if applicable) |

**ApprovalRequest**
| Field | Type | Description |
|-------|------|-------------|
| `PromptID` | string | Unique identifier for this approval |
| `Description` | string | What needs approval |
| `ToolCall` | any | The tool call details |
| `Risk` | string | `"low"`, `"medium"`, `"high"` |

**ApprovalResponse**
| Field | Type | Description |
|-------|------|-------------|
| `PromptID` | string | Matches the request |
| `Approved` | bool | Whether approved |
| `Always` | bool | Remember this decision |

**AskUser**
| Field | Type | Description |
|-------|------|-------------|
| `PromptID` | string | Unique identifier |
| `Question` | string | The question text |
| `TurnID` | string | Associated turn |

**AskUserResponse**
| Field | Type | Description |
|-------|------|-------------|
| `PromptID` | string | Matches the request |
| `Answer` | string | User's response |

---

## LLM Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `llm.request` | `LLMRequest` | Request to an LLM provider |
| `llm.response` | `LLMResponse` | Complete LLM response |
| `llm.stream.chunk` | `StreamChunk` | Streaming response chunk |
| `llm.stream.end` | `StreamEnd` | Streaming complete |

### Payloads

**LLMRequest**
| Field | Type | Description |
|-------|------|-------------|
| `Role` | string | Model role name (resolved by provider) |
| `Model` | string | Explicit model ID (optional, overrides role) |
| `Messages` | []Message | Conversation messages |
| `Tools` | []ToolDef | Available tools |
| `MaxTokens` | int | Max response tokens |
| `Temperature` | *float64 | Sampling temperature (nil = provider default) |
| `Stream` | bool | Enable streaming |
| `Metadata` | map[string]any | Additional context (e.g., `_source` for planner tagging) |

**Message**
| Field | Type | Description |
|-------|------|-------------|
| `Role` | string | `"system"`, `"user"`, `"assistant"`, `"tool"` |
| `Content` | string | Message text |
| `ToolCallID` | string | For tool role: which call this responds to |
| `ToolCalls` | []ToolCallRequest | For assistant role: tool calls made |

**ToolCallRequest**
| Field | Type | Description |
|-------|------|-------------|
| `ID` | string | Unique call identifier |
| `Name` | string | Tool name |
| `Arguments` | string | JSON-encoded arguments |

**ToolDef**
| Field | Type | Description |
|-------|------|-------------|
| `Name` | string | Tool name |
| `Description` | string | What the tool does |
| `Parameters` | string | JSON Schema for parameters |

**LLMResponse**
| Field | Type | Description |
|-------|------|-------------|
| `Content` | string | Response text |
| `ToolCalls` | []ToolCallRequest | Tool calls in the response |
| `Usage` | Usage | Token usage statistics |
| `Model` | string | Model that was used |
| `FinishReason` | string | Why the response ended |
| `Metadata` | map[string]any | Additional context |

**Usage**
| Field | Type | Description |
|-------|------|-------------|
| `PromptTokens` | int | Input tokens consumed |
| `CompletionTokens` | int | Output tokens generated |
| `TotalTokens` | int | Total tokens |

**StreamChunk**
| Field | Type | Description |
|-------|------|-------------|
| `Content` | string | Chunk text |
| `ToolCall` | *ToolCallRequest | Partial tool call (if applicable) |
| `Index` | int | Chunk sequence number |
| `TurnID` | string | Associated turn |

**StreamEnd**
| Field | Type | Description |
|-------|------|-------------|
| `TurnID` | string | Associated turn |
| `Usage` | Usage | Final token usage |
| `FinishReason` | string | Why the stream ended |

---

## Tool Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `tool.register` | `ToolDef` | Tool available for use |
| `before:tool.invoke` | `ToolCall` | Before tool execution (vetoable) |
| `tool.invoke` | `ToolCall` | Tool invocation |
| `before:tool.result` | `ToolResult` | Before tool result propagation (vetoable) |
| `tool.result` | `ToolResult` | Tool execution result |

### Payloads

**ToolCall**
| Field | Type | Description |
|-------|------|-------------|
| `ID` | string | Call identifier |
| `Name` | string | Tool name |
| `Arguments` | map[string]any | Parsed arguments |
| `TurnID` | string | Associated turn |

**ToolResult**
| Field | Type | Description |
|-------|------|-------------|
| `ID` | string | Matches the call ID |
| `Name` | string | Tool name |
| `Output` | string | Result text |
| `Error` | string | Error message (if failed) |
| `OutputFile` | string | Path to output file (optional) |
| `OutputData` | []byte | Binary output data (optional) |
| `TurnID` | string | Associated turn |

---

## Agent Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `agent.turn.start` | `TurnInfo` | Agent began processing |
| `agent.turn.end` | `TurnInfo` | Agent finished processing |
| `agent.plan` | `Plan` | Agent's current plan |

### Payloads

**TurnInfo**
| Field | Type | Description |
|-------|------|-------------|
| `TurnID` | string | Unique turn identifier |
| `Iteration` | int | Current iteration count |
| `SessionID` | string | Current session |

**Plan**
| Field | Type | Description |
|-------|------|-------------|
| `Steps` | []PlanStep | Plan steps |
| `TurnID` | string | Associated turn |

**PlanStep**
| Field | Type | Description |
|-------|------|-------------|
| `Description` | string | What this step does |
| `Status` | string | `"pending"`, `"active"`, `"completed"`, `"failed"` |

---

## Subagent Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `subagent.spawn` | `SubagentSpawn` | Subagent creation requested |
| `subagent.started` | `SubagentStarted` | Subagent began execution |
| `subagent.iteration` | `SubagentIteration` | Subagent completed an iteration |
| `subagent.complete` | `SubagentComplete` | Subagent finished |

### Payloads

**SubagentSpawn**
| Field | Type | Description |
|-------|------|-------------|
| `SpawnID` | string | Unique spawn identifier |
| `Task` | string | Task description |
| `SystemPrompt` | string | Override system prompt |
| `Tools` | []string | Available tools |
| `ModelRole` | string | Model role |
| `ParentTurnID` | string | Parent's turn ID |

**SubagentComplete**
| Field | Type | Description |
|-------|------|-------------|
| `SpawnID` | string | Spawn identifier |
| `Result` | string | Final result |
| `Error` | string | Error (if failed) |
| `Iterations` | int | Number of iterations used |
| `TokensUsed` | Usage | Token consumption |
| `ParentTurnID` | string | Parent's turn ID |

---

## Memory Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `memory.store` | `MemoryEntry` | Store a memory entry |
| `memory.query` | `MemoryQuery` | Query conversation history |
| `memory.result` | `MemoryResult` | Query results |
| `memory.compaction.triggered` | `CompactionTriggered` | Compaction started |
| `memory.compacted` | `CompactionComplete` | Compaction finished |

### Payloads

**MemoryEntry**
| Field | Type | Description |
|-------|------|-------------|
| `Key` | string | Entry identifier |
| `Content` | string | Content to store |
| `Metadata` | map[string]any | Additional context |
| `SessionID` | string | Current session |

**MemoryQuery**
| Field | Type | Description |
|-------|------|-------------|
| `Query` | string | Search query (empty = all) |
| `Limit` | int | Max results |
| `SessionID` | string | Current session |

**CompactionTriggered**
| Field | Type | Description |
|-------|------|-------------|
| `Reason` | string | Why compaction triggered |
| `MessageCount` | int | Messages before compaction |
| `BackupPath` | string | Backup file location |

**CompactionComplete**
| Field | Type | Description |
|-------|------|-------------|
| `Messages` | []Message | New compacted message set |
| `BackupPath` | string | Backup file location |
| `MessageCount` | int | Messages after compaction |
| `PrevCount` | int | Messages before compaction |

---

## Plan Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `plan.request` | `PlanRequest` | Request plan generation |
| `plan.result` | `PlanResult` | Plan generated |
| `plan.created` | `PlanResult` | Plan ready for display |
| `plan.approval.request` | *(plan data)* | User approval needed |
| `plan.approval.response` | *(approval)* | User responded |
| `plan.progress` | `PlanProgress` | Step status updated |

### Payloads

**PlanRequest**
| Field | Type | Description |
|-------|------|-------------|
| `TurnID` | string | Associated turn |
| `SessionID` | string | Current session |
| `Input` | string | User's original input |

**PlanResult**
| Field | Type | Description |
|-------|------|-------------|
| `TurnID` | string | Associated turn |
| `PlanID` | string | Unique plan identifier |
| `Steps` | []PlanResultStep | Plan steps |
| `Summary` | string | Plan summary |
| `Approved` | bool | Whether approved |
| `Source` | string | `"dynamic"` or `"static"` |

**PlanResultStep**
| Field | Type | Description |
|-------|------|-------------|
| `ID` | string | Step identifier |
| `Description` | string | What this step does |
| `Instructions` | string | Detailed instructions (optional) |
| `Status` | string | Current status |
| `Order` | int | Execution order |

**PlanProgress**
| Field | Type | Description |
|-------|------|-------------|
| `TurnID` | string | Associated turn |
| `PlanID` | string | Plan identifier |
| `StepID` | string | Step being updated |
| `Status` | string | New status |
| `Detail` | string | Additional info |

---

## Skill Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `skill.discover` | `SkillCatalog` | Skills catalog assembled |
| `skill.activate` | `SkillActivation` | Skill activation requested |
| `before:skill.activate` | `SkillActivation` | Before activation (vetoable) |
| `skill.loaded` | `SkillContent` | Skill content loaded |
| `skill.deactivate` | `SkillRef` | Skill deactivation requested |
| `skill.resource.read` | `SkillResourceReq` | Resource file requested |
| `skill.resource.result` | `SkillResourceData` | Resource content returned |

### Payloads

**SkillCatalog**
| Field | Type | Description |
|-------|------|-------------|
| `Skills` | []SkillSummary | All discovered skills |

**SkillSummary**
| Field | Type | Description |
|-------|------|-------------|
| `Name` | string | Skill name |
| `Description` | string | What the skill does |
| `Location` | string | Directory path |
| `Scope` | string | `"project"`, `"user"`, `"builtin"`, `"config"` |

**SkillContent**
| Field | Type | Description |
|-------|------|-------------|
| `Name` | string | Skill name |
| `Body` | string | Markdown content |
| `Resources` | []string | Available resource files |
| `Scope` | string | Skill scope |
| `BaseDir` | string | Skill directory |

---

## Session Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `session.file.created` | `SessionFile` | New file in session |
| `session.file.updated` | `SessionFile` | File updated in session |

**SessionFile**
| Field | Type | Description |
|-------|------|-------------|
| `Path` | string | File path within session |
| `Action` | string | `"created"` or `"updated"` |
| `Size` | int | File size in bytes |

---

## Cancellation Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `cancel.request` | `CancelRequest` | User requested cancellation |
| `cancel.active` | `CancelActive` | Cancellation broadcast |
| `cancel.complete` | `CancelComplete` | Cancellation processed |
| `cancel.resume` | `CancelResume` | Resume after cancellation |

**CancelRequest**
| Field | Type | Description |
|-------|------|-------------|
| `TurnID` | string | Turn to cancel |
| `Source` | string | Who requested: `"tui"`, `"browser"`, etc. |

---

## Thinking Events

| Event Type | Payload | Description |
|------------|---------|-------------|
| `thinking.step` | `ThinkingStep` | Agent reasoning step |

**ThinkingStep**
| Field | Type | Description |
|-------|------|-------------|
| `TurnID` | string | Associated turn |
| `Source` | string | Plugin that generated this |
| `Content` | string | Thinking content |
| `Phase` | string | `"planning"`, `"executing"`, `"reasoning"` |
| `Timestamp` | time.Time | When this occurred |
