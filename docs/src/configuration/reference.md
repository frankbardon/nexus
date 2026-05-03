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

## Top-level structure

```yaml
core:           # engine-level settings
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
```

| Key              | Type   | Default          | Description |
|------------------|--------|------------------|-------------|
| `fsync`          | string | `turn-boundary`  | Disk-flush policy. `turn-boundary` fsyncs once per `agent.turn.end` (good throughput, recovers to last completed turn). `every-event` fsyncs after every envelope (strongest crash guarantee). `none` skips explicit fsync (test-only). |
| `retain_days`    | int    | `30`             | Age in days past which a session's journal directory is removed on engine boot. `0` disables sweeping. In-flight sessions are never touched. |
| `rotate_size_mb` | int    | `4`              | Active segment size threshold (MiB). When `agent.turn.end` lands and the active segment exceeds this, it is compressed into `events-NNN.jsonl.zst` and the active segment is truncated. |

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
| `base_url`     | string   | `https://api.openai.com/v1/responses` | Only on `openai_native`.                              |
| `timeout`      | duration | `15s` (Brave), `30s` (others)      | HTTP request timeout.                                 |

Provider defaults:

| Provider                          | `api_key_env`                        | `model`                          |
|-----------------------------------|--------------------------------------|----------------------------------|
| `nexus.search.brave`              | `BRAVE_API_KEY`                      | n/a                              |
| `nexus.search.anthropic_native`   | `ANTHROPIC_API_KEY`                  | `claude-haiku-4-5-20251001`      |
| `nexus.search.openai_native`      | `OPENAI_API_KEY`                     | `gpt-4o-mini`                    |
| `nexus.search.gemini_native`      | `GEMINI_API_KEY` / `GOOGLE_API_KEY`  | `gemini-2.5-flash`               |

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

**Legacy keys** still accepted for backwards compatibility:
`allowed_commands`, `path_dirs`, `sandbox: <bool>` at the top level are
auto-shimmed into the equivalent `sandbox: { ... }` block. Documented as
deprecated; removal targeted for the milestone after gVisor lands.

### `nexus.tool.file`

Source: `plugins/tools/fileio/plugin.go`. Registers `read_file`, `write_file`,
`check_file_size`, `list_files`.

| Key                     | Type | Default                 | Description |
|-------------------------|------|-------------------------|-------------|
| `base_dir`              | string | *(session files dir)* | Base directory for file operations. |
| `allow_external_writes` | bool   | `false`               | Permit reads/writes outside `base_dir`. |
| `tools.<tool_name>`     | bool   | `true` for each       | Per-tool enable/disable: `read_file`, `write_file`, `check_file_size`, `list_files`. |

### `nexus.tool.catalog`

Source: `plugins/tools/catalog/plugin.go`. **No configuration.** Provides the
`tool.catalog` capability — a shared registry queried via
`tool.catalog.query`. Required by `nexus.agent.react`.

### `nexus.tool.web`

Source: `plugins/tools/web/plugin.go`. Registers `web_search` and `web_fetch`.
Requires the `search.provider` capability for `web_search`.

| Key                          | Type     | Default                                | Description |
|------------------------------|----------|----------------------------------------|-------------|
| `search.default_count`       | int      | `10`                                   | Default result count for `web_search`. |
| `search.default_safe_search` | string   | `moderate`                             | `off`, `moderate`, `strict`. |
| `search.default_language`    | string   | *(none)*                               | BCP-47 language tag (e.g. `en`, `es-MX`). |
| `fetch.user_agent`           | string   | `Nexus/0.1 (+https://...)`             | User-Agent header for `web_fetch`. |
| `fetch.timeout`              | duration | `20s`                                  | HTTP timeout. |
| `fetch.max_size`             | int      | `5242880` (5 MB)                       | Maximum response body size. |
| `fetch.default_extract`      | string   | `readability`                          | `readability` or `raw`. |
| `fetch.allowed_domains`      | list     | *(none — allow all)*                   | Allowlist of domains. |
| `fetch.blocked_domains`      | list     | *(none)*                               | Blocklist of domains. |
| `fetch.follow_redirects`     | bool     | `true`                                 | Follow HTTP redirects. |
| `fetch.max_redirects`        | int      | `5`                                    | Maximum redirect chain length. |

