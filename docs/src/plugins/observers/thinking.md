# Thinking Persistence

Persists thinking steps and plan progress to the session as JSONL files. This creates an audit trail of the agent's reasoning process.

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
| `thinking.step` | 90 | Persists thinking steps |
| `plan.progress` | 90 | Persists plan step updates |

### Emits

None.

## Output Files

| File | Content |
|------|---------|
| `context/thinking.jsonl` | One JSON object per thinking step |
| `context/plans.jsonl` | One JSON object per plan progress update |

Both files are appended to throughout the session.

## Thinking Step Format

Each thinking step includes:

```json
{
  "turn_id": "abc123",
  "source": "nexus.agent.react",
  "content": "The user wants to refactor the auth module...",
  "phase": "reasoning",
  "timestamp": "2026-04-08T10:30:00Z"
}
```

Phases: `planning`, `executing`, `reasoning`

## Example Configuration

```yaml
# Just activate it — no config needed
nexus.observe.thinking: {}
```
