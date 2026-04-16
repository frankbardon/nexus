# Desktop Shell API Reference

Complete reference for the `pkg/desktop/` framework types, shell
methods, settings system, and event contracts.

## Types

### Shell

Top-level orchestrator. Pass to `desktop.Run()` to start the app.

```go
type Shell struct {
    Title   string      // Window title
    Width   int         // Window width (default 900)
    Height  int         // Window height (default 720)
    Agents  []Agent     // Registered agents
    Assets  embed.FS    // Frontend assets; zero value uses built-in base template
}
```

### Agent

Registration data for a single agent hosted by the shell.

```go
type Agent struct {
    ID          string                            // Unique key: "my-agent"
    Name        string                            // Display name: "My Agent"
    Description string                            // Short blurb for agent selector
    Icon        string                            // Font Awesome class: "fa-solid fa-gear"
    ConfigYAML  []byte                            // Embedded Nexus config YAML
    Factories   map[string]func() engine.Plugin   // Custom plugin factories
    Settings    []SettingsField                   // User-configurable fields
}
```

### AgentInfo

JSON-serializable projection of `Agent` returned by `ListAgents()`.

```go
type AgentInfo struct {
    ID          string `json:"id"`
    Name        string `json:"name"`
    Description string `json:"description"`
    Icon        string `json:"icon"`
    Status      string `json:"status"` // "idle", "booting", "running", "error"
}
```

### AgentStatus

```go
const (
    AgentStatusIdle    AgentStatus = "idle"
    AgentStatusBooting AgentStatus = "booting"
    AgentStatusRunning AgentStatus = "running"
    AgentStatusError   AgentStatus = "error"
)
```

### SessionMeta

Shell-level metadata for a single engine session.

```go
type SessionMeta struct {
    ID        string    `json:"id"`
    AgentID   string    `json:"agent_id"`
    Title     string    `json:"title"`
    Status    string    `json:"status"`     // "running", "completed", "failed"
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
    Preview   any       `json:"preview,omitempty"` // Agent-specific summary data
}
```

### FileInfo

File entry returned by `ListFiles()`.

```go
type FileInfo struct {
    Name     string    `json:"name"`
    Path     string    `json:"path"`
    Size     int64     `json:"size"`
    Modified time.Time `json:"modified"`
    IsDir    bool      `json:"is_dir"`
}
```

## Settings types

### SettingsField

Declares a single configurable value for the settings UI.

```go
type SettingsField struct {
    Key         string           // Machine key: "data_dir"
    Display     string           // Human label: "Data Folder"
    Description string           // Help text shown below the field
    Type        FieldType        // UI control type
    Secret      bool             // Stored in OS keychain, masked in UI
    Default     any              // Default value if unconfigured
    Required    bool             // Agent refuses to boot without this
    Validation  *FieldValidation // Optional constraints
    ConfigPath  string           // Template variable in config YAML
    Options     []SelectOption   // For FieldSelect only
}
```

### FieldType

```go
const (
    FieldString FieldType = iota // Single-line text input
    FieldPath                    // Text input + Browse button
    FieldText                    // Multiline textarea
    FieldNumber                  // Number input (optional min/max)
    FieldBool                    // Toggle switch
    FieldSelect                  // Dropdown
)
```

### FieldValidation

```go
type FieldValidation struct {
    Regex   string   `json:"regex,omitempty"`
    Min     *float64 `json:"min,omitempty"`
    Max     *float64 `json:"max,omitempty"`
    Message string   `json:"message,omitempty"` // Validation error message
}
```

### SelectOption

```go
type SelectOption struct {
    Value   string `json:"value"`
    Display string `json:"display"`
}
```

### SettingsSchema

Top-level schema sent to the frontend for dynamic UI rendering.

```go
type SettingsSchema struct {
    Shell  []SettingsFieldInfo            `json:"shell"`
    Agents map[string][]SettingsFieldInfo `json:"agents"`
}
```

### SettingsFieldInfo

JSON projection of `SettingsField` sent to the frontend.