### `nexus.tool.knowledge_search`

Source: `plugins/tools/knowledge_search/plugin.go`. Requires
`embeddings.provider` and `vector.store`.

| Key                  | Type   | Default            | Description |
|----------------------|--------|--------------------|-------------|
| `tool_name`          | string | `knowledge_search` | Name of the registered tool. |
| `top_k`              | int    | `5`                | Default chunks to return (LLM may override; capped at 50). |
| `include_metadata`   | bool   | `true`             | Include vector metadata alongside chunks. |
| `embedding_model`    | string | *(none)*           | Override the default embedding model. |
| `namespaces`         | list   | *(required)*       | Allowed vector store namespaces. |
| `default_namespaces` | list   | *(required)*       | Namespaces searched when the LLM doesn't specify. |

### `nexus.tool.pdf`

Source: `plugins/tools/pdf/plugin.go`. Registers `read_pdf`. Requires
`pdftotext` (poppler-utils) on `PATH`.

| Key               | Type     | Default                | Description |
|-------------------|----------|------------------------|-------------|
| `pdftotext_bin`   | string   | `pdftotext`            | Path or name of the `pdftotext` binary. |
| `pdfinfo_bin`     | string   | `pdfinfo`              | Path or name of `pdfinfo` (optional). |
| `timeout`         | duration | `30s`                  | Per-extraction timeout. |
| `save_to_session` | bool     | `false`                | Persist extracted text to session files. |
| `save_file_name`  | string   | *(derived from PDF)*   | Custom filename for the saved text. |

### `nexus.tool.opener`

Source: `plugins/tools/opener/plugin.go`. Registers `open_path`.

| Key        | Type     | Default                                                         | Description |
|------------|----------|-----------------------------------------------------------------|-------------|
| `open_cmd` | string   | platform default (`open` macOS, `xdg-open` Linux, `start` Win)  | Override the platform "open" command. |
| `timeout`  | duration | `10s`                                                           | Per-open timeout. |

### `nexus.control.hitl`

Source: `plugins/control/hitl/plugin.go`. **No configuration.** The
unified human-in-the-loop primitive. Registers the LLM-facing
`ask_user` tool with an extended schema (`prompt`, `mode`, `choices`,
`default_choice_id`, `deadline_seconds`) and routes
`hitl.requested` / `hitl.responded` events between requesters (the
tool, gates, memory plugins) and IO surfaces. Replaces the prior
`nexus.tool.ask`. See [Human-in-the-Loop plugin docs](../plugins/control/hitl.md).

### `nexus.tool.code_exec`

Source: `plugins/tools/codeexec/plugin.go`. Registers `run_code` (Go).
Two compilers selected via `compiler`:

- `yaegi-host` (default) — in-process Yaegi interpreter. Full dynamic
  bindings (`tools.*`, `parallel.*`, skill helpers); no kernel isolation.
- `yaegi-wasm` — embedded Yaegi runner inside a wazero-managed Wasm
  sandbox. Capability-gated I/O via `nexus_sdk/{http,fs,exec,env}`. v1
  forfeits `tools.*`, `parallel.*`, and skill helpers — the bridge SDK
  does not surface them.

| Key                 | Type   | Default                    | Description |
|---------------------|--------|----------------------------|-------------|
| `compiler`          | string | `yaegi-host`               | `yaegi-host` or `yaegi-wasm`. The latter requires `sandbox.backend: wasm`. |
| `timeout_seconds`   | int    | `30`                       | Script timeout in seconds. |
| `max_output_bytes`  | int    | `65536`                    | Maximum captured output. |
| `max_workers`       | int    | `runtime.NumCPU()`         | Concurrency cap for `parallel.*` (yaegi-host only). |
| `persist_scripts`   | bool   | `true`                     | Write executed scripts to session files. |
| `reject_goroutines` | bool   | `true`                     | Reject scripts that spawn goroutines. |
| `allowed_packages`  | list   | *(stdlib whitelist)*       | Importable stdlib packages. |
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
| `prompt`            | string | *(default)*      | Inline summary prompt. |
| `prompt_file`       | string | *(none)*         | Path to a summary prompt file (overrides `prompt`). |

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
| `embedding_model`       | string | *(none)*               | Override the embedding model. |
| `auto_store_compaction` | bool   | `true`                 | Store summaries when `memory.compacted` fires. |
| `auto_store_user_input` | bool   | `false`                | Store user messages on every input (opt-in). |
| `section_priority`      | int    | `45`                   | Priority of the recalled-memory section in the system prompt. |
| `require_approval.enabled`                    | bool     | `false`   | Emit `hitl.requested` before each `vector.upsert`. Off = unchanged behavior. |
| `require_approval.default_choice`             | string   | *(none)*  | Choice ID picked when the deadline expires (e.g. `reject`). Empty = treat timeout as cancelled. |
| `require_approval.timeout`                    | duration | *(none)*  | Optional deadline (`5m`, `30s`, …). |
| `require_approval.match.namespace_glob`       | string   | *(any)*   | Only require approval when the configured `namespace` matches this glob. |
| `require_approval.match.size_threshold_bytes` | int      | *(any)*   | Only require approval when the document content is at least this many bytes. |

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

