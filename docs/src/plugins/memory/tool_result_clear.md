# Tool-Result Clearing

Live curator that drops the body of stale tool results from outgoing LLM
requests while keeping the call/result envelope. The model still sees that
the tool was invoked; the (often large) body is replaced inline with a
`<tool_result … cleared="true" …/>` marker.

This is the highest-leverage layer in Idea 30's curation stack — for
tool-heavy sessions, 50–80% of context bytes are tool-result bodies,
most of which are dead weight by the time the next request fires.

## Details

| | |
|---|---|
| **ID** | `nexus.memory.tool_result_clear` |
| **Capabilities** | _none_ |
| **Dependencies** | _none_ |

Operates at priority 12 on `before:llm.request` — after
`nexus.discovery.progressive` (priority 8) has shaped the tool list, then
re-shapes the outgoing message slice without touching the upstream
history buffer.

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Toggle the curator. |
| `age_turns` | int | `5` | Clear tool results older than this many turns when also exceeding `size_bytes_threshold`. |
| `size_bytes_threshold` | int | `1000` | Skip clearing for result bodies smaller than this many bytes. |
| `preserve_recent_kinds` | []string | `["error","user_question"]` | Result kinds never cleared regardless of age. |
| `drop_strategy` | string | `replace_with_envelope` | `replace_with_envelope` keeps the call/result pair with a marker body (default). `full_drop` removes the message entirely; risks tool_use/tool_result pairing breakage. |

## Heuristics

A tool result is cleared when **any** of the following hold:

- **Age + size** — `now_turn − call_turn ≥ age_turns` AND
  `len(result) ≥ size_bytes_threshold`.
- **Subsequent-call** — the same tool was invoked later with semantically
  equivalent arguments (canonical-JSON hash). Earlier results are
  redundant.
- **Preserved kind exemption** — results classified as `error` (and any
  kind listed in `preserve_recent_kinds`) are never cleared.

Once an ID is cleared, it stays cleared for the remainder of the session
— subsequent requests re-replace deterministically without re-running the
heuristic.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 60 | Track call name, canonical args hash, and turn |
| `tool.result` | 60 | Record result size and kind classification |
| `agent.turn.end` | 60 | Increment internal turn counter |
| `before:llm.request` | 12 | Mutate `req.Messages`, replace stale tool result bodies |

### Emits

| Event | When |
|-------|------|
| `memory.tool_result_cleared` | Once per cleared call (carries `tool_call_id`, `tool`, `original_size`, `cleared_at_turn`, `reason`) |
| `memory.curated` | Envelope event with stability descriptor (`Layer: "tool_result_clear"`, `CacheInvalidates: false`) |

## Replay Determinism

The clearing decision is heuristic, but every cleared call is recorded as
a `memory.tool_result_cleared` event. The durable journal (Idea 01)
captures these events so replay reproduces the same envelope without
re-running the heuristic.

## Example Configuration

```yaml
plugins:
  active:
    - nexus.agent.react
    - nexus.memory.tool_result_clear

  nexus.memory.tool_result_clear:
    enabled: true
    age_turns: 4
    size_bytes_threshold: 512
    preserve_recent_kinds: ["error", "user_question"]
    drop_strategy: replace_with_envelope
```
