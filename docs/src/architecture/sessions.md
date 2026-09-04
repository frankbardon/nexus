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

Every `SessionWorkspace` write helper announces itself on the bus, so a subscriber
does not have to watch the filesystem to know what a session changed:

| Event | When |
|-------|------|
| `session.file.created` | A path under the session root appeared |
| `session.file.updated` | A path that already existed changed |

| Helper | Emits |
|--------|-------|
| `WriteFile` | `created` on first write, `updated` on every rewrite |
| `AppendFile` | `created` on the append that creates the file, `updated` on every later append |
| `SaveMeta` | `updated` for `metadata/session.json`, which is rewritten on every `llm.response` and every `agent.turn.end` |

The payload carries `session_id`, the slash-separated session-relative `path`, `size`
(the size of the whole file after the write, not the size of the change), and an
append-aware delta: `offset` and `bytes_added`. The TUI, browser and Wails transports
subscribe to these to surface file activity; see
[Event Reference](../events/reference.md) for the exact payload.

`AppendFile` reuses the same two event types rather than a separate append event, so
existing subscribers see appends without changing.

### The append-aware delta

Appends are the highest-churn writes in a session — conversation history, turn timing,
compaction output and shell history all land through `AppendFile` — and a subscriber
told only "this path changed" has to re-read (and, for a sync backend, re-upload) the
whole file every time. `offset` and `bytes_added` say which part changed:

| Shape | Meaning |
|-------|---------|
| `offset == 0 && bytes_added == size` | The whole object is new. Every `WriteFile`, and the first append that creates a file. |
| `offset > 0 && offset + bytes_added == size` | A pure append. Every byte before `offset` is byte-identical to what the last event for this path described. |

A distinct `session.file.appended` event was considered and rejected. It would have
carried the same three numbers, cost every existing subscriber a change just to keep
seeing appends, and — because `context/conversation.jsonl` is written by both helpers —
forced each of them to merge two event streams to reconstruct one file's history. The
delta is more information about a change subscribers already receive, so it belongs on
the payload.

**Object stores have no append primitive.** Nothing here lets a backend write the
appended bytes into an existing object: S3, GCS and every S3-compatible store replace
whole objects. What the delta buys is the freedom to *coalesce and defer* — a backend
that knows the last two hundred events on `conversation.jsonl` only added bytes to the
tail knows it can collapse them into one upload of the current file at the next
boundary, and knows it has not missed a rewrite in between. Reading `offset` as "seek
here and write `bytes_added` bytes into the bucket" is a misreading.

`offset` is taken from the append descriptor's own position after the write, so it stays
exact even if another writer appends to the same path in between. If that read fails it
falls back to `0`, which reads as "the whole object changed" — the conservative
direction, since a backend then re-uploads a file it could have coalesced rather than
coalescing a change it should have treated as a rewrite.

Creating or loading a workspace writes `metadata/session.json` silently. That happens
before the journal writer subscribes to the bus, and an event there would consume a
dispatch sequence number the journal never receives — which stalls its writer, since it
only writes envelopes in contiguous sequence order. `StartSession` re-saves the
metadata once the journal is running, so the file is still announced.

### Writers that bypass the helpers

Not everything under a session tree goes through the workspace helpers. Some writers
hold a long-lived `*os.File`, some write through temp-file + rename for atomicity, and
some own a directory layout that is theirs rather than the workspace's. Two helpers
exist for them:

| Helper | Use |
|--------|-----|
| `AnnounceWrite(fullPath, existed)` | A whole-file write. `existed` must be sampled *before* the write and selects `created` vs `updated`. |
| `AnnounceAppend(fullPath, bytesAdded)` | Bytes appended to the tail. Takes no `existed` flag — it derives `created` from a post-append offset of 0, so a descriptor opened with `O_CREATE` at plugin `Init` still announces a creation for the first real bytes written through it. |

Both take an absolute path and emit exactly the payload `WriteFile` emits, with the
path relativised to the session root. Both are silent when the workspace has no bus,
when the path is not under the session root, or when the path is one the object-store
seam excludes outright — so it is impossible to announce `store.db` or `session.lock`
by mistake, and a plugin whose output directory is configurable can call them
unconditionally instead of repeating an escape check.

An fsnotify watcher over the session root was the rejected alternative. It buys
completeness with no call-site changes, and costs a watch descriptor per directory, a
rename storm to debounce on every atomic write, and — fatally — no way to tell "the
writer finished" from "the writer is halfway through", which is the one distinction a
sync backend needs.

### Every raw writer has a decided disposition

The set of writers that bypass the helpers is closed and enumerable: the plugin-level
ones all take their directory from `PluginDir()` or a config key, and the rest are
named engine subsystems. Each has one of four dispositions, recorded in code as
`engine.SessionTreeWriters()` so an enforcement test can consume it:

| Disposition | Meaning |
|-------------|---------|
| **emit** | Announces every write on the bus; a sync backend can push it as it lands. |
| **turn-boundary-only** | Silent on the bus by decision. The bytes still reach the store, through the whole-tree snapshot taken at `agent.turn.end` and at shutdown. |
| **write-through** | Pushed to the object store the moment the bytes land, with no bus event at all. Only safe where the object key is derived from the content, so a duplicate upload is a no-op rather than a race — exactly one subtree qualifies. |
| **excluded-by-design** | Never leaves the machine at all — not on the bus, not in the snapshot. |

