# Capped Conversation History

Maintains a sliding window of conversation messages and persists them to the session as JSONL. Default provider of `memory.history`.

## Details

| | |
|---|---|
| **ID** | `nexus.memory.capped` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_messages` | int | `100` | Maximum messages to keep in the buffer |
| `persist` | bool | `true` | Write messages to `context/conversation.jsonl` in the session |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.input` | 10 | Records user messages |
| `io.output` | 10 | Records agent responses |
| `tool.invoke` | 50 | Records tool calls |
| `tool.result` | 50 | Records tool results |
| `memory.store` | 50 | Explicit memory storage requests |
| `memory.query` | 50 | Responds to history queries |
| `memory.compacted` | 50 | Replaces history with compacted version |

### Emits

| Event | When |
|-------|------|
| `memory.result` | Response to a `memory.query` |

## Behavior

- Messages are stored in a rolling buffer of size `max_messages`
- Oldest messages are dropped when the buffer is full
- If `persist: true`, each message is appended to `context/conversation.jsonl` as it arrives
- On `memory.compacted`, the buffer is replaced with the compacted messages
- An `llm.response` produced by an internal sub-flow is **not** recorded. The
  filter reads `task_kind` off the response metadata, and the skipped set is
  `plan`, `classify`, `summarise`, `compact`, `subagent` and `delegate` — each
  of those loops keeps a history of its own. The main agent loops
  (`react_main`, `planexec_step`, `orchestrator_decompose`,
  `orchestrator_synthesize`) are deliberately absent because they *are* the
  conversation. Recording a sub-flow's tool-calling response leaves two
  consecutive assistant turns in the user-facing history, which Gemini rejects
  with `400 INVALID_ARGUMENT`.
- A `tool.result` is recorded only when it belongs to the conversation's own
  turn. Sub-flows dispatch their tools on the same bus, so the buffer learns
  the conversation's `TurnID` from a `tool.invoke` whose ID a recorded
  assistant message declared, and drops a result carrying any other. Without
  it the history grew a `tool` message whose `ToolCallID` no assistant turn
  had asked for. The filter is **inert** until that turn is known and on a
  result with an empty `TurnID` — an engine caller may drive the tool bus with
  no turn at all — which is the same posture `nexus.agent.react` takes on its
  pending-call count. The existing `ParentCallID` filter still applies
  independently, for sub-calls fired from inside another tool.

## Querying History

Other plugins can query conversation history:

```go
bus.Emit("memory.query", events.MemoryQuery{
    Query:     "",    // Not filtered — returns all
    Limit:     50,    // Max messages to return
    SessionID: "...", // Current session
})
// Listen for memory.result event with the messages
```

## Example Configuration

```yaml
nexus.memory.capped:
  max_messages: 200
  persist: true
```
