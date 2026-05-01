# Plugin System Deep Reference

## Embedder API (library use)

Nexus embeds in another Go process (e.g. Wails desktop shell) without owning signals or process lifecycle. Embedder path:

1. `engine.New(configPath)` — creates engine. `configPath == ""` uses `DefaultConfig()`.
2. `eng.Registry.Register(id, factory)` — register plugins embedder wants. Embedder can mutate `eng.Config.Plugins.Active` and `eng.Config.Plugins.Configs` in memory; no on-disk YAML needed.
3. `eng.Boot(ctx)` — non-blocking. Starts session, boots plugins via lifecycle manager, installs run-scoped bus subs, starts tick heartbeat. Returns when engine ready.
4. `<-eng.SessionEnded()` — channel signals when plugin emits `io.session.end`. Embedders `select` on this plus own lifecycle signals.
5. `eng.Stop(ctx)` — tears down tick goroutine, unsubs run-scoped handlers, finalizes session metadata, shuts plugins in reverse dep order.

`eng.Run(ctx)` = CLI convenience wrapper: `Boot` + wait-for-signal-or-session-end + `Stop`. **Embedders must call `Boot`/`Stop` direct, never `Run`** — `Run` installs own `SIGINT`/`SIGTERM` handler, conflicts with host process (Wails, tests) owning signals.

## Auto-activation (`Requires()`)

`Requires()` lets a plugin declare sibling plugins it needs to function and the default config to use when the user has not configured them. At boot, the lifecycle manager walks `Requires()` transitively starting from the user-declared active list and appends any missing IDs. This is separate from `Dependencies()`: `Dependencies()` only validates boot order, `Requires()` activates.

Each `Requirement` carries **exactly one** of `ID` (concrete plugin ID — e.g. `nexus.memory.capped`) or `Capability` (abstract capability name — e.g. `memory.history`). Both set on the same `Requirement` fails boot. `Default` and `Optional` apply to whichever form you pick.

**Merge rule (whole-object replace — no field-level merge).** If the user has supplied **any** config for the resolved ID, the user's config wins entirely and the Requirement's `Default` is discarded. If the user has not supplied a config, `Default` is installed as-is. This keeps precedence predictable.

**Optional requirements.** `Requirement.Optional: true` causes the engine to skip a missing factory (ID form) or missing capability provider with a `WARN` log and continue booting. `Optional: false` (the default) fails boot.

**Visibility.** Every auto-activation emits an `INFO` log at boot: `"auto-activating plugin X (required by Y); config source: default|user-override|empty"`, with `capability` and `capability_source` fields added when the activation was driven by a capability. A single `"active plugin set resolved"` line at the end of expansion annotates every entry as `[user]` or `[auto: required-by=Z,config=...]`. After expansion, one `"capability resolved"` INFO line is emitted per capability naming the providers and whether the resolution came from explicit config or the active list. Observers (the always-on journal, OTel) pick these up through the standard structured fields.

**Currently declared (non-nil) Requires():**
- `nexus.agent.react` → `Capability: memory.history` (default: `max_messages: 100, persist: true`), `Capability: control.cancel`, `Capability: tool.catalog`
- `nexus.agent.subagent`, `nexus.agent.orchestrator` — still use `Dependencies()`; will migrate as part of the same dedup if their state machines warrant it.

## Capabilities (`Capabilities()`, `Requirement.Capability`)

Plugins advertise abstract capabilities via `Capabilities() []engine.Capability`. Consumers then `Requires()` a capability name rather than a concrete plugin ID, letting the engine resolve an appropriate provider at boot.

```go
// Provider: advertise what I do.
func (p *Plugin) Capabilities() []engine.Capability {
    return []engine.Capability{{
        Name:        "memory.history",
        Description: "LLM-native conversation history for the active session.",
    }}
}

// Consumer: I need whatever plugin provides memory.history.
func (p *Plugin) Requires() []engine.Requirement {
    return []engine.Requirement{{
        Capability: "memory.history",
        Default:    map[string]any{"max_messages": 100, "persist": true},
    }}
}
```

**Resolution order** (boot-time, inside `expandRequirements`):

1. **Explicit pin.** Top-level `capabilities:` block in config pins `capability → plugin-ID`. The pinned provider must either be in the active list or have a registered factory that advertises the capability. If neither, boot fails.
2. **Active list.** First plugin in `plugins.active` that advertises the capability wins.
3. **Auto-activate.** If no active plugin advertises it, the engine walks the registry and picks the alphabetically first factory that does. When more than one candidate exists, a `WARN` names every candidate so operators know to pin one.

```yaml
# Pin a specific provider when multiple are registered.
capabilities:
  memory.history: nexus.memory.capped

plugins:
  active:
    - nexus.agent.react
    # memory.history provider auto-activates per the pin above.
```

**Introspection.** `eng.Capabilities() map[string][]string` returns the resolved capability → provider-IDs map after boot. Each plugin receives the same map through `PluginContext.Capabilities` at `Init` — prefer checking `ctx.Capabilities["control.cancel"]` over string-matching specific plugin IDs.

**Currently advertised capabilities:**
- `memory.history` — `nexus.memory.simple`, `nexus.memory.capped` (default), `nexus.memory.summary_buffer`
- `memory.compaction` — `nexus.memory.compaction`, `nexus.memory.summary_buffer` (inline)
- `memory.longterm` — `nexus.memory.longterm`
- `control.cancel` — `nexus.control.cancel`
- `tool.catalog` — `nexus.tool.catalog`
- `search.provider` — `nexus.search.brave`, `nexus.search.anthropic_native`, `nexus.search.openai_native` (required by `nexus.tool.web`; pin with `capabilities.search.provider` when multiple are active)
