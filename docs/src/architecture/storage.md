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