```go
type SettingsFieldInfo struct {
    Key         string           `json:"key"`
    Display     string           `json:"display"`
    Description string           `json:"description,omitempty"`
    Type        string           `json:"type"`      // "string", "path", "text", "number", "bool", "select"
    Secret      bool             `json:"secret,omitempty"`
    Required    bool             `json:"required,omitempty"`
    Default     any              `json:"default,omitempty"`
    Validation  *FieldValidation `json:"validation,omitempty"`
    Options     []SelectOption   `json:"options,omitempty"`
}
```

## Shell methods (Wails-bound)

All methods below are exposed to the frontend via Wails bindings.
Call them from JavaScript as `go.desktop.Shell.MethodName(args)`.

### Agent lifecycle

| Method | Signature | Description |
|--------|-----------|-------------|
| `ListAgents` | `() []AgentInfo` | All registered agents with current status |
| `EnsureAgentRunning` | `(agentID string) error` | Lazy boot — creates and boots engine on first call, no-op if already running |
| `StopAgent` | `(agentID string) error` | Stop engine, tear down bus subs, mark idle |

### Session management

| Method | Signature | Description |
|--------|-----------|-------------|
| `NewSession` | `(agentID string) error` | Stop current engine, boot fresh with new session |
| `RecallSession` | `(agentID, sessionID string) error` | Stop current, boot with `RecallSessionID` for history replay |
| `ListSessions` | `(agentID string) []SessionMeta` | Session metadata for agent, sorted most-recent first |
| `DeleteSession` | `(agentID, sessionID string) error` | Remove from index + delete engine session dir. Cannot delete active session |

### OS integration

| Method | Signature | Description |
|--------|-----------|-------------|
| `PickFile` | `(agentID, title, filter string) (string, error)` | Native file open dialog, rooted in agent's `input_dir` |
| `PickFolder` | `(agentID, title string) (string, error)` | Native folder selection dialog |
| `OpenExternal` | `(target string) error` | Open URL in system browser or file in default app |
| `RevealInFinder` | `(path string) error` | Open file manager at path (Finder/Explorer/xdg-open) |
| `Notify` | `(title, body string) error` | OS notification (placeholder — logs for now) |

### File portal

| Method | Signature | Description |
|--------|-----------|-------------|
| `ListFiles` | `(agentID, filter string) ([]FileInfo, error)` | Non-recursive listing of agent's `input_dir`, glob filter (e.g. `"*.pdf"`) |
| `OutputDir` | `(agentID string) (string, error)` | Resolve agent's `output_dir`, create if needed |
| `CopyFileToInputDir` | `(agentID, sourcePath string) (string, error)` | Copy file into `input_dir` (drag-and-drop). Returns dest path |
| `WriteFileToInputDir` | `(agentID, name, base64Data string) (string, error)` | Write base64-encoded file to `input_dir` (webview fallback for drag-and-drop) |
| `WatchInputDir` | `(agentID string)` | Start fsnotify watcher on `input_dir`. Emits `{agentID}:files.changed` on changes |

### Settings

| Method | Signature | Description |
|--------|-----------|-------------|
| `GetSettingsSchema` | `() SettingsSchema` | Full schema for frontend rendering (shell + per-agent) |
| `GetSettings` | `() map[string]map[string]any` | All current values. Secrets show `"__keychain__"` |
| `UpdateSetting` | `(scope, key string, value any) error` | Write plaintext setting. Scope = agent ID or `"shell"` |
| `UpdateSecret` | `(scope, key, value string) error` | Write secret to OS keychain |
| `DeleteSetting` | `(scope, key string, secret bool) error` | Remove plaintext setting or secret |
| `HasMissingRequired` | `() map[string][]string` | Map of agentID to missing required setting keys |

## Event contracts

### Bus events (plugin to shell)

Events emitted by agent plugins that the shell subscribes to
internally. These do not need to be in the wails IO plugin's
`subscribe`/`accept` config — the shell installs its own bus
subscriptions.

