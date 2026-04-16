# Wails Desktop IO

A Wails-native IO transport for embedding Nexus inside a desktop webview
shell. Unlike `nexus.io.browser`, which is a session-scoped dev-mode
transport owned by the stock `nexus` binary, `nexus.io.wails` is
process-scoped and owned by a host Wails application.

Nexus does **not** take a build-time dependency on
`github.com/wailsapp/wails/v2/pkg/runtime`. The plugin defines a small
`Runtime` interface and the downstream Wails app hands in a wrapper
around `runtime.EventsEmit` / `runtime.EventsOn` before calling
`engine.Boot`.

## Details

| | |
|---|---|
| **ID** | `nexus.io.wails` |
| **Dependencies** | None |
| **Status** | Production (config-driven event bridging, multi-agent scoping) |

## Configuration

The plugin supports two modes:

**Legacy mode** (no config keys): falls back to hardcoded chat-event
subscriptions (`io.output`, `io.input`, etc.) for backward compatibility.

**Config-driven mode**: explicit `subscribe` and `accept` lists control
which events are bridged:

```yaml
plugins:
  nexus.io.wails:
    # Events bridged outbound: bus → frontend
    subscribe:
      - "match.result"
      - "hello.response"
      - "ui.state.restore"
      - "io.file.selected"
      - "session.file.created"
    # Events accepted inbound: frontend → bus
    accept:
      - "match.request"
      - "hello.request"
      - "ui.state.save"
      - "io.file.selected"
```

## Events

In config-driven mode, the plugin bridges whatever events are listed in
`subscribe` (outbound: Go → JS) and `accept` (inbound: JS → Go). Custom
domain events use a generic passthrough handler. The existing typed
handlers (`handleOutput`, `handleStreamChunk`, etc.) remain for chat
events that need special mapping.

## Architecture

- **Hub** — A single-client transport wrapper holding the `Runtime`
  implementation installed by the embedder. No fanout, no client map,
  no lifecycle — a Wails app has exactly one attached webview for its
  process lifetime.
- **Adapter** — Implements [`ui.UIAdapter`](../../architecture/ui-adapter.md)
  by marshaling outbound messages into `ui.Envelope` and calling
  `Hub.BroadcastEnvelope`, which in turn calls
  `Runtime.EmitEvent("nexus", envelopeJSON)`.
- **Plugin** — Wiring layer that subscribes to configured events and
  translates inbound events from the webview onto the Nexus bus.

The `Runtime` interface is deliberately minimal:

```go
type Runtime interface {
    EmitEvent(name string, optionalData ...any)
    OnEvent(name string, callback func(optionalData ...any))
}
```

### Multi-agent scoping

When a desktop shell runs multiple agent engines, the shell provides
a **scoped `Runtime`** adapter per agent. The scoped runtime prepends
the agent ID to event channels:

- Outbound: `"{agentID}:nexus"` instead of `"nexus"`
- Inbound: `"{agentID}:nexus.input"` instead of `"nexus.input"`

The plugin itself is unaware of scoping — it talks to its `Runtime`,
and the `Runtime` implementation handles the namespace. No plugin
changes needed for multi-agent.

## Using `pkg/desktop/` (recommended)

> **Full documentation**: See the [Desktop Shell](../../desktop/overview.md)
> section for architecture details, a
> [step-by-step build guide](../../desktop/building-your-app.md), and the
> complete [API reference](../../desktop/reference.md).

The desktop shell framework in `pkg/desktop/` handles all the
boilerplate of embedding Nexus in a Wails app. Each agent is
registered with its config YAML and plugin factories:

```go
desktop.Run(&desktop.Shell{
    Title:  "My App",
    Width:  900,
    Height: 720,
    Assets: assets,
    Agents: []desktop.Agent{{
        ID:         "my-agent",
        Name:       "My Agent",
        ConfigYAML: configYAML,
        Factories: map[string]func() engine.Plugin{
            "nexus.io.wails":   wailsio.New,
            "my.custom.plugin": myplugin.New,
        },
    }},
})
```

The shell handles:
- Per-agent engine lifecycle (lazy boot on first selection)
- Scoped `Runtime` adapters for multi-agent event isolation
- Singleton-factory registration for the wails plugin
- Shell services (file dialogs, OS notifications, etc.)
- Left-nav navigation for multi-agent apps
- Settings UI with agent-contributed schemas, keychain secrets, and `${var}` config injection
- Session history: per-agent session list, recall, new session, cleanup

See `cmd/desktop/` for the reference multi-agent app.

### Agent-contributed settings

Agents declare configurable fields via `Settings []SettingsField` on the
`Agent` struct. The shell renders a settings UI from the schema, persists
values to `~/.nexus/desktop/settings.json` (plaintext) and the OS
keychain (secrets), and resolves `${var}` placeholders in the agent's
`ConfigYAML` before engine creation.

```go
desktop.Agent{
    ID:         "my-agent",
    ConfigYAML: configYAML, // contains ${shell.anthropic_api_key}, ${data_dir}
    Settings: []desktop.SettingsField{
        {
            Key:      "shell.anthropic_api_key",
            Display:  "Anthropic API Key",
            Type:     desktop.FieldString,
            Secret:   true,
            Required: true,
        },
        {
            Key:     "data_dir",
            Display: "Data Folder",
            Type:    desktop.FieldPath,
            Required: true,
        },
    },
}
```

**Scope fallback:** Settings keys prefixed with `shell.` are stored in
shell scope and shared across agents. During resolution, the shell
checks agent scope first, then falls back to shell scope. This means
an API key entered once under "shell" is available to all agents.

