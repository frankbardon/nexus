# Simple History

Minimal reference implementation of `memory.history`: an unbounded,
in-memory slice with no persistence. Useful for tests, demos, and
short-lived sessions where the sliding-window/pair-safe machinery of
[`nexus.memory.capped`](./capped.md) adds no value.

## Details

| | |
|---|---|
| **ID** | `nexus.memory.simple` |
| **Capability** | `memory.history` |
| **Dependencies** | None |

## Configuration

No configuration required.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.input` | 10 | Records user messages |
| `llm.response` | 10 | Records assistant responses (with `ToolCalls` populated) |
| `tool.invoke` | 10 | Tracks `ParentCallID` filter set only; no message appended |
| `tool.result` | 10 | Records tool role messages (unless internally dispatched) |
| `memory.history.query` | 50 | Responds with the current buffer in LLM-native order |
| `memory.compacted` | 50 | Replaces the buffer with the compacted message set |

### Emits

None.

## Behaviour

- Storage mirrors `events.Message` exactly so consumers feed the buffer
  directly into an `LLMRequest` without translation.
- Internal tool calls (`ParentCallID != ""`) are filtered so the LLM never
  sees `tool_use_id`s it didn't generate — same invariant as
  [`nexus.memory.capped`](./capped.md).
- `llm.response` events tagged with `Metadata["_source"]` (planner replies,
  summariser replies) are ignored so only user-facing turns land in the
  buffer.
- No persistence: history is lost on process exit.

## When to Use

- Integration tests where the default [`capped`](./capped.md) plugin's
  JSONL persistence is overhead.
- Experimental agents where bounded history would confuse the outcome.
- Reference for building a new `memory.history` provider.

## Example Configuration

```yaml
capabilities:
  memory.history: nexus.memory.simple

plugins:
  active:
    - nexus.agent.react
    - nexus.memory.simple
```
