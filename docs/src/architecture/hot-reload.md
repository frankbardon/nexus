# Hot Reload

`Engine.ReloadConfig(newConfig *Config) error` applies a config change to a
running engine without restarting unaffected plugins. Phase 5 of Idea 10
(engine resilience) introduced it; the design is intentionally cautious so
operators can adjust gate thresholds, model assignments, and tool
allowlists in production without dropping every active session.

## Architecture

The reload runs in two phases.

### 1. Validate (atomic)

- The new config is run through the same JSON-schema validation pass that
  `Boot` performs. Every active plugin's `ConfigSchemaProvider` is
  re-checked against the new config map; engine-level fields
  (`engine.shutdown.drain_timeout`, `engine.config_watch`) are validated
  against the engine schema (`pkg/engine/engine_schema.json`).
- **Capability provider identity is pinned.** If a capability bound at
  boot (e.g. `memory.history`) would resolve to a different concrete
  plugin under the new active set, the reload is rejected. The session
  has in-flight state bound to the existing provider; a silent swap would
  strip the operator's history.
- The active-set diff is computed: which IDs are added, which removed,
  which kept with a config change.

Any error here returns immediately. The engine is unchanged.

### 2. Apply (best-effort)

The diff is walked:

| Delta             | Action                                             |
|-------------------|----------------------------------------------------|
| Plugin added      | `Init` → `Ready` (subscriptions registered by Init)|
| Plugin removed    | `Shutdown` (subscriptions released by Shutdown)    |
| Config change w/ `ConfigReloader` | `ReloadConfig(old, new)` in place; subscriptions preserved |
| Config change w/o `ConfigReloader`| `Shutdown` → fresh factory → `Init` → `Ready`      |
| Engine-only field | Swapped before per-plugin work; takes effect on next read |

### Atomicity caveat

The validate phase is atomic — failures here leave the engine state
untouched. The apply phase is **best-effort**. If a per-plugin
`ReloadConfig` or `Init` / `Ready` fails partway through, prior changes
have already taken effect (a restarted plugin has already re-subscribed
to the bus and may have written to journals or storage). The engine logs
the failure and surfaces it to the caller; we do **not** attempt to roll
back. "Undoing" a `Shutdown` is not generally possible — the plugin's
in-memory state is gone.

If a partial reload leaves the engine in a state the operator dislikes,
re-issue `ReloadConfig` with the previous config to revert.

## `ConfigReloader` opt-in

```go
// pkg/engine/plugin.go
type ConfigReloader interface {
    ReloadConfig(old, new map[string]any) error
}
```

A plugin that implements `ConfigReloader` receives the in-place hook on a
config-only change instead of going through the restart path. Both paths
are supported; the hook is purely an optimization for plugins where a
full restart would drop in-progress work (e.g. an HTTP listener with
established WebSocket connections, an MCP client mid-stream).

Implementations must be transactional from the bus's perspective:
returning an error must leave the plugin in its prior state — bus
subscriptions, in-memory data, persisted scratch — unchanged. The engine
makes no attempt to restart on top of a failed in-place reload.

## Capability pinning

Capability provider identity is pinned for the lifetime of a session.
The constraint is enforced in the validate phase: if a capability that
the running engine has resolved (e.g. `memory.history` →
`nexus.memory.capped`) would resolve to a different provider in the new
config, the reload is rejected with the error:

```
capability provider "memory.history" cannot change at runtime (nexus.memory.capped -> nexus.memory.summary_buffer); restart required to rebind session state
```

Restart the engine to change capability providers — the new session
boots with the new provider from clean state.

## Triggers

Three triggers feed `ReloadConfig`:

### `SIGHUP` (CLI)

The `cmd/nexus` binary's main loop intercepts `SIGHUP`, re-reads the
original `-config` path, and calls `ReloadConfig`. `SIGINT` and `SIGTERM`
continue to terminate the engine.

```
kill -HUP $(pgrep -f 'nexus -config')
```

### `POST /admin/reload-config` (browser plugin)

When `nexus.io.browser` is active, its HTTP server exposes
`POST /admin/reload-config`. Body is empty (re-read the original path)
or `{"path": "/abs/path/to/new.yaml"}` for an ad-hoc reload. The endpoint
returns `200 OK` with `{"ok": true}` on success or `400` with
`{"ok": false, "error": "..."}` on a validation failure; a stuck reload
returns `504` after 30s.

No auth layer yet — alpha-only; front with a reverse proxy if exposed.
Implementation lives in `plugins/io/browser/server.go`.

### `fsnotify` watcher

Off by default. Opt in via:

```yaml
engine:
  config_watch:
    enabled: true
    debounce: 1s
```

The CLI starts a watcher on the `-config` path and fires `ReloadConfig`
after each debounced edit. The watcher lives in
`pkg/engine/configwatch/`. It watches the parent directory rather than
the file itself because editors that swap-on-save (Vim's default) replace
the file's inode — a watcher on the original path would miss the swap.

The `debounce` window collapses bursts of `Write`/`Create` events on the
same path into a single reload. Editors commonly fire two or three Write
events when saving; reading a half-written YAML through the validator
would surface a confusing schema error. `1s` is well above the typical
storm and short enough that the operator perceives the reload as
instant. Tune downward for fast feedback during dev; leave at the
default (or higher) in production where rapid re-saves rarely happen.

## Bus events

External triggers can also dispatch `core.config.reload.request` on the
event bus and listen for `core.config.reload.result`. The browser admin
endpoint uses this internally — the engine subscribes to the request
event during `Boot` and emits the result back. Custom plugins or
embedders that don't want to hold an `*Engine` reference can use the
same path.

## Code locations

| Concern              | File |
|----------------------|------|
| `ReloadConfig` API   | `pkg/engine/reload.go` |
| `ConfigReloader`     | `pkg/engine/plugin.go` |
| Engine schema        | `pkg/engine/engine_schema.json` |
| `fsnotify` watcher   | `pkg/engine/configwatch/watcher.go` |
| `SIGHUP` / CLI hooks | `cmd/nexus/main.go` |
| Admin HTTP endpoint  | `plugins/io/browser/server.go` |
| Bus event types      | `pkg/events/core.go` |
| Tests                | `pkg/engine/reload_test.go` |
