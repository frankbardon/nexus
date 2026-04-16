# Cancel Control Plugin

Coordinates cancellation of in-progress agent operations. Tracks active turns and routes cancellation requests to the appropriate handlers.

## Details

| | |
|---|---|
| **ID** | `nexus.control.cancel` |
| **Dependencies** | None |

## Configuration

No configuration options.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `agent.turn.start` | 10 | Tracks which turn is active |
| `agent.turn.end` | 10 | Clears active turn tracking |
| `cancel.request` | 10 | Receives cancellation requests from I/O |
| `cancel.resume` | 10 | Receives resume requests |

### Emits

| Event | When |
|-------|------|
| `cancel.active` | Broadcasts cancellation to all handlers |
| `io.status` | Status updates |

## How It Works

1. When the user triggers a cancel (e.g., pressing a key in the TUI), the I/O plugin emits `cancel.request`
2. The cancel controller checks if there's an active turn
3. If so, it emits `cancel.active` with the turn ID
4. All plugins listening for `cancel.active` abort their current work (LLM provider cancels the API call, agent stops iterating)
5. When the user resumes, `cancel.resume` restarts the flow

## When to Use

Include this plugin when using agents that support cancellation (ReAct, Plan & Execute, Orchestrator). It's essential for interactive workflows where the user may want to interrupt long-running operations.

```yaml
plugins:
  active:
    - nexus.control.cancel
    # ... other plugins
```
