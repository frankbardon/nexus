# Architecture Overview

Nexus follows a strict event-driven architecture. The engine is intentionally minimal — it provides the event bus, plugin registry, lifecycle management, and session workspace. **All behavior comes from plugins.**

## Core Principle

Plugins never call each other directly. Every interaction flows through the central event bus as typed events. This keeps plugins decoupled and makes the system easy to extend or reconfigure.

```mermaid
flowchart TB
    subgraph Engine["🛠 Engine (pkg/engine)"]
        direction TB
        EB[EventBus]
        REG[PluginRegistry]
        LM[LifecycleManager]
        EB --- REG --- LM
        DISPATCH{{Event Dispatch}}
        EB --> DISPATCH
    end

    DISPATCH --> IO[IO Plugins<br/>tui · browser · wails]
    DISPATCH --> AG[Agent Plugins<br/>react · planexec · orchestrator]
    DISPATCH --> LLM[LLM Providers<br/>anthropic · openai · fallback]
    DISPATCH --> TL[Tool Plugins<br/>shell · file · web · knowledge_search]
    DISPATCH --> MEM[Memory Plugins<br/>capped · summary · longterm · vector]
    DISPATCH --> OBS[Observers<br/>logger · otel · thinking]

    classDef engine fill:#1e3a5f,stroke:#4a90e2,stroke-width:2px,color:#fff;
    classDef plugin fill:#2d4a3e,stroke:#5fb878,stroke-width:1.5px,color:#fff;
    classDef dispatch fill:#4a3a5f,stroke:#9b59b6,stroke-width:2px,color:#fff;
    class EB,REG,LM engine;
    class IO,AG,LLM,TL,MEM,OBS plugin;
    class DISPATCH dispatch;
```

## Engine Components

The engine (`pkg/engine/`) contains these components:

| Component | File | Purpose |
|-----------|------|---------|
| **Engine** | `engine.go` | Top-level orchestrator that wires everything together |
| **EventBus** | `bus.go` | Central event dispatch with priority ordering and filtering |
| **PluginRegistry** | `registry.go` | Stores plugin factories, creates instances on demand |
| **LifecycleManager** | `lifecycle.go` | Boots plugins in dependency order, shuts down in reverse |
| **SessionWorkspace** | `session.go` | File-based session persistence |
| **ModelRegistry** | `models.go` | Resolves model role names to provider/model/token configs |
| **PromptRegistry** | `prompt.go` | Dynamic system prompt assembly from plugin sections |
| **ContextManager** | `context.go` | Agent context management (placeholder for future windowing) |
| **SystemInfo** | `system.go` | Platform detection (OS, architecture, open commands) |
| **Config** | `config.go` | YAML configuration loading and merging |

## Boot Sequence

When `Engine.Run()` is called:

1. **Config loaded** — YAML file is parsed, defaults merged, per-plugin configs extracted
2. **Session created** — A new session workspace is set up on disk (or an existing one is resumed)
3. **`core.boot` emitted** — Signals the start of the boot process
4. **Plugins initialized** — Topologically sorted by dependencies, then `Init()` called serially
5. **Plugins readied** — `Ready()` called in parallel on all initialized plugins
6. **`core.ready` emitted** — All plugins are up and listening
7. **Event loop** — The engine listens for events until a shutdown signal arrives
8. **Shutdown** — Plugins shut down in reverse dependency order, `core.shutdown` emitted