| Writer | Writes | Disposition | Why |
|--------|--------|-------------|-----|
| `plugins/scene` | `plugins/nexus.scene/scenes.json` + `scenes.jsonl` patch journal | emit | The highest-churn raw writer under a session, and the journal is the durable source of truth the replay primitive reconstructs scene state from — a run killed mid-turn otherwise loses exactly the scenes it just built. |
| `plugins/workflows/icm/session` | `plugins/nexus.workflows.icm/<runID>/` stage artifacts and sidecars | emit | An ICM run is long enough that waiting for a turn boundary discards completed stages on a crash. Every write funnels through `WriteArtifact` plus the two input-copy loops. |
| `plugins/llm/batch` | one JSON state file per in-flight batch | turn-boundary-only | `batch.data_dir` defaults to `~/.nexus/batches`, outside every session tree. Its durability requirement is a local disk that survives a restart — the coordinator resumes batches by scanning the directory at boot — not a remote copy. |
| `plugins/memory/longterm` | one markdown file per memory key | turn-boundary-only | Defaults to `~/.nexus/memory`; cross-session by definition, so deliberately not under a session. |
| `plugins/rag/ingest` | embedding cache entries (`cache.go`) and generated chunk prefixes under `<cache_dir>/_prefix` (`contextual.go`) | turn-boundary-only | Defaults to `~/.nexus/vectors/_cache`. Both are caches of output derivable from the source documents — pushing them spends bandwidth on bytes a resume can regenerate, and losing them costs latency and tokens, not correctness. |
| `plugins/tools/codeexec` | skill helper `.go` sources into an `os.MkdirTemp` GOPATH | turn-boundary-only | Cannot be under a session tree. The helpers are staged into a fresh temp root purely so Yaegi's import resolver can find them, and the deferred cleanup deletes the whole root before the tool call returns — there is nothing durable to carry. |
| `plugins/io/oneshot` | the run's JSON transcript, to `output_file` | turn-boundary-only | `output_file` is unset by default, so normally no file exists. When set it is an operator-chosen destination for a shell pipeline, normally outside the session, and `finalize` runs once at the end of the last turn or at shutdown — an announcement would buy the tail of a run that is already over. |
| `plugins/control/hitl` | request/response files | turn-boundary-only | Defaults to `~/.nexus/hitl`, and is a filesystem IPC rendezvous rather than session state. Restoring an in-flight pair would re-ask a question that was already answered. |
| `plugins/observe/sampler` | sampled journal + `metadata.json` | turn-boundary-only | Defaults to `~/.nexus/eval/samples`: an eval corpus accumulated outside sessions so it survives their cleanup. Also a copy of journal bytes the snapshot already carries. |
| `pkg/engine/journal/writer.go`, `rotate.go` | `journal/events.jsonl`, `journal/events-NNN.jsonl.zst` | turn-boundary-only | Emitting here is a self-feeding loop, not a preference: the writer's input is every event on the bus. It holds no bus reference at all, which makes the loop impossible by construction. The snapshot captures the journal through `journal.Writer.Snapshot`. |
| `pkg/engine/toolcache.go` | `journal/cache/<tool>/<argshash>.json` | turn-boundary-only | A replay companion to `journal/events.jsonl` and useless without it, so streaming one while the other waits would push half an artefact pair. It also runs inside a `tool.result` handler, where an emission would roughly double bus traffic on the hottest path. |
| `pkg/engine/blobs` | `blobs/<xx>/<sha256>.bin` and `.meta` | write-through | Content-addressed, so the key is derived from the bytes and a duplicate upload is a no-op rather than a race — the one subtree that can be pushed the instant it lands with no barrier. Pushed by a plain func hook (`blobs.WithPutHook`), not by an event, which keeps the package free of a bus dependency and keeps blob traffic off the hottest tool paths in a session. Local LRU eviction is **not** mirrored remotely. See [Blobs push on write](#blobs-push-on-write). |
| `pkg/engine/storage/sqlite.go` | `plugins/<pluginID>/store.db` | **excluded-by-design** | WAL mode. Committed frames live in `store.db-wal` until a checkpoint, so streaming partial writes uploads a database that is corrupt, or plausible and silently stale. It reaches the store only as a checkpointed `VACUUM INTO` snapshot; the `-wal` / `-shm` / `-journal` sidecars never cross the seam. |
| `pkg/engine/session_lock.go` | `session.lock` | **excluded-by-design** | The file carries the local PID of the owning process and `Boot` refuses to start against a live one. Round-tripping it stamps one host's PID onto every later resume — correct by coincidence on a fresh container, and wrong the moment that number is in use. |

### The invariant, and the test that holds it

> **A write under a session tree must announce itself on the bus.** Real-time sync is
> exactly as complete as the events are, so a write nothing announced is history that
> quietly never arrives.

The one sanctioned alternative to announcing is pushing directly, and it is available
only where the object key is derived from the content: see
[Blobs push on write](#blobs-push-on-write). Anything else that is silent on the bus
waits for the turn boundary, which is a decision the table has to record rather than a
gap the table hides.

The table above closes today's gap. `TestPluginRawWritesAreAnnouncedOrAllowlisted`
(`pkg/engine/session_writers_enforce_test.go`) is what stops a future plugin reopening it.
It runs inside `make test` — untagged, no network — and does the following:

1. Parses every non-test Go file under `plugins/` with `go/parser` (standard library; this
   guard adds no dependency).
2. Flags direct calls to `os.WriteFile`, `os.Create`, `os.CreateTemp` and `os.OpenFile`,
   resolving the `os` import through its local name so an alias or a dot-import is not a
   way around it.
3. Requires each flagged file to either call `AnnounceWrite` / `AnnounceAppend`, or carry a
   row in `engine.SessionTreeWriters()`.

If you trip it, the failure message spells out the three ways forward: announce the write,
route it through `session.WriteFile` / `session.AppendFile`, or add a row with a `Why`. The
allowlist is `SessionTreeWriters()` itself rather than a list local to the test, so
silencing the guard and documenting the decision are the same edit — and a row whose file
stops writing raw bytes is reported as stale by
`TestSessionTreeWriters_PluginRowsStillWriteRawBytes`, so the list shrinks as well as grows.

What the guard deliberately cannot see, so that it is not trusted past its limits:

- Writes through an `*os.File` handed to another package, through `bufio` or `io.Copy` onto
  a descriptor opened elsewhere, through `text/template.Execute`, or through a third-party
  library.
- Announcement is matched per file, not per call: a file with one announced and one
  unannounced write reads as covered.
- Only `plugins/` is scanned. `pkg/engine`'s raw writers are a small closed set of named
  subsystems that already have rows; `cmd/` is not scanned at all.

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

By default a session lives only on local disk. `core.object_store`
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

With no backend named — the default — no object-store code runs at all: no
handle is opened, no snapshot handler is subscribed, and a backend sitting
registered in the process is never touched. Every shipped profile under
`configs/` leaves the seam inert, and
`TestDefaultPathNeverTouchesARegisteredBackend` in `pkg/engine` holds the
default path to it.

### Lifecycle points

The engine touches the seam in exactly six places, all in `pkg/engine`:

| When | What happens |
|------|--------------|
| Top of `Boot` | The configured backend is resolved once. A failure here fails the boot. |
| Top of `Boot`, before any plugin can open storage | App- and agent-scope plugin stores are hydrated. See [The other roots](#the-other-roots). |
| Before a resumed workspace is opened | The whole tree is hydrated from the store under the object key prefix `sessions/<session id>`, then pruned to exactly the object set the committed manifest names. |
| Once the workspace exists and the local lock is held | An owner marker is claimed at `sessions/<session id>.owner/owner.json`, and a second host holding the session is detected. See [Two hosts, one session](#two-hosts-one-session). |
| Every turn boundary | The whole tree is snapshotted and made durable, a per-object manifest and then a commit marker are published, then the shared plugin stores are snapshotted. |
| End of `Stop` | A final snapshot runs, the owner marker is removed, then `Flush`, then the backend is released. |
| `Abandon` | Every background worker stops and the backend handle is dropped. No snapshot, no `Flush`, and the owner marker is left where it is. See [Dropping a session without closing it](#dropping-a-session-without-closing-it). |

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

### Turn-boundary snapshots

**The hook is `agent.turn.end`, and it is handled in core.** That event is
already the engine's definition of a turn boundary — the journal fsyncs on it
and rotates on it, and the turn counter and `metadata/timing.jsonl` are driven
by it — so hanging the snapshot anywhere else would invent a second,
disagreeing notion of "turn". The subscription lives in
`pkg/engine/session_objectstore.go`: no plugin implements an interface, calls a
method, or learns that an object store exists. An agent loop emits the event it
already emitted.

Two more triggers exist. `session.snapshot.request` forces a snapshot for
callers that do not emit turn events — an embedder driving the engine directly,
or a custom agent loop — and `Stop` takes a final one, so a session that ends
between turns (or ran no turns at all) is still in the bucket. Every snapshot
publishes a `session.snapshot.result` carrying the object count, the byte total,
the duration and whether it succeeded.

The snapshot is **synchronous**: it blocks the goroutine that ended the turn
until the bytes are durable. A background snapshot would report a turn complete
while its state was still in flight, which is precisely the guarantee this
exists to provide.

It is installed as a **wildcard subscription filtered to `agent.turn.end`**,
not a typed one, and that is a correctness requirement rather than a style
choice. The bus runs every typed handler before any wildcard, and the journal is
itself a wildcard. A typed handler would therefore snapshot a journal ending one
envelope short of the very boundary it is reacting to — and a journal whose last
turn has no `agent.turn.end` is exactly what
`journal.Coordinator.IsPartialTurn` calls an unfinished turn, so every resume
from that snapshot would re-fire the last input and re-run a turn that had
already completed. Registering after the journal's wildcard also means the
snapshot sees the writes the boundary itself produces — memory compaction, the
turn counter, history pruning — rather than the state before them.

#### What must not be copied naively

Two things in the tree rewrite themselves while a reader is walking it, and both
are staged into a temporary directory *beside* the session rather than uploaded
in place.

**Per-plugin SQLite.** A live `store.db` is a WAL database: committed
transactions sit in `store.db-wal` until a checkpoint folds them back, and
`store.db-shm` is a process-local index into that WAL. Neither sidecar means
anything on another host, so the uploaded file has to stand alone. Every
snapshot therefore runs `PRAGMA wal_checkpoint(TRUNCATE)` and then
`VACUUM INTO` a staging path. The checkpoint is what makes the *live* file
self-contained (and bounds `-wal` growth over a long session); `VACUUM INTO` is
what makes the *snapshot* untearable, since a plugin committing between the
checkpoint and the last read byte would otherwise produce a corrupt — not merely
stale — file, and a corrupt file that uploads successfully is worse than a
failed upload. `store.db-wal`, `store.db-shm` and `store.db-journal` are
excluded from the seam entirely, in both directions.

**The journal.** Rotation compresses `events.jsonl` into the next
`events-NNN.jsonl.zst` and truncates the active segment, and it fires on the
drain goroutine the instant an `agent.turn.end` envelope lands — the same event
the snapshot reacts to. Read the active segment after the truncate but list the
directory before the new `.zst` appears and the turn's events are in neither
object; capture both and they are in the bucket twice. `journal.Writer.Snapshot`
takes the writer's file mutex — the one rotation holds — and captures a single
consistent instant: rotated segments and `header.json` are immutable once
written and are read in place, while the mutable active segment is copied under
the lock. A `Barrier` runs first so the capture includes the very turn that
triggered it rather than trailing it by whatever is still queued.
`journal/cache/` is ordinary data and is walked normally.

#### What is never re-uploaded

A snapshot is `O(whole tree)`, not `O(what changed)`, and that cost is paid on
every turn. Two kinds of file in a session tree **cannot** change once written,
and a snapshot that has already stored them does not store them again:

| Path | Why it cannot change |
|------|----------------------|
| `journal/events-NNN.jsonl.zst` | Sealed at rotation. Rotation compresses the active segment into the next free `NNN` slot and never reopens it. |
| `blobs/<xx>/<sha256>.bin` and `.meta` | Content-addressed. Different bytes would have a different sha256 and therefore a different name, so a file at that path either holds those bytes or does not exist. Usually already uploaded before the snapshot runs at all — see [Blobs push on write](#blobs-push-on-write). |

The skip is **by construction, never by diffing**. A file qualifies because its
*identity* proves immutability, not because a hash or an mtime comparison
suggested it was unchanged — `blobs.Store.Put` touches mtime on every hit, so
mtime would have been actively misleading here. General content-hash or
mtime-based change detection over the rest of the tree is deliberately not
implemented: ordinary session output is still re-uploaded in full every turn.

Being unchangeable locally says nothing about whether the object ever reached the
bucket, so immutability alone is only half the decision. Every snapshot **lists
the session's key prefix** and skips a file only when that listing says the store
holds it at exactly that size. A skipped file that is missing — or truncated — is
uploaded like anything else, so a skip can never turn a gap into a permanent gap,
and the skip repairs itself against anything that removes an object out of band.
A backend that cannot list makes the snapshot upload everything, which is always
the safe direction. The listing costs one or two round trips against as many
avoided uploads as the session has immutable files.

A skipped object is still part of the **committed object set**: `objects` and
`bytes` on the commit marker and on `session.snapshot.result` describe the whole
stored session, exactly as they did before the skip existed, and
`objects_uploaded` / `objects_skipped` split that set into what this turn paid
for and what it saved. The [per-object manifest](#the-generation-stamp-and-the-per-object-manifest)
is built from the committed set, **not** from "what was uploaded this turn" — a
manifest of only-what-was-uploaded would describe a session with its journal
segments and blobs missing, and a hydration honouring it would then faithfully
reproduce that truncated session.

#### Blobs push on write

`<session>/blobs/` does not wait for the turn boundary. A blob store opened
through `SessionWorkspace.BlobStore()` carries a `blobs.PutHook`, and each new
blob's `.bin` and `.meta` are handed to a single background worker that uploads
them and flushes once the queue goes quiet.

**What that buys, precisely.** Not bandwidth — `objectStoreImmutable` already
made each blob a once-ever upload, so the repeated per-turn cost was never
being paid. What is left is the window between a blob landing and the next
`agent.turn.end`. Blobs are the largest single objects in a session tree, so a
turn that fetches a PDF, renders a screenshot and embeds an image otherwise
holds all of it on local disk until the turn ends: a process killed halfway
loses every blob it produced, and what survives is a conversation history full
of `nexus-blob:` URIs that resolve to nothing after a resume on a fresh host.
Write-through shrinks that window from "one turn" to "one queue drain", and
spreads the upload across the turn instead of spiking at the end of it. That is
a narrow win, and it is the whole win.

**Why no barrier is needed.** The key is derived from the sha256 of the
content, so the same key can only ever carry the same bytes. A write-through
`Put` racing a snapshot `Put` of the same blob is two identical uploads, not a
conflict: no read-modify-write, no window in which a partial object sits under
a key another writer will fill with different content. `context/
conversation.jsonl` has none of those properties, which is why the general push
still waits for a boundary.

**It is an optimisation, not the guarantee.** Every failure mode on this path —
a full queue, a `Put` error, a drain that timed out at shutdown — costs the
delay it was trying to remove and nothing else. The turn-boundary snapshot
still walks the whole tree and still re-uploads any immutable file the store
does not already hold at exactly the right size, so correctness lives there.

**No event.** The push is a plain func hook rather than a `session.file.*`
emission. An event per blob would have put traffic on the hottest tool paths in
a session — every `read_image`, every `fetch_page_image`, every MCP binary
payload — to carry a fact the object key already encodes, would have made every
existing subscriber react to blob writes it has no use for, and would have cost
`pkg/engine/blobs` its deliberate independence from the bus (it is a standalone
content store usable outside an engine, and it still imports nothing outside
the standard library).

**Local eviction never deletes remotely.** The blob store sweeps by mtime under
an LRU byte budget, and that budget exists to bound *disk* — exactly the
constraint a bucket does not have. There is no delete hook, and nothing on this
path ever calls `Backend.Delete`. A swept blob stays in the store; a `Get` for
it afterwards is a local miss that hydration can repair, not a lost object.
Mirroring the eviction would destroy data the operator is paying to keep, and
would do it to content a later session may still reference by URI.

`blobs/` is still created lazily, on the first `Put` — not at session boot and
not by opening the store — so a session whose tools never produce a blob has no
`blobs/` directory to sync at all.

#### Failure, and the commit marker

Object stores have no multi-object transaction, so a tree spread over many
objects cannot be replaced atomically. Three properties give
"a failed or partial upload never replaces a good remote copy" anyway:

1. Nothing is uploaded from a file that could be torn — see above.
2. A snapshot **never deletes**. It adds and overwrites only, so a failure
   cannot remove remote state it did not successfully replace.
3. A **per-object manifest** at `sessions/<session id>.manifest/manifest.json`
   is written and flushed after every other object is durable, listing exactly
   the object set that generation asserts is present.
4. A **commit marker** at `sessions/<session id>.snapshot.json` is written and
   flushed after the manifest. It therefore only ever advances past a complete
   snapshot: a failed or half-finished upload leaves it naming the previous one,
   which is the snapshot guaranteed to be restorable.

Both are *sibling* keys, not members of the tree. Because prefixes match whole
segments, `sessions/<id>.snapshot.json` and `sessions/<id>.manifest/` are
deliberately not under prefix `sessions/<id>`, so neither hydrates back into the
session and neither becomes an input to the next snapshot.

Generation directories — write the whole tree under `sessions/<id>/gen-<n>/` and
flip a pointer — were considered and rejected. They give true atomic replace at
the cost of a second full copy of every session in the bucket and an
indirection hydration would have to resolve on every boot. The marker answers
the same question for one small object.

#### The generation stamp and the per-object manifest

A snapshot never deletes, so an **interrupted** snapshot leaves a superset: some
objects from generation *N+1* sitting beside the rest of generation *N*. Without
something to say which is which, that tree hydrates silently and produces a
session whose artifacts disagree with its own history.

Two records answer it, both siblings of the tree:

| Key | Contents |
|-----|----------|
| `sessions/<id>.snapshot.json` | The commit marker: session ID, `generation`, `manifest_key`, per-run `sequence`, trigger, turn ID, completion time, object and byte counts. Deliberately small — an operator reads it by hand. |
| `sessions/<id>.manifest/manifest.json` | The per-object manifest: `generation` plus a sorted array of the session-relative paths that generation asserts are present. Paths only — no sizes, no digests, no mtimes. It is a *set*, not an index. |

`generation` increases by one per completed snapshot and, unlike `sequence`,
carries **across runs**: a resuming host seeds it from the committed manifest, so
a bucket never records the stamp going backwards. Gaps are normal — a failed
snapshot claims a generation and does not roll it back.

The write order is load-bearing and is the same write-last discipline the marker
always had: objects → flush → manifest → flush → marker → flush. So a manifest is
never visible before the objects it describes are durable, and a marker is never
visible before the manifest it names. The only mismatch this ordering can produce
is a manifest one generation ahead of the marker, which is a manifest that is
still exactly right — which is why hydration keys off the manifest.

The manifest is a directory-shaped prefix rather than a flat
`sessions/<id>.manifest.json` because `objectstore.Backend` has no single-object
read: `Hydrate` is the only way to pull bytes down, and it takes a prefix whose
exact-match object is explicitly not "under" it. That is also why the commit
marker itself cannot be what hydration reads. Widening the published interface
with a `Get` would break every out-of-repo backend module.

**What hydration does with it.** The tree is pulled into a staging directory as
before, then everything the committed manifest does not name is removed — before
the staging directory is renamed into place, so an uncommitted object is never
observable at the session path even for an instant. The orphaned objects are
**left in the bucket, never deleted**: reclamation is the operator's, and this
seam never removes remote data.

Two deliberate exceptions:

- **No manifest at all** (a bucket written by an older build, or a session that
  has never completed a snapshot) falls back to materialising everything under
  the prefix — byte-for-byte the behaviour that shipped before — and logs it.
- **Content-addressed blobs are never pruned**, even when the manifest does not
  name them. That is a correctness requirement, not a bandwidth saving: the blob
  store sweeps *local* disk under an LRU byte budget while a snapshot never
  deletes remotely, so a blob referenced by a `nexus-blob:` URI in the committed
  history can legitimately be in the bucket and absent from the manifest.
  Pruning it would break a URI that resolves today. The exemption is also
  exactly coextensive with "objects written outside a snapshot", because
  write-through and its retry queue push nothing but blobs. Sealed journal
  segments are immutable too and are deliberately **not** exempt — one from an
  interrupted generation carries events the committed history does not.

**Where the guarantee stops.** A snapshot overwrites in place and the manifest
names *paths*, not versions. An interrupted snapshot that got as far as
re-uploading a mutable object — `context/conversation.jsonl`, the active journal
segment, a per-plugin `store.db` — has already replaced the committed
generation's bytes at that key, and no listing of paths brings them back.
Hydration restores exactly the committed *set*; within that set an overwritten
object carries the dead generation's bytes. Closing that window means
per-generation object keys, which is the generation-directories design costed and
rejected above. `TestInterruptedSnapshotCanOverwriteACommittedObjectInPlace` in
`pkg/engine` pins the boundary so it is not rediscovered by accident;
`TestInterruptedSnapshotRestoresTheCommittedGenerationIntact` and
`TestHydrationRestoresOnlyTheCommittedGeneration` pin the guarantee.

**Cost.** The manifest is re-uploaded whole on every snapshot, which is the cost
the commit marker was originally designed to avoid and which was accepted here in
exchange for correctness. Measured by `BenchmarkSessionSnapshot`'s `manifest_KiB`
metric: **18.9 KiB for the 1007-object / 91 MiB session** (~19 bytes per object,
0.02% of the tree, against a 90.7 MiB per-turn upload). A blob-heavy session pays
more per object because a content-addressed path is 69 characters — 157 KiB for
2009 objects — which is still 0.55% of that shape's 27.9 MiB per-turn upload.

#### Failure policy: `degrade` and `strict`

A snapshot that cannot complete is where `core.object_store.failure_policy`
earns its keep. Both values retry, both publish the outage on the bus, and both
recover with **no operator action**. What differs is whether the session keeps
taking turns while the store is unreachable.

Under **`degrade`** — the default — the session keeps running against the local
working copy. The failure is a warning, the state is queued for retry, and turns
carry on. The honest caveat belongs here rather than only in a comment: *during
a long outage the durability guarantee is not being met even though nothing is
failing.* Work the user watched happen exists only on local disk, and a host that
dies while degraded loses it. That is the trade the operator chose by selecting
it.

Under **`strict`** the failure additionally raises `core.error` and closes a
turn gate: every subsequent `io.input` is vetoed until a snapshot succeeds. Be
precise about what that buys, because the tempting summary is wrong:

> **The turn that hit the outage already happened.** Its output was streamed to
> the user, its tools ran, and its side effects are in the world. Nothing in
> Nexus un-runs it.

`strict` refuses the *next* turn, which is the last point at which nothing has
happened yet. A genuine pre-commit gate would need a vetoable turn-boundary
event, which does not exist — and would not help if it did, since by the time an
agent loop can report a turn the work is done. So the guarantee is: **no turn
ever runs against state whose predecessor was not durably stored, and the
divergence is never silent.** Not: "the failed turn was prevented".

The gate is a `before:io.input` subscriber at priority 200, behind every other
one (`nexus.control.cancel` and `nexus.mcp.client` both sit at 5), so slash
commands and cancellation still work while it is closed — an operator whose
bucket is down must still be able to stop the run.

**Retry, and the bound.** One background worker per run retries with exponential
backoff (1 s, doubling, capped at 60 s). It carries two kinds of work: a bounded
queue of deferred pushes, capacity 256 objects, fed by blob write-through
failures; and a "whole-tree snapshot pending" flag, the backstop, set by a failed
snapshot, a failed flush or a queue overflow. Overflow therefore **does not lose
work** — the discarded push is replaced by a snapshot that re-uploads everything
the store does not already hold at the right size, which is strictly stronger
and merely coarser. The bounds, the schedule and the timeouts are compiled-in
constants rather than config keys, on the same reasoning E3-S2 applied to the
write-through constants: an operator who wants to tune them is asking for a
different `failure_policy`.

**Recovery drains itself.** Either the next turn-boundary snapshot closes the
episode, or — on an idle session where no further turn is coming — the retry
worker does, publishing its snapshot under the `retry` trigger. Exactly one
`session.storage.degraded` goes out per outage and one
`session.storage.recovered` when it ends, so a subscriber counts outages rather
than failed requests.

**One deliberate exception.** A blob write-through failure never closes the
`strict` gate. Write-through is an optimisation in front of the snapshot, which
re-uploads anything missing; failing a turn because it stumbled on an object the
very next snapshot repairs would make `strict` fire on transients it is not there
to catch. Such a failure still queues for retry and still counts towards the
degraded state.

Hydration failure is the one thing both policies treat identically: it fails the
boot. `degrade` means "fall back to the local copy", and at hydrate time there is
no local copy.

#### Cost

The snapshot is `O(tree size minus the immutable share)` on every turn, and a
session tree only grows. Measured on an M1 Max against the in-memory backend
(engine work only — staging, checkpoint, tree walk, per-object handoff — with no
network):

| Tree | Objects | Size | Uploaded per turn | Per turn |
|------|---------|------|-------------------|----------|
| 10 files + a 100-row store | 17 | 0.05 MiB | 17 objects / 0.05 MiB | ~13 ms |
| 200 files + a 5k-row store | 207 | 6.0 MiB | 207 objects / 6.0 MiB | ~29–35 ms |
| 1000 files + a 50k-row store | 1007 | 91 MiB | 1007 objects / 91 MiB | ~155–175 ms |
| 1000 blobs + a 50k-row store | 2007 | 90 MiB | **7 objects / 28 MiB** | ~137 ms |

The first three rows are ordinary `files/` output, which nothing in the tree
proves immutable and which is therefore still re-uploaded in full every turn. The
last row is the same volume of bytes held as content-addressed blobs: before
immutable-skip it uploaded 2007 objects and 90 MiB per turn (~200 ms); after, it
uploads the 7 mutable objects and 28 MiB, of which the per-plugin `store.db` is
almost all. On a 100 Mbit link that is roughly 7.6 s per turn down to 2.3 s.

Underneath is a fixed floor of roughly 12 ms (checkpoint, `VACUUM INTO`, journal
barrier, fsync) plus 500–600 MiB/s of local throughput; real network time lands
on top. `BenchmarkSessionSnapshot` in `pkg/engine` reproduces all four rows,
reporting `puts/op` and `upload_MiB/op` alongside the tree size. Every snapshot
logs `objects`, `bytes`, `objects_uploaded`, `bytes_uploaded`,
`objects_skipped`, `bytes_skipped`, `db_bytes` and `duration`, and publishes the
same numbers as `session.snapshot.result`, so the growth is visible rather than
inferred.

The residual `store.db` cost is `O(database size)` per turn regardless of how
little changed, and delta upload for mutable files and a size-dependent snapshot
cadence are still deliberately not designed.

### The other roots

The session tree is one of four roots the seam covers, and the only one with a
commit marker, a turn-by-turn history and a lock. The other three are:

| Root | Object key | Lifecycle |
|------|------------|-----------|
| App-scope plugin storage | `plugins/<pluginID>/store.db` | Hydrated at `Boot` before any plugin can open a handle; snapshotted at every turn boundary and at shutdown. |
| Agent-scope plugin storage | `agents/<agent_id>/plugins/<pluginID>/store.db` | The same, when `core.agent_id` is set. With it empty the handle collapses to app scope and so does the key. |
| Eval run output | `eval/<run-id>/…` | Published once by `nexus eval run` when its `--config` names a backend. Written once, never mutated, so there is nothing to hydrate. |

Keys mirror the on-disk layout beneath the data root and sit **beside**
`sessions/`, never under it. That is the reservation the `sessions/` segment was
chosen for. Nesting shared state under the session that flushed it would give
every session its own copy of a machine-wide store, which is how
`nexus.gate.token_budget`'s tenant token ceiling would quietly turn into a
per-session ceiling with nothing erroring.

One interface serves all four: no `Backend` method mentions a root, and the
per-root policy — when to push, what wins on a collision, whether a local copy
may be overwritten — lives entirely on the engine side in
`pkg/engine/shared_objectstore.go`.

The journal needs no separate row: it lives at `<session>/journal/` and is
captured at a consistent instant inside the session snapshot.

Details, including the one-writing-host-at-a-time constraint a shared root
brings, are in [Per-Plugin Storage → Object storage for app and agent
scope](storage.md#object-storage-for-app-and-agent-scope).

### Two hosts, one session

The seam assumes a session has exactly one writing host at a time. **Nothing
enforces that**, and the failure when it is violated is completely silent: two
hosts hydrate the same session ID, both snapshot the whole tree at their own turn
boundaries, and the loser's conversation history, journal and per-plugin
`store.db` are overwritten at whole-file granularity with no error anywhere. On
ephemeral compute, an instance the scheduler presumed dead but which is still
running is routine rather than exotic.

An **owner marker** makes that diagnosable. It is a small JSON object at
`sessions/<id>.owner/owner.json` — a sibling of the session prefix, exactly like
the commit marker, so it never hydrates into the tree and never becomes an input
to the next snapshot:

| Field | Meaning |
|-------|---------|
| `host` | `os.Hostname` of the holder. Per-instance on Kubernetes, Cloud Run and ECS. |
| `pid` | The holder's OS process ID. Meaningful only together with `host`. |
| `instance_id` | Unique per engine run — what tells two containers sharing a hostname and a PID apart. |
| `claimed_at` | When the session was claimed. |
| `heartbeat_at` | Refreshed every 30 seconds while the run is live. |

`Boot` reads the marker before it writes its own — for a resumed session and a
brand-new one alike — and a **clean `Stop` removes it**, while
[`Abandon`](#dropping-a-session-without-closing-it) deliberately leaves it. The read also runs when
hydration short-circuited because a local tree was already present, so a warm
host whose stale local copy is shadowing a session another host has since taken
over is detected too.

If someone else still looks like the holder, the engine logs at **error** level
and emits [`session.owner.conflict`](../events/reference.md#session-events) —
and then carries on exactly as it would have. **This detects; it does not
prevent.** No lock is taken, no fencing token is issued, nothing is refused and
nothing waits. Refusing on a detection that can be wrong would let a false
positive strand a session nobody can open, which is worse than the failure being
detected. Fencing, expiry semantics and refusal are a real lease, and a real
lease is a separate piece of work.

The alarm is only worth having if it stays quiet on the happy path, so a marker
is treated as live only when nothing says otherwise:

| Signal | Verdict |
|--------|---------|
| The marker's `instance_id` is this run's | Ours. Silent. |
| `host` matches this host and the PID is no longer running | The holder crashed here. Silent — the same signal-0 liveness probe `session.lock` uses, and sound only because the host matches. |
| `heartbeat_at` stopped advancing more than 5 minutes ago | Holder presumed gone. Silent, logged at info as a takeover. |
| Anything else | Conflict. |

Both thresholds are constants, not config keys: 30 seconds between beats, ten
missed beats before a marker reads as stale. The slack is deliberate — the
timestamp comes from another machine's clock, and an alarm that fires because two
hosts disagree about the time is an alarm everybody learns to ignore. The
residual false alarm is a crash on a *different* host resumed inside the
staleness window; nothing can distinguish that from a real second writer without
a lease.

The local session lock is **not** a substitute and does not go away. It carries a
PID and is excluded from the seam for exactly that reason — a PID from host A
means nothing on host B — so it can only see one machine. The two answer
different questions.

The conflict event is raised after `startJournal` and after plugin `Init`, not at
the point of detection. The bus assigns a sequence number to every event whether
the journal's wildcard is subscribed or not, and the writer only flushes
contiguous sequences, so a single event emitted during hydration would stall the
drain and empty the journal for the whole run.

**Scope.** The marker covers session trees only. The shared roots — app- and
agent-scope plugin storage — have a stronger version of the same problem and no
marker yet; see [Per-Plugin Storage → Object storage for app and agent
scope](storage.md#object-storage-for-app-and-agent-scope).

### Dropping a session without closing it

`Stop` and `Abandon` are the two ways a run ends, and they are not variants of
each other. `Stop` closes a session: it takes a final snapshot, flushes, removes
the owner marker, shuts plugins down, closes the journal and per-plugin SQLite,
and releases the local lock. `Abandon` **drops** one:

| | `Stop` | `Abandon` |
|---|---|---|
| Tick heartbeat, run-scoped subscriptions | stopped | stopped |
| Object-store recovery worker | stopped | stopped |
| Blob write-through worker | stopped, queue drained | stopped, queue **discarded** |
| Owner-marker heartbeat | stopped | stopped |
| Owner marker in the store | **deleted** | **left in place** |
| Shutdown snapshot, `Flush` | yes | **no** |
| Plugin `Shutdown`, journal close, SQLite close, session metadata, session lock | all done | **none of it** |

It exists for two callers. A host on ephemeral compute that is being reclaimed
and does not want to pay a whole-tree snapshot of a session nobody will read.
And a test simulating a process death — an abandoned engine that keeps
heartbeating and retrying is still writing into the bucket the test is trying to
tear down, and stopping those workers without recording a clean exit is the only
honest way to fake a kill.

The owner marker is the part worth understanding. A clean `Stop` deletes it
because the broker's ordinary release-and-respawn cycle resumes the same session
minutes later, well inside the five-minute staleness window, and a marker left
behind would fire the split-brain alarm on every legitimate resume. `Abandon`
does not delete it, because doing so would record a clean release that never
happened — the next host would resume in silence exactly where the evidence
matters most. It does stop the heartbeat, though: a marker that kept beating for
a run that no longer exists would read as live for ever and turn a genuine
takeover into a reported conflict. With the beat stopped and the marker left, the
next host applies the ordinary staleness rules from the table above — a dead PID
on the same host, or a heartbeat past the threshold anywhere else — and takes
over quietly.

`Abandon` writes nothing and closes nothing local, so on its own it leaks the
journal writer's goroutine and the open handles under the session tree. That is
correct for a process about to exit and wrong for one that is not; a long-lived
host can call `Stop` afterwards, which with the store handle already gone
degenerates to local teardown and still writes nothing remote. It is idempotent,
safe after `Stop` or on an engine that never booted, and costs nothing when no
object store is configured.

### What survives a kill

The recovery point is the **last completed turn**. A process that dies without
running `Stop` — SIGKILL, a container eviction, a Lambda timeout — loses only
what was written after the last `agent.turn.end`; everything up to and including
that boundary is restorable from the store, on a host that has never seen the
session.

`TestKillAndResumeRestoresIdenticalSessionState` in `pkg/engine` is that claim
as an assertion, and it runs untagged inside `make test` against the in-memory
backend. It boots an engine, writes conversation history, artifacts, blobs and
per-plugin SQLite rows, completes one turn, writes some more, and then abandons
the engine with every handle still open — no `Stop`, no shutdown snapshot, no
flush — which is what forces the turn-boundary snapshot to be the only thing
that could have saved the session. A second engine over a **separate, empty**
data root then resumes the same session ID and is held to equality rather than
to "it opened": the same history bytes and the same replayed messages, the same
artifact bytes, the same blob bytes and media type, the same 500 SQLite rows
through the storage manager with `PRAGMA integrity_check` clean — and none of
the three writes made after the turn boundary. A companion test compares the
whole hydrated tree against a content-hash fingerprint of the killed one, file
by file.

`TestKillAndResumeAgainstMinIORestoresIdenticalSessionState` in
`modules/objectstore-s3` repeats that scenario against MinIO, where a real wire
protocol, real latency and real error mapping are in the loop. It lives in the
S3 module rather than beside its sibling because that is the only package in the
tree that can hold a real `engine.Engine` and a real S3 wire at once — the
engine is in the root module, the backend is in that one — and it reaches the
engine through the exported `engine.NewFromBytes` and an `object_store:` block,
the same path an operator's config takes. It adds one assertion the in-memory
run cannot make: the `store.db` object is pulled back out of the bucket with a
client that is **not** the backend under test and opened as a database, so
"MinIO is holding a valid, queryable, fully checkpointed SQLite file" is
asserted rather than inferred. Against the memory backend an upload is a
`[]byte` copied inside the process, so a broken WAL checkpoint would still round
trip; here it does not. Both MinIO kill tests express the kill as
`engine.Abandon()` rather than by simply dropping the engine: against a real
bucket an engine nobody stopped keeps a heartbeat and a retry worker writing
objects into the bucket the test then has to delete, and `Abandon` is the one
teardown that stops them without recording the clean exit the scenario depends
on not having happened. See [Dropping a session without closing
it](#dropping-a-session-without-closing-it).

**The mid-flush kill, and the boundary it stops at.**
`TestMidFlushKillAgainstMinIORestoresTheCommittedGeneration` kills a process
partway through a snapshot — the tree objects of the dead generation reach the
bucket, the manifest and the commit marker never do — and asserts what actually
survives:

- Hydration restores exactly the committed generation's **object set**. A key
  the dead generation added and the committed manifest does not name is not
  materialised.
- Orphaned objects are left in the bucket, not deleted.

and what does not, per **Where the guarantee stops** under [The generation stamp
and the per-object manifest](#turn-boundary-snapshots): an object the dead
generation **overwrote in place** carries the dead generation's bytes, because
the manifest names paths rather than versions. The restored `store.db` reads the
uncommitted generation, and the test asserts that rather than hoping otherwise.
`TestInterruptedSnapshotCanOverwriteACommittedObjectInPlace` pins the same
boundary against the in-memory backend; the MinIO run is where an argument that
a real store would somehow keep the old bytes would be exposed.

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

### Writing a backend

A backend is an implementation of `objectstore.Backend` plus a `Register` call.
It can live in any module: the interface names no bucket API, credential type
or HTTP client, and `pkg/engine/objectstore` imports nothing outside the
standard library.

Two rules are easy to read past, and both corrupt sessions rather than
producing an error:

**Keys are validated, not merely documented.** Every method must reject a
malformed key or prefix with an error wrapping `objectstore.ErrInvalidKey`,
before touching the store or the filesystem. A key is `/`-separated, non-empty,
with no leading or trailing `/`, no empty segment, no `.` or `..` segment, no
`\`, and no NUL. `objectstore.ValidateKey` and `objectstore.ValidateKeyPrefix`
implement exactly this; the `..` ban is what stops a hostile key from writing
outside a hydration destination.

**Prefixes match whole segments.** Key `K` is under prefix `P` when `P` is
empty, or when `K` begins with `P + "/"`. Raw string matching — the native
behaviour of `ListObjectsV2` and its GCS equivalent — makes the prefix
`sessions/sess-1` select the objects of `sessions/sess-10`, which mixes two
sessions into one tree. A backend listing with a raw prefix must post-filter.
`objectstore.TrimKeyPrefix` is the rule in code, and also yields the relative
path `Hydrate` needs: `Hydrate` **strips** the prefix, so
`sessions/s1/files/a.md` under prefix `sessions/s1` lands at
`<destDir>/files/a.md`.

`Hydrate` adds and overwrites; it does not mirror. Entries already at the
destination that no object corresponds to are left alone.

#### The contract suite

`pkg/engine/objectstore/objectstoretest` holds the shared conformance suite.
Every backend — the in-memory one used by unit tests, and each out-of-tree
module — is held to the same cases: key round-tripping, overwrite,
delete-then-list, segment-aware prefixes, complete listings past one page,
absent-key behaviour, zero-byte objects, key-syntax rejection, `Flush`
idempotency and concurrent use.

```go
func TestContract(t *testing.T) {
    objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
        return newMyBackend(t) // empty, cleaned up via t.Cleanup
    })
}
```

Each case gets its own backend, so the factory must hand back an empty one — a
temp bucket, a cleared prefix, a fresh emulator namespace. `WithListProbeCount`
lowers the 1200-object pagination probe for a backend that cannot afford it
(below the backend's own page size the case stops proving anything), and
`WithoutConcurrency` skips the parallel case.

`objectstoretest.NewMemory` is the reference implementation that passes the
suite, and doubles as the substituted seam for ordinary untagged unit tests. It
is deliberately not registered as a driver at init: a `memory` backend silently
selectable in production config would discard everything on exit while
reporting success. `objectstoretest.RegisterMemory` makes it reachable by name
for the duration of one test and removes it again on cleanup.

#### Shipped backends, and what having two of them proves

Two backends ship in-repo, each in its own module:
`modules/objectstore-s3` (Amazon S3 and every S3-compatible store) and
`modules/objectstore-gcs` (Google Cloud Storage). Neither is part of
`bin/nexus`; an embedder blank-imports the one they want.

The second one was written to answer a question about the seam rather than
about Google. An interface with exactly one implementation is indistinguishable
from that implementation's API with the names changed, and the whole premise of
`pkg/engine/objectstore` is that a third party can implement it without a PR to
this repository. GCS disagrees with S3 in several specific places — deleting a
missing object is an error there and a success here; there is no region, and no
project either; the client holds resources worth closing where the AWS one does
not; uploads are not retried by default because an insert without a
precondition is not idempotent.

Every one of those turned out to be a translation the backend module owns.
`modules/objectstore-gcs` passes `objectstoretest.RunSuite` **unmodified and
with no option other than a reduced page-count probe**, and required no change
to `pkg/engine/objectstore` at all. Two things in the interface earned their
keep in the process: `Delete`'s "a missing key is not an error" rule, which
picks the S3 behaviour precisely because it is the one a retrying caller wants
and leaves the other store to absorb the difference, and the decision to have
the engine type-assert `io.Closer` rather than put `Close` on the interface,
which is why a backend holding an SDK client needed no widening of a published
type.

See [Configuration Reference](../configuration/reference.md#coreobject_store)
for the keys, their defaults and their validation behaviour.
