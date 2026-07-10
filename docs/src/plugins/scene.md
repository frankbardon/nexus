# Scene Store (`nexus.scene`)

Owns the per-session [Scene](../architecture/scenes.md) store and exposes
five tools the LLM uses to construct durable, addressable, structured
visual output: charts, dashboards, multi-section documents.

## Details

| | |
|---|---|
| **ID** | `nexus.scene` |
| **Capability** | `scene.store` |
| **Dependencies** | *(none)* |
| **Requires** | *(none)* |

## Configuration

No config keys today — activate the plugin in `plugins.active` and the
default tools register at boot.

## Tool surface

| Tool | Arguments | Output |
|------|-----------|--------|
| `scene_create` | `schema` (string), `content` (any) | `SceneHandle` JSON |
| `scene_patch` | `scene_id`, `patch` | `SceneHandle` JSON |
| `scene_get` | `scene_id` | full `Scene` JSON (handle + content + history) |
| `scene_list` | *(none)* | array of `SceneHandle` |
| `scene_delete` | `scene_id` | `{"deleted":true}` |

Map patches merge shallow (keys in the patch overwrite); non-map patches
replace content. Schema-specific renderers wanting richer merge semantics
can plug a custom `Patcher` into the in-process `scene.MemoryStore`.

## Persistence

| File | When | Contents |
|------|------|----------|
| `<session>/plugins/nexus.scene/scenes.jsonl` | Per mutation | JSONL record of every create / patch / delete. Replay reads this to reconstruct historical state. |
| `<session>/plugins/nexus.scene/scenes.json` | On `Shutdown` | Full snapshot of every scene at session end. Loaded on next `Init` for clean restart resume. |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handle invocations of the `scene_*` tools. |

### Emits

| Event | When |
|-------|------|
| `tool.register` | Registers each `scene_*` tool at boot. |
| `tool.result` / `before:tool.result` | Per tool call. |
| `scene.created` | Per `scene_create`. Payload includes `content` (the initial content). |
| `scene.patched` | Per `scene_patch`. Payload includes `content` (the full post-merge content). |
| `scene.deleted` | Per `scene_delete`. |

`scene.created` and `scene.patched` carry the scene's full post-mutation
`content` so bus consumers (e.g. the AG-UI transport's shared-state mirror) can
track scene state without a tool call. The scene store's own patch semantics are
shallow-merge, not RFC 6902, so a consumer needing an RFC 6902 delta diffs the
full content itself.

`agent_id` on scene events flows from `Event.Causation.AgentID` so
sub-agent contributions stay attributable. See
[scene events](../events/reference.md#scene-events) for payload shape.

## Example

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.scene
    - nexus.memory.capped

  nexus.agent.react:
    system_prompt: |
      When building visual output (charts, dashboards, multi-section
      documents), use the scene_create / scene_patch tools to build
      durable, addressable artifacts the UI can render incrementally.
```
