# Sessions

Every Nexus run creates a session — a persistent workspace on disk that captures conversation history, thinking steps, plans, and plugin data.

## Directory Structure

Sessions are stored under the configured root directory (default: `~/.nexus/sessions/`):

```
~/.nexus/sessions/<session-id>/
├── context/
│   └── conversation.jsonl    # Conversation history (from memory plugin)
├── files/                    # Files created during the session
├── journal/
│   ├── active.jsonl          # Live event journal (every bus event,
│   │                         #   including thinking.step + plan.progress)
│   └── *.jsonl.zst           # Rotated, zstd-compressed segments
├── metadata/
│   ├── session.json          # Session metadata
│   └── config-snapshot.yaml  # Config used for this session
└── plugins/
    └── <plugin-id>/          # Per-plugin data directories
```

Thinking steps and plan progress are no longer kept in dedicated
`thinking.jsonl` / `plans.jsonl` files — they live in the journal
alongside every other event. Read them via
`journal.Writer.SubscribeProjection` (live) or `journal.ProjectFile`
(post-mortem).

## Session Metadata

Each session tracks metadata in `metadata/session.json`:

```go
type SessionMeta struct {
    ID                   string            // Random hex identifier
    StartedAt            time.Time         // When the session began
    EndedAt              *time.Time        // When the session ended (nil if active)
    Profile              string            // Config profile name
    Plugins              []string          // Active plugin IDs
    Labels               map[string]string // User-defined labels
    TurnCount            int               // Number of conversation turns
    TokensUsed           int               // Total tokens consumed
    PromptTokensUsed     int               // Input tokens consumed
    CompletionTokensUsed int               // Output tokens consumed
    CostUSD              float64           // Accumulated cost in USD
    Status               string            // "active" or "ended"
}
```

## Session Workspace API

Plugins interact with the session through the `SessionWorkspace` struct:

```go
// Write a file to the session workspace
session.WriteFile("context/mydata.json", data)

// Read a file back
data, err := session.ReadFile("context/mydata.json")

// Append to a file (useful for JSONL logs)
session.AppendFile("context/events.jsonl", line)

// List files in a subdirectory
files, err := session.ListFiles("context")

// Check if a file exists
exists := session.FileExists("context/conversation.jsonl")
```

### Directory Helpers

```go
session.ContextDir()          // ~/.nexus/sessions/<id>/context/
session.FilesDir()            // ~/.nexus/sessions/<id>/files/
session.MetadataDir()         // ~/.nexus/sessions/<id>/metadata/
session.PluginDir("nexus.tool.shell")  // ~/.nexus/sessions/<id>/plugins/nexus.tool.shell/
```

`PluginDir()` creates the directory lazily on first access.

## File Events

When files are written to the session, events are emitted automatically:

| Event | When |
|-------|------|
| `session.file.created` | A new file is written |
| `session.file.updated` | An existing file is overwritten |

These events carry the file path, session ID, and file size. The TUI plugin subscribes to these to show file creation notifications.

## Session Lifecycle

### Creating a Session

When `Engine.Run()` starts, it calls `NewSessionWorkspace()` which:

1. Generates a random hex session ID
2. Creates the directory structure (`context/`, `files/`, `metadata/`, `plugins/`)
3. Writes initial metadata with status `"active"`

### Resuming a Session

When launched with `-recall <sessionID>`:

1. The engine loads the session's config snapshot from `metadata/config-snapshot.yaml`
2. `LoadSessionWorkspace()` opens the existing directory
3. The session metadata is updated back to `"active"`
4. Plugins find their persisted data in their `PluginDir()`

### Ending a Session

On shutdown, the engine:

1. Sets `EndedAt` on the session metadata
2. Updates status to `"ended"`
3. Saves a config snapshot for future recall

## Configuration

Session behavior is configured in the `core.sessions` section:

```yaml
core:
  sessions:
    root: ~/.nexus/sessions   # Where sessions are stored
    retention: 30d            # How long to keep old sessions
    id_format: datetime_short # ID generation format
```

| Field | Default | Description |
|-------|---------|-------------|
| `root` | `~/.nexus/sessions` | Base directory for all sessions |
| `retention` | `30d` | Retention period for old sessions |
| `id_format` | `timestamp` | Format for generating session IDs |