If `Boot` returns an error, the caller does not call `Stop`. Boot undoes its own
partial work on the way out: it releases the session lock and the object-store
handle, and it unsubscribes every engine-owned run-scoped handler installed before
the failing step. That covers the journal wildcard, the tool cache and the cost-tag
seeder. It also closes the journal writer, so its drain goroutine and open
segment file do not outlive the failure. So a host that retries `Boot` on the same
`Engine` gets each of those handlers registered exactly once and a fresh journal
writer ([#206](https://github.com/frankbardon/nexus/issues/206)).

Plugins get the same treatment. When a plugin's `Init` or any plugin's `Ready`
fails, `Boot` calls `Shutdown` on every plugin whose `Init` already succeeded, in
reverse init order, before it returns. A plugin whose own `Init` failed is not shut
down; instead the engine removes every bus subscription that plugin made before
its `Init` returned the error. It does this by recording the subscriptions made
during each plugin's `Init` (typed, `SubscribeAll` and `SubscribeAllReplay`) and
unsubscribing them, newest first, when that `Init` fails, so a plugin that
subscribes and then fails leaves no handler behind and needs no cleanup code of
its own ([#222](https://github.com/frankbardon/nexus/issues/222)). Only
subscriptions are undone this way. Anything else a failing `Init` opened (a file,
a goroutine, a listener) is still the plugin's to release before it returns the
error. On a `Ready` failure every plugin has finished `Init`, so all of them are
shut down, the failing one included. No `core.shutdown` is emitted and the bus is
not drained. `Shutdown` gets a context detached from Boot's own, bounded by
`engine.shutdown.drain_timeout`. A `Shutdown` error or panic is logged and never
replaces the Boot error the caller receives.

This means **a plugin's `Shutdown` must tolerate running after `Init` without
`Ready`.** Anything `Ready` sets up (a started server, a watcher, a goroutine) may
not exist yet, so nil-check it. Unsubscribing what `Init` subscribed is the main
job here: it is what keeps a retried `Boot` from registering the plugin's
handlers twice.

```mermaid
sequenceDiagram
    autonumber
    participant Caller as Caller<br/>(CLI / Embedder)
    participant Engine
    participant Session
    participant Bus as EventBus
    participant Plugins

    Caller->>Engine: Run(ctx)
    Engine->>Engine: Load YAML config
    Engine->>Session: Create session workspace
    Engine->>Bus: emit core.boot
    Bus->>Plugins: Init() in dependency order
    Plugins-->>Bus: subscriptions registered
    Engine->>Plugins: Ready() in parallel
    Engine->>Bus: emit core.ready
    Note over Bus,Plugins: Event loop —<br/>plugins drive behavior
    Caller-->>Engine: SIGINT / Stop()
    Engine->>Plugins: Shutdown() in reverse order
    Engine->>Bus: emit core.shutdown
```

## Event Flow Example

Here's a typical request flow through the system:

```mermaid
sequenceDiagram
    autonumber
    actor User
    participant IO as nexus.io.tui
    participant Agent as nexus.agent.react
    participant LLM as nexus.llm.anthropic
    participant Gates as before:* gates
    participant Tool as nexus.tool.shell

    User->>IO: types message
    IO->>Agent: io.input
    Agent->>LLM: llm.request
    LLM->>LLM: call Claude API
    LLM-->>Agent: llm.response

    alt response contains tool calls
        Agent->>Gates: before:tool.invoke (vetoable)
        Gates-->>Agent: pass
        Agent->>Tool: tool.invoke
        Tool->>Tool: execute
        Tool->>Gates: before:tool.result (vetoable)
        Gates-->>Tool: pass
        Tool-->>Agent: tool.result
        Agent->>LLM: llm.request (loop)
    else final answer
        Agent->>IO: io.output
        IO-->>User: display response
    end
```

## Key Design Decisions

### Synchronous Dispatch
Events are dispatched synchronously — handlers execute one at a time, ordered by priority. This makes the system predictable and avoids race conditions.

### Vetoable Events
Events prefixed with `before:` are vetoable. Any handler can block the action by setting a veto on the payload. This enables approval workflows (e.g., confirming tool execution).

### Plugin Dependencies
Plugins declare their dependencies by ID. The lifecycle manager topologically sorts them to ensure correct init order. Circular dependencies cause a boot failure.

### Multi-Instance Plugins
Some plugins (like `nexus.agent.subagent`) support multiple instances via ID suffixes. For example, `nexus.agent.subagent/researcher` creates an instance with `InstanceID` set to the full suffixed ID.

## Next Steps

- [Event Bus](./event-bus.md) — How events are dispatched, filtered, and prioritized
- [Plugin System](./plugin-system.md) — The plugin interface, lifecycle, and how to write your own
- [Sessions](./sessions.md) — How session data is persisted to disk
