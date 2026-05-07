# Tool-Definition Pruner

Removes individual tool definitions from outgoing `LLMRequest.Tools`
when those tools have been idle past a turn threshold. Pairs with
[`nexus.discovery.progressive`](../tools/discovery.md): progressive scopes
by class, this scopes per individual tool.

For sessions where the agent loaded 30 tool definitions in turn 4 but
only uses 3 of them, the pruner keeps the unused 27 from paying tokens
on every subsequent request.

## Details

| | |
|---|---|
| **ID** | `nexus.memory.tool_def_pruner` |
| **Capabilities** | _none_ |
| **Dependencies** | _none_ |

Operates at priority 14 on `before:llm.request` — after both
`nexus.discovery.progressive` (8) and `nexus.memory.tool_result_clear`
(12) have shaped the request.

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Toggle the pruner. |
| `unused_turns_threshold` | int | `6` | Drop a tool definition after this many consecutive turns without an invocation. |
| `never_prune` | []string | `["discover","ask_user"]` | Tool names exempt from pruning. |

## Behaviour

- Subscribes to `tool.invoke` to reset the per-tool last-used counter on
  every successful call. A pruned tool that the agent invokes again is
  un-pruned automatically — typically by going back through
  `discovery/progressive`'s `discover` meta-tool.
- First sight of a tool registers it at the current turn rather than
  zero, so freshly-loaded tools aren't pruned the moment they appear.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 60 | Reset per-tool last-used counter |
| `agent.turn.end` | 60 | Increment internal turn counter |
| `before:llm.request` | 14 | Filter `req.Tools` |

### Emits

| Event | When |
|-------|------|
| `memory.tool_def_pruned` | Per pruned tool (carries `tool_id`, `last_used_turn`, `definition_size`) |
| `memory.curated` | Envelope event (`Layer: "tool_def_pruner"`, `CacheInvalidates: true` — tool defs are part of the cached prefix) |

## Example Configuration

```yaml
plugins:
  active:
    - nexus.agent.react
    - nexus.memory.tool_def_pruner

  nexus.memory.tool_def_pruner:
    unused_turns_threshold: 6
    never_prune: ["discover", "ask_user", "memory_read"]
```
