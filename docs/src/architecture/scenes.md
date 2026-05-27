# Scenes

A **Scene** is a named, structured, mutable entity that lives for the
lifetime of a session. Agents use scenes to construct durable visual output
(charts, dashboards, multi-section documents) that is addressable across
tool calls, patchable over time, and persisted to disk.

The runtime is schema-agnostic — Nexus stores the content blob and journals
patches; downstream renderers (UIs, exporters) interpret the schema-specific
content.

## Schema

```go
type SceneHandle struct {
    ID        string // Stable, session-scoped — "scene_<hex>"
    SessionID string
    Schema    string // Names the schema the content conforms to
    Version   int    // Incremented on each patch
}

type Scene struct {
    Handle    SceneHandle
    Content   any          // Current state
    CreatedAt time.Time
    UpdatedAt time.Time
    History   []SceneEvent
}

type SceneEvent struct {
    Sequence  int
    Timestamp time.Time
    AgentID   string
    Patch     any
    Initial   bool
}
```

`pkg/scene` defines these types plus the `Store` interface that
`nexus.scene` implements with a goroutine-safe in-memory backend.

## Behavior

- **Stable IDs.** Scene IDs are session-scoped and never change after
  creation. Agents reference them by ID in subsequent tool calls.
- **Patches are journaled.** Every patch appends a `SceneEvent` to the
  scene's in-memory history and a JSONL line to
  `<session>/plugins/nexus.scene/scenes.jsonl`. This is the substrate the
  [replay primitive](./replay.md) reads to reconstruct historical state.
- **Bus events.** Creation, patching, and deletion each emit a `scene.*`
  event — see the [events reference](../events/reference.md#scene-events).
  `agent_id` flows from `Event.Causation.AgentID` so a sub-agent's
  contribution is attributable.
- **Schema is advisory.** The runtime does not validate content against
  the named schema; renderers do.
- **Linearization.** Concurrent patches (parent + sub-agent, two parallel
  sub-agents) serialize through the store mutex. First patch at a given
  key wins; later patches see post-first-patch state.

## Patcher

Patches merge through a `Patcher` implementation. The default is
`ShallowMerge`:

- Map patch + map content → key-by-key merge, patch keys overwrite.
- Anything else → patch replaces content entirely.

Schema-specific renderers that need richer semantics can swap in their own
`Patcher` via `MemoryStore.WithPatcher`.

## Tool surface

`nexus.scene` registers five tools the LLM uses to manipulate scenes:

| Tool | Arguments | Output |
|------|-----------|--------|
| `scene_create` | `schema`, `content` | `SceneHandle` JSON |
| `scene_patch` | `scene_id`, `patch` | `SceneHandle` JSON |
| `scene_get` | `scene_id` | full `Scene` JSON (handle + content + history) |
| `scene_list` | *(none)* | array of `SceneHandle` |
| `scene_delete` | `scene_id` | `{"deleted":true}` |

## Persistence

- Per-patch JSONL append to `scenes.jsonl` — the durable source of truth
  for time-travel reconstruction.
- Full state snapshot to `scenes.json` on `Shutdown` — what a clean
  restart loads to pick up where the prior run left off.

Sessions configured to drop scene history can compact `scenes.jsonl` to
the current state only; correctness does not depend on a complete log.

## Configuration

See [`nexus.scene`](../configuration/reference.md#nexusscene) in the
configuration reference. No config keys today — activate the plugin and
the default tools register at boot.
