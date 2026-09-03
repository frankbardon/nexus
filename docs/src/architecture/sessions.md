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
named engine subsystems. Each has one of three dispositions, recorded in code as
`engine.SessionTreeWriters()` so an enforcement test can consume it:

| Disposition | Meaning |
|-------------|---------|
| **emit** | Announces every write on the bus; a sync backend can push it as it lands. |
| **turn-boundary-only** | Silent on the bus by decision. The bytes still reach the store, through the whole-tree snapshot taken at `agent.turn.end` and at shutdown. |
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
| `pkg/engine/blobs` | `blobs/<xx>/<sha256>.bin` and `.meta` | turn-boundary-only | Content-addressed and immutable, so the snapshot already treats each blob as a once-ever upload — the repeated cost a real-time push would remove is not being paid. The package deliberately has no bus dependency. |
| `pkg/engine/storage/sqlite.go` | `plugins/<pluginID>/store.db` | **excluded-by-design** | WAL mode. Committed frames live in `store.db-wal` until a checkpoint, so streaming partial writes uploads a database that is corrupt, or plausible and silently stale. It reaches the store only as a checkpointed `VACUUM INTO` snapshot; the `-wal` / `-shm` / `-journal` sidecars never cross the seam. |
| `pkg/engine/session_lock.go` | `session.lock` | **excluded-by-design** | The file carries the local PID of the owning process and `Boot` refuses to start against a live one. Round-tripping it stamps one host's PID onto every later resume — correct by coincidence on a fresh container, and wrong the moment that number is in use. |

### The invariant, and the test that holds it

> **A write under a session tree must announce itself on the bus.** Real-time sync is
> exactly as complete as the events are, so a write nothing announced is history that
> quietly never arrives.

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

With no backend named — the default — no object-store code runs at all: no
handle is opened, no snapshot handler is subscribed, and a backend sitting
registered in the process is never touched. Every shipped profile under
`configs/` leaves the seam inert, and
`TestDefaultPathNeverTouchesARegisteredBackend` in `pkg/engine` holds the
default path to it.

### Lifecycle points

The engine touches the seam in exactly four places, all in `pkg/engine`:

| When | What happens |
|------|--------------|
| Top of `Boot` | The configured backend is resolved once. A failure here fails the boot. |
| Before a resumed workspace is opened | The whole tree is hydrated from the store under the object key prefix `sessions/<session id>`. |
| Every turn boundary | The whole tree is snapshotted and made durable, then a commit marker is published. |
| End of `Stop` | A final snapshot runs, then `Flush`, then the backend is released. |

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
| `blobs/<xx>/<sha256>.bin` and `.meta` | Content-addressed. Different bytes would have a different sha256 and therefore a different name, so a file at that path either holds those bytes or does not exist. |

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
for and what it saved. Anything that later writes a per-object manifest must
build it from the committed set, **not** from "what was uploaded this turn" — a
manifest of only-what-was-uploaded would describe a session with its journal
segments and blobs missing.

#### Failure, and the commit marker

Object stores have no multi-object transaction, so a tree spread over many
objects cannot be replaced atomically. Three properties give
"a failed or partial upload never replaces a good remote copy" anyway:

1. Nothing is uploaded from a file that could be torn — see above.
2. A snapshot **never deletes**. It adds and overwrites only, so a failure
   cannot remove remote state it did not successfully replace.
3. A **commit marker** at `sessions/<session id>.snapshot.json` is written and
   flushed only after every other object is durable. It therefore only ever
   advances past a complete snapshot: a failed or half-finished upload leaves it
   naming the previous one, which is the snapshot guaranteed to be restorable.

The marker is a *sibling* key, not a member of the tree. Because prefixes match
whole segments, `sessions/<id>.snapshot.json` is deliberately not under prefix
`sessions/<id>`, so it never hydrates back into the session and never becomes an
input to the next snapshot.

Generation directories — write the whole tree under `sessions/<id>/gen-<n>/` and
flip a pointer — were considered and rejected. They give true atomic replace at
the cost of a second full copy of every session in the bucket and an
indirection hydration would have to resolve on every boot. The marker answers
the same question for one small object.

The marker is currently **written but not read back**. Hydration pulls whatever
objects exist under `sessions/<id>` without comparing them against
`sessions/<id>.snapshot.json`, so a tree left mixed by an interrupted snapshot —
some objects from snapshot *N*, some from a half-finished *N+1* — hydrates
silently and is indistinguishable from a clean one. The marker still bounds the
damage (a snapshot never deletes, so the mixed tree is a superset of a complete
one), and `TestHydrationDoesNotValidateTheCommitMarker` in `pkg/engine` pins
that behaviour so a future change to it is deliberate rather than accidental.

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

E4-S4 repeats the same scenario against MinIO, where a real wire protocol is in
the loop.

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

See [Configuration Reference](../configuration/reference.md#coresessionsobject_store)
for the keys, their defaults and their validation behaviour.
