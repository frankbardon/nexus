# Desktop Shell Framework (`pkg/desktop/`)

Desktop shell framework provides everything to embed one or more Nexus agents in Wails desktop app. Framework parts:

- **`shell.go`** — Core orchestrator. Manages per-agent engine lifecycles, Wails app setup, session mgmt, file portal, all Wails-bound methods.
- **`settings.go`** — Settings schema types (`SettingsField`, `FieldType`, `SettingsSchema`).
- **`store.go`** — Persistent settings store. Plaintext JSON at `~/.nexus/desktop/settings.json`, secrets in OS keychain via `go-keyring`.
- **`resolve.go`** — `${var}` placeholder resolution in config YAML from settings store with scope fallback (agent → shell).
- **`sessions.go`** — Session metadata index (`SessionMeta`), persists to `~/.nexus/desktop/sessions.json`, cleanup + reconciliation.
- **`runtime.go`** — Scoped `Runtime` adapter for multi-agent event isolation. Enriches file dialog `DefaultDirectory` from settings.
- **`watcher.go`** — Filesystem watcher (`fsnotify`) for file browser panel. Watches one dir at a time with debounced change notifications.

## Usage

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
        Settings: []desktop.SettingsField{
            {Key: "shell.api_key", Display: "API Key", Type: desktop.FieldString, Secret: true, Required: true},
            {Key: "input_dir", Display: "Input Folder", Type: desktop.FieldPath, Required: true},
            {Key: "output_dir", Display: "Output Folder", Type: desktop.FieldPath},
        },
    }},
})
```

## Wails-bound shell methods

Only Wails-bound methods — all domain comms flow through bus.

**Agent lifecycle:**
- `ListAgents()` — Returns all agents with current status (idle/booting/running/error)
- `EnsureAgentRunning(agentID)` — Lazy boot on first selection
- `StopAgent(agentID)` — Stop engine, mark idle

**Session management:**
- `NewSession(agentID)` — Stop current engine, boot fresh
- `RecallSession(agentID, sessionID)` — Stop current, boot with `RecallSessionID` to replay history
- `ListSessions(agentID)` — Sorted session metadata for agent
- `DeleteSession(agentID, sessionID)` — Remove from index + delete engine session dir

**OS integration:**
- `PickFile(agentID, title, filter)` — Native file open dialog, rooted in agent's `input_dir`
- `PickFolder(agentID, title)` — Native folder selection
- `OpenExternal(url)` — Open URL in system browser
- `RevealInFinder(path)` — Open file manager at path
- `Notify(title, body)` — OS notification (placeholder)

**File portal:**
- `ListFiles(agentID, filter)` — List contents of agent's `input_dir` (non-recursive, glob filter)
- `OutputDir(agentID)` — Resolve agent's `output_dir`, creating if needed
- `CopyFileToInputDir(agentID, sourcePath)` — Copy file into agent's `input_dir` (for drag-and-drop)
- `WatchInputDir(agentID)` — Start fsnotify watcher on agent's `input_dir`

**Settings:**
- `GetSettingsSchema()` — Full schema for frontend rendering (shell + per-agent)
- `GetSettings()` — All current values (secrets masked as `"__keychain__"`)
- `UpdateSetting(scope, key, value)` — Write plaintext setting
- `UpdateSecret(scope, key, value)` — Write to OS keychain
- `DeleteSetting(scope, key, secret)` — Remove setting or secret
- `HasMissingRequired()` — Map of agentID → missing required keys

## Settings system

- **Agent-contributed schemas**: Each `Agent` declares `Settings []SettingsField`. Shell renders settings UI dynamically from these schemas with type-appropriate controls (text input, file picker, toggle, etc.).
- **Persistence**: Plaintext settings in `~/.nexus/desktop/settings.json`, secrets in OS keychain via `zalando/go-keyring` (service name `nexus-desktop`).
- **Config injection**: Agent `ConfigYAML` uses `${var}` placeholders. Shell resolves from settings store before calling `engine.NewFromBytes`. Scope fallback: agent scope first, then shell.
- **Shell-scoped secrets**: Keys prefixed `shell.` (e.g. `shell.anthropic_api_key`) shared across agents — entered once in General settings.
- **Required field gating**: Agents with missing required settings cannot boot; shell redirects to settings page with missing fields highlighted.

## Session management

Shell tracks session history per agent in `~/.nexus/desktop/sessions.json`.

**Session metadata events** (bus contract — agent plugins emit, shell subscribes):
- `session.meta.title` — Human-readable session title (e.g. "Hello, World!")
- `session.meta.preview` — Agent-specific summary data for session list (e.g. `{ candidateCount: 5, topCandidate: "Jane" }`)
- `session.meta.status` — Explicit status change (e.g. `{ status: "completed" }`)

**Session lifecycle:**
- **New session**: `Shell.NewSession(agentID)` stops current engine + boots fresh one.
- **Recall**: `Shell.RecallSession(agentID, sessionID)` stops current engine, boots with `RecallSessionID` set. Engine replays conversation history via `io.history.replay`.
- **Delete**: `Shell.DeleteSession(agentID, sessionID)` removes both index entry + engine session dir from disk.
- **Cleanup**: On startup, shell removes sessions older than configured retention period (default 30 days) + reconciles orphaned engine dirs.

**Frontend notifications**: Session list updates arrive via `{agentID}:sessions.updated` Wails events emitted direct by shell (not through wails IO plugin bridge).

## UI state persistence

Framework-agnostic mechanism for frontends to persist + restore UI state across sessions:

- **`ui.state.save`** (inbound: frontend → bus) — Frontend emits with opaque `{ state: { ... } }` payload after meaningful actions. Shell writes to `ui-state.json` in engine session dir (`~/.nexus/sessions/<id>/ui-state.json`).
- **`ui.state.restore`** (outbound: bus → frontend) — On session recall, shell reads `ui-state.json` and emits so frontend can rehydrate.

Both events must be in wails IO plugin's `accept`/`subscribe` config. Payload structure entirely up to frontend — shell treats as opaque JSON blob. Works with any frontend framework.

## File portal

Shell provides standardized file access layer for agents. Each agent declares `input_dir` and optionally `output_dir` settings. Shell uses these to:

- **Root file dialogs**: `PickFile` + `scopedRuntime.OpenFileDialog` enrich `DefaultDirectory` from agent's `input_dir`, falling back to `shell.shared_data_dir`, then `~/Documents`.
- **List files**: `ListFiles(agentID, filter)` returns agent's `input_dir` contents for file browser panel.
- **Resolve output directory**: `OutputDir(agentID)` returns (and creates) agent's `output_dir`. Also via bus events: `io.file.output_dir.request` → `io.file.output_dir.response`.
- **File browser panel**: Right-side collapsible panel showing active agent's dir contents. Supports click-to-select (emits `io.file.selected`), open in default app, reveal in finder, drag-and-drop import.
- **Filesystem watching**: `fsnotify` watcher notifies frontend via `{agentID}:files.changed` Wails events when files added/removed in watched dir. Debounced to 200ms.
- **Drag-and-drop**: File panel accepts OS file drops. Uses HTML5 drag-and-drop with Wails webview file path access (optimistic), with FileReader fallback path.
- **Shared data directory**: Shell-scoped `shared_data_dir` setting accessible to all agents as fallback when `input_dir` not configured.

**Bus events for file portal:**

| Event | Direction | Purpose |
|-------|-----------|---------|
| `io.file.open.request` | Plugin → shell | Request a file dialog (existing) |
| `io.file.open.response` | Shell → plugin | File dialog result (existing) |
| `io.file.output_dir.request` | Plugin → shell | Ask where to write outputs |
| `io.file.output_dir.response` | Shell → plugin | Output directory path |
| `io.file.selected` | Shell → plugin | User selected a file in the browser panel |
| `session.file.created` | Plugin → shell | Agent wrote an output file (existing) |

## Desktop App (`cmd/desktop/`)

Reference multi-agent desktop app with two agents:

1. **staffing-match** — AI candidate ranking. Uses Anthropic LLM provider to score candidates against job descriptions. Domain plugin at `cmd/desktop/internal/matcher/`. Emits `session.meta.title`, `session.meta.preview`, `session.meta.status` after completing match. Writes match result JSON to `output_dir`. Saves UI state (job text, candidates, cost) for session recall. File dialogs rooted in `input_dir`.
2. **hello-world** — Minimal bus bridge PoC. Plugin at `plugins/apps/helloworld/`. Responds to `hello.request` with configurable greeting, emits `session.meta.title`. Saves UI state (name, response) for session recall.

**Frontend**: Single `index.html` AlpineJS SPA with:
- Left-nav sidebar with agent icons, active indicator, collapsible to icon-only
- Session list panel (per-agent, left side) with new/recall/delete actions
- File browser panel (right side, collapsible) with file list, click-to-select, open/reveal actions, drag-and-drop import, fsnotify-driven auto-refresh
- Settings page with dynamic schema rendering, keychain secret support
- Per-agent UI sections (hello-world form, staffing-match file picker + results table)
- Shared `createBus(agentID)` helper for scoped event bridging

**Agent configs** = `//go:embed`'d YAML files (`config-hello.yaml`, `config-staffing.yaml`) loaded via `engine.NewFromBytes`. No filesystem dep at runtime.
