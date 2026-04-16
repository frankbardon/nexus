# Building Your Desktop App

This guide walks through creating a Nexus desktop application from
scratch. By the end you will have a Wails app hosting a custom agent
with settings, session management, and a frontend that communicates
with the agent through the bus bridge.

The reference implementation at `cmd/desktop/` demonstrates every
feature covered here. When in doubt, consult it as the living example.

## Prerequisites

- Go 1.22+
- [Wails v2 CLI](https://wails.io/docs/gettingstarted/installation)
  (`go install github.com/wailsapp/wails/v2/cmd/wails@latest`)
- Nexus as a Go module dependency

## Project structure

A typical desktop app looks like this:

```
cmd/my-app/
  main.go                  # Entry point — registers agents, calls desktop.Run
  config.yaml              # Embedded Nexus config for your agent
  internal/
    myplugin/              # Your domain plugin(s)
      plugin.go
      events.go
  frontend/
    dist/                  # Your frontend assets (HTML/CSS/JS)
      index.html
  build/                   # Wails build artifacts (icons, Info.plist)
```

## Step 1: Create your domain plugin

Your agent's behavior lives in a standard Nexus plugin. It subscribes
to events from the frontend, does work, and emits results back.

```go
package myplugin

import (
    "github.com/frankbardon/nexus/pkg/engine"
)

type Plugin struct {
    bus    engine.EventBus
    logger engine.Logger
}

func New() engine.Plugin { return &Plugin{} }

func (p *Plugin) ID() string { return "myapp.agent.worker" }

func (p *Plugin) Init(ctx engine.PluginContext) error {
    p.bus = ctx.Bus
    p.logger = ctx.Logger
    return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
    return []engine.EventSubscription{
        {EventType: "work.request", Handler: p.handleRequest},
    }
}

func (p *Plugin) Emissions() []string {
    return []string{"work.result", "session.meta.title"}
}

func (p *Plugin) handleRequest(event engine.Event[any]) {
    payload, _ := event.Payload.(map[string]any)
    input, _ := payload["input"].(string)

    // Do your work here...

    p.bus.Emit("work.result", map[string]any{
        "output": "Processed: " + input,
    })

    // Contribute session metadata so the session list shows
    // a meaningful title instead of "Untitled".
    p.bus.Emit("session.meta.title", map[string]any{
        "title": "Work: " + input,
    })
}
```

## Step 2: Write your agent config

The config YAML declares which plugins are active and how the
`nexus.io.wails` plugin bridges events. Use `${var}` placeholders for
values that come from user settings.

```yaml
# config.yaml
core:
  log_level: info
  tick_interval: 5s

  sessions:
    root: ~/.nexus/sessions
    retention: 30d
    id_format: datetime_short

plugins:
  active:
    - nexus.io.wails
    - myapp.agent.worker

  nexus.io.wails:
    # Events bridged outbound: bus -> frontend
    subscribe:
      - "work.result"
      - "ui.state.restore"
    # Events accepted inbound: frontend -> bus
    accept:
      - "work.request"
      - "ui.state.save"

  myapp.agent.worker:
    api_key: "${shell.api_key}"
    data_dir: "${data_dir}"
```

**Config-driven bridging is explicit.** Only events listed in
`subscribe` and `accept` cross the bus-to-frontend boundary. If your
frontend isn't receiving an event, check these lists first.

## Step 3: Write your entry point

The entry point registers agents and calls `desktop.Run`. Config YAML
is embedded at compile time — the shipped binary has no filesystem
dependency on config files.

```go
package main

import (
    "embed"
    "log"

    "github.com/frankbardon/nexus/pkg/desktop"
    "github.com/frankbardon/nexus/pkg/engine"
    wailsio "github.com/frankbardon/nexus/plugins/io/wails"

    "my-module/cmd/my-app/internal/myplugin"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed config.yaml
var agentConfig []byte

func main() {
    if err := desktop.Run(&desktop.Shell{
        Title:  "My App",
        Width:  1024,
        Height: 768,
        Assets: assets,
        Agents: []desktop.Agent{
            {
                ID:          "my-agent",
                Name:        "My Agent",
                Description: "Does useful work",
                Icon:        "fa-solid fa-gear",
                ConfigYAML:  agentConfig,
                Factories: map[string]func() engine.Plugin{
                    "nexus.io.wails":    wailsio.New,
                    "myapp.agent.worker": myplugin.New,
                },
                Settings: []desktop.SettingsField{
                    {
                        Key:      "shell.api_key",
                        Display:  "API Key",
                        Type:     desktop.FieldString,
                        Secret:   true,
                        Required: true,
                    },
                    {
                        Key:      "data_dir",
                        Display:  "Data Folder",
                        Type:     desktop.FieldPath,
                        Required: true,
                    },
                },
            },
        },
    }); err != nil {
        log.Fatalf("app: %v", err)
    }
}
```

### Key registration details

- **`nexus.io.wails` must be in `Factories`.** The framework needs
  to intercept this plugin to install the scoped runtime before boot.
  Always register it via the factories map.
- **Every plugin in `plugins.active` needs a factory.** Either through
  `Agent.Factories` (for custom and wails plugins) or the engine's
  built-in registry (for stock Nexus plugins like `nexus.llm.anthropic`).
- **`Assets` is optional.** If omitted (zero-value `embed.FS`), the
  framework uses its built-in base template. For anything beyond a
  demo, provide your own.

## Step 4: Build your frontend

Your frontend communicates with the agent through scoped Wails events.
The pattern:

```js
// Create a bus helper scoped to your agent ID.
function createBus(agentID) {
    return {
        // Listen for events from the Go side.
        on(eventType, callback) {
            window.runtime.EventsOn(
                `${agentID}:nexus`,
                (envelopeJSON) => {
                    const envelope = JSON.parse(envelopeJSON);
                    if (envelope.type === eventType) {
                        callback(envelope.payload);
                    }
                }
            );
        },

        // Send an event to the Go side.
        emit(eventType, payload) {
            window.runtime.EventsEmit(
                `${agentID}:nexus.input`,
                JSON.stringify({ type: eventType, payload })
            );
        },

        // Request-response pattern: emit a request, wait for
        // a specific response event type.
        call(requestType, responseType, payload) {
            return new Promise((resolve) => {
                const off = window.runtime.EventsOnce(
                    `${agentID}:nexus`,
                    (envelopeJSON) => {
                        const envelope = JSON.parse(envelopeJSON);
                        if (envelope.type === responseType) {
                            resolve(envelope.payload);
                        }
                    }
                );
                this.emit(requestType, payload);
            });
        }
    };
}
```

Usage in your UI:

```js
const bus = createBus('my-agent');

// Listen for results.
bus.on('work.result', (data) => {
    document.getElementById('output').textContent = data.output;
});

// Send a request.
document.getElementById('submit').addEventListener('click', () => {
    const input = document.getElementById('input').value;
    bus.emit('work.request', { input });
});
```

### Calling shell methods

Shell services (settings, sessions, file dialogs) are Wails-bound Go
methods on the `Shell` struct. Call them from JavaScript via the
generated Wails bindings:

```js
// List available agents.
const agents = await go.desktop.Shell.ListAgents();

// Boot an agent.
await go.desktop.Shell.EnsureAgentRunning('my-agent');

// Open a native file dialog.
const path = await go.desktop.Shell.PickFile('my-agent', 'Select File', '*.pdf');

// Manage sessions.
const sessions = await go.desktop.Shell.ListSessions('my-agent');
await go.desktop.Shell.NewSession('my-agent');
await go.desktop.Shell.RecallSession('my-agent', sessionID);
await go.desktop.Shell.DeleteSession('my-agent', sessionID);
```

### Session list updates

Session list changes arrive as Wails events (not bus events):

```js
window.runtime.EventsOn('my-agent:sessions.updated', (jsonStr) => {
    const sessions = JSON.parse(jsonStr);
    renderSessionList(sessions);
});
```

## Step 5: Settings

Agents declare settings via `Settings []SettingsField` on the `Agent`
struct. The framework handles persistence, UI rendering, and config
injection.

### Field types

| Type | Constant | UI Control |
|------|----------|------------|
| Single-line text | `FieldString` | Text input |
| File/folder path | `FieldPath` | Text input + Browse button |
| Multi-line text | `FieldText` | Textarea |
| Number | `FieldNumber` | Number input (optional min/max) |
| Boolean | `FieldBool` | Toggle switch |
| Dropdown | `FieldSelect` | Select with `Options` |

### Scope and sharing

Settings keys prefixed with `shell.` are stored in shell scope and
shared across all agents. During config resolution, the framework
checks agent scope first, then falls back to shell scope.

Common pattern: declare `shell.api_key` as a required secret on every
agent that needs it. The user enters it once — the shell-scoped value
resolves for all agents.

```go
Settings: []desktop.SettingsField{
    {
        Key:      "shell.api_key",    // shell-scoped: shared
        Display:  "API Key",
        Type:     desktop.FieldString,
        Secret:   true,
        Required: true,
    },
    {
        Key:      "data_dir",         // agent-scoped: per-agent
        Display:  "Data Folder",
        Type:     desktop.FieldPath,
        Required: true,
    },
},
```

### Secrets

Fields with `Secret: true` are stored in the OS keychain (macOS
Keychain, Windows Credential Manager, Linux Secret Service) via
`go-keyring`. The JSON settings file stores the sentinel
`"__keychain__"` so the frontend knows a value exists without
exposing it.

### Required field gating

If an agent has required settings with no value and no default, the
framework refuses to boot the engine. The frontend should check
`HasMissingRequired()` and redirect to the settings page.

### Config injection

`${var}` placeholders in config YAML are replaced with resolved
settings values before the engine is created. The resolution order:

1. Agent-scoped value for the key
2. Shell-scoped value for the key (fallback)
3. `Default` from the `SettingsField` definition
4. If still unresolved and `Required: true` — boot fails with an
   error listing the missing keys

## Step 6: Session management

The framework tracks session history per agent automatically.

### How it works

Every `bootAgent` call creates a session index entry in
`~/.nexus/desktop/sessions.json`. Your agent contributes metadata
via bus events:

| Event | Payload | Purpose |
|-------|---------|---------|
| `session.meta.title` | `{ "title": "..." }` | Human-readable title for the session list |
| `session.meta.preview` | `{ ... }` | Agent-specific summary data (opaque) |
| `session.meta.status` | `{ "status": "..." }` | Explicit status change |

The shell subscribes to these events on each engine's bus and updates
the index. The frontend receives updates via
`{agentID}:sessions.updated` Wails events.

### Session lifecycle

| Action | Method | What happens |
|--------|--------|--------------|
| First select | `EnsureAgentRunning(id)` | Creates engine, boots, creates session entry |
| New session | `NewSession(id)` | Stops current engine, boots fresh |
| Recall | `RecallSession(id, sid)` | Stops current, boots with `RecallSessionID` for history replay |
| Delete | `DeleteSession(id, sid)` | Removes index entry + engine session dir |

### Cleanup

On startup, the framework removes sessions older than the configured
retention period (default 30 days, configurable via
`session_retention_days` shell setting) and reconciles orphaned engine
directories.

## Step 7: UI state persistence

The framework provides a mechanism for frontends to persist and
restore UI state across sessions.

### Save state

Emit `ui.state.save` from your frontend with an opaque state object:

```js
bus.emit('ui.state.save', {
    state: {
        selectedTab: 'results',
        scrollPosition: 450,
        formData: { name: 'Jane', role: 'Engineer' }
    }
});
```

The shell writes this to `ui-state.json` in the engine session
directory. Call this after meaningful UI interactions — not on every
keystroke.

### Restore state

On session recall, the shell emits `ui.state.restore` with the saved
payload:

```js
bus.on('ui.state.restore', (data) => {
    if (data.state) {
        applyUIState(data.state);
    }
});
```

Both events must be in the wails IO plugin's `accept`/`subscribe`
config:

```yaml
nexus.io.wails:
  subscribe:
    - "ui.state.restore"
  accept:
    - "ui.state.save"
```

## Step 8: File portal (optional)

If your agent works with files, the framework provides a standardized
file access layer.

### Declare directories

Add `input_dir` and optionally `output_dir` to your agent's settings:

```go
Settings: []desktop.SettingsField{
    {Key: "input_dir", Display: "Input Folder", Type: desktop.FieldPath, Required: true},
    {Key: "output_dir", Display: "Output Folder", Type: desktop.FieldPath},
},
```

### Use shell methods

```js
// List files in the agent's input directory.
const files = await go.desktop.Shell.ListFiles('my-agent', '*.pdf');

// Get the output directory (creates if needed).
const outDir = await go.desktop.Shell.OutputDir('my-agent');

// Copy a file into the input directory (drag-and-drop).
const dest = await go.desktop.Shell.CopyFileToInputDir('my-agent', '/path/to/file.pdf');

// Start watching for file changes.
go.desktop.Shell.WatchInputDir('my-agent');
window.runtime.EventsOn('my-agent:files.changed', () => {
    refreshFileList();
});
```

### Bus events for plugins

Plugins that need file access use bus events instead of shell methods:

| Event | Direction | Purpose |
|-------|-----------|---------|
| `io.file.output_dir.request` | Plugin to shell | Ask where to write outputs |
| `io.file.output_dir.response` | Shell to plugin | Output directory path |
| `io.file.selected` | Shell to plugin | User selected a file in the browser panel |
| `session.file.created` | Plugin to shell | Agent wrote an output file |

## Adding multiple agents

Register additional agents in the `Agents` slice. Each gets its own
engine, config, plugins, settings, and sessions:

```go
Agents: []desktop.Agent{
    {
        ID:         "agent-a",
        Name:       "Agent A",
        ConfigYAML: configA,
        Factories:  factoriesA,
        Settings:   settingsA,
    },
    {
        ID:         "agent-b",
        Name:       "Agent B",
        ConfigYAML: configB,
        Factories:  factoriesB,
        Settings:   settingsB,
    },
},
```

The frontend receives scoped events per agent (`"agent-a:nexus"`,
`"agent-b:nexus"`). Create a bus helper per agent and switch the
active one when the user navigates.

## Using LLM providers

If your agent needs an LLM provider (like Anthropic), register it in
the factories and reference the model in your config:

```go
import "github.com/frankbardon/nexus/plugins/providers/anthropic"

Factories: map[string]func() engine.Plugin{
    "nexus.io.wails":      wailsio.New,
    "nexus.llm.anthropic": anthropic.New,
    "myapp.agent.worker":  myplugin.New,
},
```

```yaml
core:
  models:
    default: quick
    quick:
      provider: nexus.llm.anthropic
      model: claude-sonnet-4-6
      max_tokens: 4096

plugins:
  active:
    - nexus.llm.anthropic
    - nexus.io.wails
    - myapp.agent.worker

  nexus.llm.anthropic:
    api_key: "${shell.api_key}"
```

Your plugin requests LLM completions via the bus using the model
registry — see the [Model Registry](../architecture/models.md)
documentation.

## Building and running

```bash
# Development mode (live reload).
cd cmd/my-app
wails dev

# Production build.
wails build
```

The `wails dev` command watches for Go and frontend changes and
rebuilds automatically. Production builds produce a single binary
with embedded frontend assets.

## Common patterns

### Session metadata from plugins

Emit `session.meta.title` after completing meaningful work so the
session list shows useful titles:

```go
p.bus.Emit("session.meta.title", map[string]any{
    "title": "Analysis: Q4 Revenue Report",
})
```

For richer session list entries, emit `session.meta.preview` with
agent-specific summary data:

```go
p.bus.Emit("session.meta.preview", map[string]any{
    "itemCount": 5,
    "topResult": "Jane Smith",
    "score":     0.95,
})
```

### Output files

When your plugin writes a file, emit `session.file.created` so the
shell and frontend can track it:

```go
p.bus.Emit("session.file.created", map[string]any{
    "path":     outputPath,
    "filename": filepath.Base(outputPath),
    "type":     "application/json",
})
```

### Error handling in boot

If `EnsureAgentRunning` returns an error, the agent status is set to
`"error"`. Common causes:

- Missing required settings — the framework lists which keys are
  missing. Redirect to the settings page.
- Invalid config YAML — check `${var}` placeholder names match the
  `SettingsField.Key` values.
- Plugin init failure — check the plugin's `Init` method for errors.

## Next steps

- [Desktop Shell Overview](./overview.md) — Architecture and component
  details
- [API Reference](./reference.md) — All shell methods, types, and
  events
- [Wails IO Plugin](../plugins/io/wails.md) — Plugin-level
  configuration and manual embedding