---

## Vector store

### `nexus.vectorstore.chromem`

Source: `plugins/vectorstore/chromem/plugin.go`. Provides `vector.store`.

| Key        | Type   | Default          | Description |
|------------|--------|------------------|-------------|
| `path`     | string | `~/.nexus/vectors` | Directory for persistent storage (one subdir per namespace). |
| `compress` | bool   | `false`          | Gzip-compress JSON on disk. |

---

## RAG

### `nexus.rag.ingest`

Source: `plugins/rag/ingest/plugin.go`. Backs the `nexus ingest` CLI subcommand
and the `rag.ingest` event handler.

| Key                   | Type | Default                    | Description |
|-----------------------|------|----------------------------|-------------|
| `chunker.size`        | int  | `1000`                     | Characters per chunk. |
| `chunker.overlap`     | int  | `200`                      | Character overlap between chunks. |
| `cache_dir`           | string | `~/.nexus/vectors/_cache` | Embedding cache directory (hash → vector). |
| `backfill`            | bool | `true`                     | Walk watched directories at startup and ingest pre-existing files. |
| `watch`               | list | *(empty)*                  | File watch entries; each is `{path, glob, namespace}`. |
| `watch[].path`        | string | *(required)*             | Directory to watch. |
| `watch[].glob`        | string | *(empty — match all)*    | Glob pattern for files to ingest. |
| `watch[].namespace`   | string | *(required)*             | Vector store namespace. |

Requires `embeddings.provider` and `vector.store`.

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

### `nexus.io.test`

Source: `plugins/io/test/plugin.go`. Non-interactive testing transport.

| Key                      | Type     | Default     | Description |
|--------------------------|----------|-------------|-------------|
| `inputs`                 | list     | *(empty)*   | Scripted user inputs (fed sequentially). |
| `input_delay`            | duration | `500ms`     | Delay between inputs. |
| `approval_mode`          | string   | `approve`   | `approve`, `deny`, `per-prompt`. |
| `approval_rules`         | list     | *(empty)*   | Per-prompt rules: each `{match: <substring>, action: <approve|deny>}`. |
| `hitl_responses`         | list     | *(empty)*   | Scripted answers to `hitl.requested` events. Bare strings are treated as `free_text`; `{choice_id: ..., free_text: ...}` maps populate the corresponding response fields. |
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

## Skills

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

Source: `plugins/gates/token_budget/plugin.go`.

| Key                 | Type  | Default                                     | Description |
|---------------------|-------|---------------------------------------------|-------------|
| `max_tokens`        | int   | `100000`                                    | Session token ceiling. |
| `warning_threshold` | float | `0.8`                                       | Warn when usage reaches this fraction. |
| `message`           | string | `Token budget exhausted for this session.` | Veto message. |

### `nexus.gate.rate_limiter`

Source: `plugins/gates/rate_limiter/plugin.go`. Pauses (re-emits via
`gate.llm.retry` after a wait) rather than rejecting.

| Key                   | Type   | Default                                                     | Description |
|-----------------------|--------|-------------------------------------------------------------|-------------|
| `requests_per_minute` | int    | `60`                                                        | Requests allowed per `window_seconds`. |
| `window_seconds`      | int    | `60`                                                        | Sliding window length. |
| `pause_message`       | string | `Rate limit reached. Pausing for {seconds}s...`             | Output template; `{seconds}` is interpolated. |

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
