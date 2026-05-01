# Thinking Observer

Marker observer for `thinking.step` and `plan.progress` events. The
plugin no longer writes derived JSONL files — the per-session
**[journal](../../architecture/sessions.md)** is the single source of
truth for both event types, alongside every other event on the bus.

When this plugin is in `plugins.active`, terminal and browser shells
turn on thinking-related UI affordances (e.g. dedicated "thinking"
message styling). Without it, the events still flow on the bus and
land in the journal — only the optional UI surface differs.

## Details

| | |
|---|---|
| **ID** | `nexus.observe.thinking` |
| **Dependencies** | None |

## Configuration

No configuration options.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `thinking.step` | 90 | Marker subscription (no side effects) |
| `plan.progress` | 90 | Marker subscription (no side effects) |

### Emits

None.

## Reading the thinking history

Thinking steps and plan progress live in
`<session>/journal/active.jsonl` (and rotated `*.jsonl.zst` segments)
exactly like every other event. Two ways to consume them:

- **Live**, in-process: subscribe to envelopes via
  `journal.Writer.SubscribeProjection(["thinking.step", "plan.progress"], handler)`.
  Handlers fire synchronously after the writer has flushed each
  envelope to disk.
- **Post-mortem**, walking the journal directory:
  `journal.ProjectFile(journalDir, []string{"thinking.step", "plan.progress"}, handler)`.
  Useful for regenerating derived views after a recall or crash.

## Thinking Step Payload

```json
{
  "turn_id": "abc123",
  "source": "nexus.agent.react",
  "content": "The user wants to refactor the auth module...",
  "phase": "reasoning",
  "timestamp": "2026-04-08T10:30:00Z"
}
```

Phases: `planning`, `executing`, `reasoning`.

## Example Configuration

```yaml
# Just activate it — no config needed
nexus.observe.thinking: {}
```
