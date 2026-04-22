# Summary-Buffer History

Inline auto-compacting `memory.history` provider. Keeps the most recent
`max_recent` messages verbatim and replaces older messages with an
LLM-generated summary, emitted as a single system message at the head of
the buffer.

Unlike [`nexus.memory.compaction`](./compaction.md) — which is an external
coordinator that emits `memory.compacted` for a separate history plugin
to adopt — this plugin serves `memory.history` directly, so the
summarised view is what the ReAct agent sees on the next request.

## Details

| | |
|---|---|
| **ID** | `nexus.memory.summary_buffer` |
| **Capabilities** | `memory.history`, `memory.compaction` |
| **Dependencies** | An LLM provider addressable by the configured `model_role`. |

> **Note.** Running this plugin alongside `nexus.memory.compaction` is a
> misconfiguration: both advertise `memory.compaction` and the engine
> will emit a boot-time WARN naming the ambiguity. Pick one.

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `strategy` | string | `"message_count"` | Trigger type: `"message_count"`, `"token_estimate"`, `"turn_count"` |
| `message_threshold` | int | `50` | Buffer size that fires `message_count` strategy |
| `token_threshold` | int | `30000` | Estimated tokens that fires `token_estimate` strategy |
| `turn_threshold` | int | `10` | Turn count that fires `turn_count` strategy |
| `chars_per_token` | float64 | `4.0` | Rough per-token char ratio for token estimation |
| `max_recent` | int | `8` | Number of recent messages kept verbatim after summarisation |
| `model_role` | string | `"quick"` | Role used to dispatch the summarisation LLM request |
| `model` | string | `""` | Explicit model override; otherwise inferred from the role |
| `prompt` | string | _built-in_ | Inline override of the summarisation system prompt |
| `prompt_file` | string | _unset_ | Path to a file containing the summarisation prompt (takes precedence over `prompt`) |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.input` | 10 | Records user messages |
| `llm.response` | 10 | Records assistant responses; absorbs summariser replies tagged `_source=nexus.memory.summary_buffer` |
| `tool.invoke` | 10 | Tracks internal-call filter |
| `tool.result` | 10 | Records tool role messages |
| `agent.turn.end` | 5 | Increments turn counter (drives `turn_count` strategy) |
| `memory.history.query` | 50 | Serves the current buffer in LLM-native order |
| `memory.compact.request` | 10 | Forces a summarisation cycle (from context-window gate, etc.) |

### Emits

| Event | When |
|-------|------|
| `llm.request` | When a summarisation trigger fires. Tagged `Metadata["_source"] = "nexus.memory.summary_buffer"` |
| `memory.compaction.triggered` | Start of each summarisation cycle |
| `memory.compacted` | End of each summarisation cycle, with the new buffer contents |
| `io.status` | UI status updates: `"Summarising context..."` / `"idle"` |

## Behaviour

1. Every append runs a threshold check (except `turn_count`, which checks
   at `agent.turn.end`).
2. On trip, the plugin snapshots the buffer and computes a safe split —
   protecting the trailing `max_recent` messages, shifting the boundary
   left when needed so an assistant `tool_use` and its matching
   `tool_result`(s) are never separated.
3. The snapshot prefix is serialised into a transcript and sent to the
   LLM via the configured `model_role`.
4. On the summariser's reply, the plugin collapses the prefix into a
   single system message (`"## Prior Context (Summarised)\n\n..."`) and
   replaces the buffer with `[summary, ...recent]`.
5. `memory.compacted` is emitted so any observer plugins (logger, UI) see
   the transition.

## When to Use

- Long-running chat sessions where context window is the binding constraint.
- Agents that don't benefit from verbatim history beyond the last few turns.
- Workloads that tolerate occasional latency spikes when a summarisation
  cycle runs mid-turn.

## Example Configuration

```yaml
capabilities:
  memory.history: nexus.memory.summary_buffer

plugins:
  active:
    - nexus.agent.react
    - nexus.memory.summary_buffer

  nexus.memory.summary_buffer:
    strategy: token_estimate
    token_threshold: 20000
    max_recent: 10
    model_role: quick
```
