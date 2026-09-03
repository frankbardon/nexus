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

The journal records every bus event **except** the types listed in
`journal.exclude_events` (default `["core.tick"]`). Excluded events
still dispatch to bus subscribers — only the durable log skips them,
and their seq is not consumed, so on-disk envelopes stay gap-free.
The default suppresses the engine heartbeat, which replay regenerates
from the live tick goroutine and which otel / eval already treat as
noise. See [configuration reference](../configuration/reference.md#journal)
for the full key.

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

The snapshot is the original config YAML bytes verbatim, not a re-serialization
of the typed `Config` struct. `core.models` and per-plugin configs are parsed
via a second-pass raw map (`yaml:"-"` on the typed fields), so re-marshaling
would silently drop them and break recall. Configs constructed in-memory via
`DefaultConfig()` (no source bytes) fall back to `yaml.Marshal` of the typed
struct.

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

## Object-Store Backing (optional)

By default a session lives only on local disk. `core.sessions.object_store`
optionally makes a remote object store the source of truth for a session
*between* runs, so a session can be killed on one host and resumed on another
with no shared filesystem — the case for containers, Cloud Run and Lambda,
where there is no disk between invocations.

Local disk remains the working copy *during* a run. The seam
(`pkg/engine/objectstore.Backend`) is deliberately a **lifecycle** interface —
`Hydrate`, `Put`, `Delete`, `List`, `Flush` over object keys — not an
abstraction over `os.*`. Core and every plugin keep reading and writing
ordinary local files, so "behaves exactly like local disk" is a guarantee
rather than an aspiration, and SQLite keeps running against a real file.

Backends are selected by name in the `database/sql` driver style. Each ships as
its own Go module so the main module's dependency list never grows; an embedder
blank-imports the module and names it in config. The interface refers to no
cloud-specific concept, so a third party can implement it out of tree.

With no backend named — the default — no object-store code runs at all.

### Lifecycle points

The engine touches the seam in exactly three places, all in `pkg/engine`:

| When | What happens |
|------|--------------|
| Top of `Boot` | The configured backend is resolved once. A failure here fails the boot. |
| Before a resumed workspace is opened | The whole tree is hydrated from the store under the object key prefix `sessions/<session id>`. |
| End of `Stop` | `Flush` runs, then the backend is released. |

Hydration is **eager and whole-tree**, and completes before the first turn
runs. There is deliberately no lazy or faulting read: threading one through the
engine and ~60 plugins would be impossible to get right, and SQLite could not
use it at all — so "behaves exactly like local disk" would degrade from a
guarantee to an aspiration.

Hydration lands in a staging directory inside the sessions root and is
published with an atomic rename, so a hydration that dies partway leaves
nothing at `<root>/<session id>` and the partial tree is discarded. That failure
fails the boot under **both** failure policies: `degrade` means "keep running
against the local copy", and at hydrate time there is no local copy — degrading
would hand the agent an empty session that looks complete.

Resuming a session ID the store has never seen is **not** an error. It yields a
valid empty session, created through the same code path as a brand-new local
one, so the two are indistinguishable.

`<sessions.root>/<id>/session.lock` (written on `Boot`, see
[Human-in-the-Loop operations](../operations/hitl.md)) never crosses the seam in
either direction. It records the PID of the process holding the session
on one particular machine; a lock that travelled with the session would make
every rehydrated session look permanently locked by a process that no longer
exists. The exclusion is enforced at the seam itself, in
`pkg/engine/session_objectstore.go`, so every present and future push path
shares one definition of "never syncs".

Plugins are unaware of any of this. Nothing is exposed on `PluginContext`, and
no plugin calls the seam.

### Lifetime of the local working copy

The local tree under `core.sessions.root` is **not** wiped on clean exit.

- On the target deployment — a container, Cloud Run, Lambda — the filesystem
  vanishes when the process does, so wiping buys nothing beyond a slower
  shutdown and a window where a crash mid-wipe leaves a half-deleted tree.
- On a durable host the local tree is a warm cache: the next resume of the same
  session skips hydration entirely, and, more importantly, it is the copy that
  `failure_policy: degrade` falls back to. Deleting it would mean a store
  outage at shutdown destroys the only good copy.
- `core.sessions.retention` is already the operator-owned answer to "when does
  local session data go away". A second, implicit, shutdown-triggered answer
  would be a surprise, and deleting user data is irreversible.

See [Configuration Reference](../configuration/reference.md#coresessionsobject_store)
for the keys, their defaults and their validation behaviour.