**Required field gating:** If an agent has required settings with no
value and no default, the shell refuses to boot the engine and redirects
the user to the settings page with missing fields highlighted.

### Session history

The shell tracks session history per agent in
`~/.nexus/desktop/sessions.json`. Each engine boot creates a new session
entry; agents contribute metadata via bus events:

- **`session.meta.title`** — Human-readable session title (e.g.
  "Match: Senior Go Engineer"). Emitted by the agent plugin after a
  meaningful action completes.
- **`session.meta.preview`** — Agent-specific summary data for the
  session list (e.g. `{ candidateCount: 5, topCandidate: "Jane" }`).
- **`session.meta.status`** — Explicit status change (e.g.
  `{ status: "completed" }`). Also inferred from `io.session.end`.

The shell subscribes to these events on the engine's bus after boot and
updates the session index. The frontend receives updates via
`{agentID}:sessions.updated` Wails events.

**Session lifecycle:**
- **New session:** `Shell.NewSession(agentID)` stops the current engine
  and boots a fresh one.
- **Recall:** `Shell.RecallSession(agentID, sessionID)` stops the
  current engine, creates a new one with `RecallSessionID` set, and
  boots it. The engine replays conversation history via
  `io.history.replay`.
- **Delete:** `Shell.DeleteSession(agentID, sessionID)` removes the
  session from the index and deletes the engine session directory.
- **Cleanup:** On startup, the shell removes sessions older than the
  configured retention period (default 30 days) and reconciles
  orphaned engine directories.

### UI state persistence

The shell provides a framework-agnostic mechanism for frontends to
persist and restore UI state across sessions via two bus events:

- **`ui.state.save`** (inbound: frontend → bus) — The frontend emits
  this event with an opaque `{ state: { ... } }` payload whenever it
  wants to checkpoint its UI state. The shell writes the payload to
  `ui-state.json` in the engine session directory
  (`~/.nexus/sessions/<id>/ui-state.json`).

- **`ui.state.restore`** (outbound: bus → frontend) — On session recall,
  after the engine boots, the shell reads `ui-state.json` (if it exists)
  and emits this event onto the bus. The frontend listens for it and
  rehydrates its state from the payload.

Both events must be included in the wails IO plugin's config-driven
`accept` and `subscribe` lists:

```yaml
plugins:
  nexus.io.wails:
    subscribe:
      - "ui.state.restore"
    accept:
      - "ui.state.save"
```

The payload structure is entirely up to the frontend — the shell treats
it as an opaque JSON blob. This means the mechanism works regardless of
whether the frontend uses Alpine.js, React, vanilla JS, or any other
framework.

### File portal

The desktop shell provides a file portal layer that gives agents a
consistent way to access files without navigating the raw filesystem.
Agents declare `input_dir` and `output_dir` settings; the shell uses
these to root file dialogs, list directory contents, and provide a
file browser panel.

**Shell-bound methods:**

- `ListFiles(agentID, filter)` — Non-recursive listing of the agent's
  `input_dir`, filtered by glob pattern.
- `OutputDir(agentID)` — Resolve and create the agent's `output_dir`.
- `CopyFileToInputDir(agentID, sourcePath)` — Copy a file into the
  agent's `input_dir` (used for drag-and-drop).
- `WatchInputDir(agentID)` — Start fsnotify watcher, emits
  `{agentID}:files.changed` on file create/remove/rename.

**Bus events:**

| Event | Direction | Purpose |
|-------|-----------|---------|
| `io.file.open.request` | Plugin → shell | Request a file dialog (existing) |
| `io.file.open.response` | Shell → plugin | File dialog result (existing) |
| `io.file.output_dir.request` | Plugin → shell | Ask where to write outputs |
| `io.file.output_dir.response` | Shell → plugin | Output directory path |
| `io.file.selected` | Shell → plugin | User selected a file in the browser panel |
| `session.file.created` | Plugin → shell | Agent wrote an output file (existing) |

**Directory resolution priority:**

1. Agent-scoped `input_dir` setting
2. Shell-scoped `shared_data_dir` setting
3. User's `~/Documents` directory

**File browser panel (frontend):**

The reference desktop app includes a right-side collapsible file
browser panel that shows the active agent's `input_dir` contents. It
supports click-to-select (emits `io.file.selected`), open in default
app, reveal in finder, and drag-and-drop file import from the OS.
The panel auto-refreshes via `fsnotify` when files change on disk.

## Manual embedding (advanced)

If you need more control than `pkg/desktop/` provides:

### 1. Singleton-factory registration

```go
p := wailsio.New().(*wailsio.Plugin)
eng.Registry.Register("nexus.io.wails", func() engine.Plugin { return p })
```

### 2. Install runtime before boot

```go
p.Hub().SetRuntime(&wailsRuntime{ctx: ctx})
eng.Boot(ctx)
```

### 3. Frontend bus helper

```js
const bus = createBus('my-agent');
bus.on('my.response', (data) => { /* handle */ });
bus.emit('my.request', { /* payload */ });
const result = await bus.call('my.request', 'my.response', payload);
```

## Known gotchas

- **Factory-per-boot**: Always use a singleton factory closure for the
  Wails plugin. `LifecycleManager.Boot` calls factories once per boot.
- **Boot ordering**: `SetRuntime` must happen before `Boot`.
- **`OnStartup` timing**: the webview may not be fully attached the
  instant `OnStartup` fires. If you see dropped events during the first
  tick, defer `Boot` one event loop iteration.
- **Do not call `eng.Run`**: Embedders must use `Boot`/`Stop` directly.
  `Run` installs its own signal handler, which conflicts with Wails.
