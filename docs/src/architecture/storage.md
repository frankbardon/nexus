# Per-Plugin Storage

Every plugin can request a SQLite-backed storage handle scoped at session,
agent, or application level. The storage primitive is engine-native (no plugin
needs to be activated) and is exposed through `PluginContext.Storage`.

The backend is `modernc.org/sqlite` — pure Go, no CGO, FTS5 included. WAL
mode and a 5-second busy timeout are on by default.

## Scopes

| Scope | Path | Lifetime |
|-------|------|----------|
| `ScopeSession` | `<session.RootDir>/plugins/<pluginID>/store.db` | Disappears when the session is archived. |
| `ScopeAgent`   | `~/.nexus/agents/<agent_id>/plugins/<pluginID>/store.db` | Persists across sessions for one agent. Collapses to `ScopeApp` when no `core.agent_id` is configured. |
| `ScopeApp`     | `~/.nexus/plugins/<pluginID>/store.db` | Machine-wide, survives across sessions and agents. |

Multi-agent embedders (the desktop shell) set `core.agent_id` per engine
instance so each agent gets its own `ScopeAgent` partition. CLI and
single-agent embedders leave it empty, which collapses agent scope to app
scope so plugins do not end up with two separate connection pools pointing
at the same file.

The data root can be overridden via `core.storage.root` (defaults to
`~/.nexus`).

## Plugin API

```go
func (p *Plugin) Init(ctx engine.PluginContext) error {
    st, err := ctx.Storage(storage.ScopeSession)
    if err != nil {
        return err
    }

    // KV sugar — convenient for trivial put/get cases.
    if err := st.Put("last_run", []byte(time.Now().String())); err != nil {
        return err
    }
    val, ok, err := st.Get("last_run")

    // Raw SQL — for joins, transactions, virtual tables (FTS5).
    if _, err := st.DB().Exec(`CREATE TABLE IF NOT EXISTS jobs (
        id INTEGER PRIMARY KEY, payload TEXT
    )`); err != nil {
        return err
    }

    // Transactions.
    return st.Tx(func(tx *sql.Tx) error {
        _, err := tx.Exec(`INSERT INTO jobs(payload) VALUES(?)`, "work")
        return err
    })
}
```

Handles are pooled — repeated calls to `ctx.Storage(scope)` return the same
underlying `*sql.DB` for that `(scope, pluginID)` pair. The handle lives for
the lifetime of the engine; do not call `Close` on the returned `*sql.DB`.

The `kv` table is created lazily on the first KV-method call. Plugins that
only use `DB()` never see it.

## Configuration

See [Configuration Reference](../configuration/reference.md#core) for the
authoritative list. The relevant block:

```yaml
core:
  agent_id: ""                 # set by multi-agent embedders
  storage:
    root: ~/.nexus             # data root for app + agent scope
    busy_timeout_ms: 5000
    cache_size_kb: 2048
    pool_max_idle: 2
    pool_max_open: 4
```

## Concurrency

App-scope storage is shared across every session on the machine. SQLite WAL
mode handles concurrent readers cleanly, and writers serialize behind the
busy timeout. Multiple processes (two CLIs sharing the same app-scope DB
file) work but are not the design target — prefer agent or session scope
for concurrent independent workloads.

Within a single process, `Storage` is safe for concurrent use across
goroutines.

## Checkpoints and snapshots

A `store.db` is only half a database while a writer is active: committed
transactions live in `store.db-wal` until a checkpoint folds them back, and
`store.db-shm` is a process-local index into that WAL. Copying `store.db` on its
own gets a file that opens cleanly and is silently missing data — the reason
`cp store.db elsewhere` is never a backup.

The manager exposes two operations for this:

- `Manager.Checkpoint(scope)` runs `PRAGMA wal_checkpoint(TRUNCATE)` on every
  open handle at a scope, folding the WAL back into the main file and resetting
  it to zero length. `TRUNCATE` rather than `PASSIVE` (which gives up silently
  the moment a reader is present) or `FULL` (which leaves the WAL at its
  high-water mark, so one large batch costs the session forever). It blocks on
  readers, bounded by the 5 s busy timeout.
- `Manager.Snapshot(scope, destDir)` checkpoints and then `VACUUM INTO`s each
  handle to `<destDir>/<pluginID>/store.db`, returning the live path, the
  snapshot path, its size, the checkpoint result and the elapsed time.

The two steps do different jobs and both are needed. The checkpoint makes the
*live* file self-contained; `VACUUM INTO` makes the *snapshot* untearable, since
it runs inside a read transaction and so cannot be torn by a plugin committing
mid-copy. A plain `io.Copy` after the checkpoint would produce a corrupt rather
than merely stale file in that case, which is strictly worse when the result is
about to be uploaded over a good remote copy. `VACUUM INTO` also refuses an
existing destination, so snapshotting over a live database is impossible by
construction, and it compacts, so the snapshot is never larger than the live
file. The driver is pure Go, so there is no CGO backup API available; this is the
portable equivalent.

The cost is `O(database size)`: roughly 220–265 MiB/s on an M1 Max
(0.6 MiB → ~3 ms, 117 MiB → ~530 ms). `BenchmarkSnapshot` in
`pkg/engine/storage` measures it.

Only handles the manager has actually opened are covered, which is the right
set: a `store.db` in the tree with no handle has no writer in this process and
is already static.

The engine's object-store seam is the caller. It snapshots session-scope
handles at every turn boundary and never uploads `-wal`, `-shm` or `-journal`
sidecars, so the stored database restores on a host that has never seen them.
See [Sessions → Turn-boundary snapshots](sessions.md#turn-boundary-snapshots).