| Event | Payload | Purpose |
|-------|---------|---------|
| `session.meta.title` | `{ "title": string }` | Set human-readable session title |
| `session.meta.preview` | `{ ... }` (opaque) | Agent-specific summary for session list |
| `session.meta.status` | `{ "status": string }` | Explicit status change (`"completed"`, `"failed"`, etc.) |
| `io.session.end` | — | Signals session end, marks session as completed |
| `io.file.output_dir.request` | `{ "requestID": string }` | Plugin asks shell for the output directory path |
| `session.file.created` | `{ "path": string, "filename": string, ... }` | Plugin notifies that an output file was written |

### Bus events (shell to plugin)

| Event | Payload | Purpose |
|-------|---------|---------|
| `io.file.output_dir.response` | `{ "requestID": string, "path": string, "error": string }` | Output directory path response |

### Bus events (frontend bridge)

These events cross the bus-to-frontend boundary and **must** be listed
in the wails IO plugin's `subscribe`/`accept` config.

| Event | Direction | Config key | Purpose |
|-------|-----------|------------|---------|
| `ui.state.save` | Frontend to bus | `accept` | Frontend persists UI state |
| `ui.state.restore` | Bus to frontend | `subscribe` | Shell restores UI state on recall |
| Domain events | Either | `subscribe`/`accept` | Agent-specific events (e.g. `work.request`, `work.result`) |

### Wails events (shell direct)

Events emitted by the shell directly via `wailsruntime.EventsEmit`,
bypassing the bus bridge. Listen with `window.runtime.EventsOn`.

| Event | Payload | Purpose |
|-------|---------|---------|
| `{agentID}:sessions.updated` | JSON string of `[]SessionMeta` | Session list changed for agent |
| `{agentID}:files.changed` | — | Files added/removed in watched `input_dir` |

## Settings store

### Persistence

- **Plaintext**: `~/.nexus/desktop/settings.json` — JSON file with
  `{ version, shell: {}, agents: {} }` structure.
- **Secrets**: OS keychain via `go-keyring`, service name
  `"nexus-desktop"`, account `"{scope}.{key}"`.
- **Sentinel**: Secret fields store `"__keychain__"` in the JSON file
  so the frontend knows a value exists without exposing it.

### Scope resolution

When resolving a `${var}` placeholder in config YAML:

1. Check agent scope for the key
2. Fall back to shell scope
3. Fall back to `SettingsField.Default`
4. If still unresolved and `Required: true`, boot fails

Keys prefixed with `shell.` (e.g. `shell.api_key`) are always looked
up in shell scope directly.

### Built-in shell settings

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `session_root` | `path` | `~/.nexus/sessions` | Session storage directory |
| `session_retention_days` | `number` | `30` | Days to keep sessions before cleanup (1–365) |
| `shared_data_dir` | `path` | — | Shared directory accessible to all agents |

## Session index

Persisted at `~/.nexus/desktop/sessions.json`.

### Maintenance (runs on startup)

- **Cleanup**: Removes sessions older than `session_retention_days`
  from both the index and disk.
- **Reconcile**: Adopts orphaned engine directories (on disk but not
  in index) if newer than the retention cutoff. Removes stale index
  entries whose directories no longer exist on disk.

## File watcher

Single-directory watcher using `fsnotify`. Debounced at 200ms.

- Only fires on `Create`, `Remove`, and `Rename` operations.
- Calling `Watch(newDir)` automatically unwatches the previous
  directory.
- Calling `Watch("")` stops watching without starting a new watch.
- Notifications arrive as `{agentID}:files.changed` Wails events.

## Config resolution

`resolveConfig()` performs `${key}` placeholder substitution on raw
YAML bytes before engine creation. For each `SettingsField`:

1. If `${field.Key}` appears in the YAML, resolve it
2. Shell-prefixed keys (`shell.xxx`) look up in shell scope directly
3. Other keys check agent scope, then fall back to shell scope
4. Unresolved required fields are collected and returned as an error
5. Unresolved optional fields are left as literal `${key}` — the
   plugin sees the placeholder string and can handle or ignore it
