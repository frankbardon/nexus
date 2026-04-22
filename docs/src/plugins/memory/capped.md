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
