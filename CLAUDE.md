# Nexus

Modular AI agent harness. Pure event-driven Go. Core manages event lifecycle + plugin registry only — all behavior via composable plugins.

## Quick Reference

```bash
make build        # Build binary to bin/nexus
make run          # Build and run with default config (configs/default.yaml)
make test         # Run all tests
make fmt          # Format code (gofmt)
make vet          # Run go vet
make lint         # Run staticcheck (includes vet)
```

Run specific profile: `bin/nexus -config configs/coding.yaml`

Run integration tests: `go test -tags integration ./tests/integration/ -v`

Needs an LLM provider API key in env or `.env` file (e.g. `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`).

## Architecture

All comms via central typed event bus — plugins never call each other direct.

- **Engine** (`pkg/engine/`) — Event bus, plugin registry, lifecycle, session workspace, config loading. Only "core" code.
- **Events** (`pkg/events/`) — Typed event payload structs by domain: `core.go`, `llm.go`, `agent.go`, `tool.go`, `io.go`, `memory.go`, `skill.go`, `session.go`, `schema.go`.
- **Plugins** (`plugins/`) — All behavior lives here. Each implements `engine.Plugin`.
- **Desktop shell** (`pkg/desktop/`) — Reusable framework to embed Nexus in Wails desktop app. Manages per-agent engine lifecycles, settings, sessions, shell services.
- **Desktop app** (`cmd/desktop/`) — Reference multi-agent desktop app hosting hello-world + staffing-match agents.
- **CLI entry point** (`cmd/nexus/main.go`) — Creates engine, registers plugins, runs with signal handling.
- **Plugin registry** (`pkg/engine/allplugins/`) — Shared `RegisterAll()` function used by both `cmd/nexus` and `pkg/testharness`. Single source of truth for plugin registration.
- **Test harness** (`pkg/testharness/`) — Integration test framework. Boots real engine with `nexus.io.test` plugin, provides two-tier assertions (deterministic + semantic LLM judge).
- **Integration tests** (`tests/integration/`) — Go tests behind `//go:build integration` tag. Each test config in `configs/test-*.yaml` maps to test functions. Two modes:
  - **Mock mode** (`mock_responses` set): No LLM calls, no API key, sub-second. Use for gate/plugin behavior tests. Mock intercepts `before:llm.request` at priority 20 (after gates at 10), so gates still fire normally.
  - **Live mode** (no `mock_responses`): Real LLM calls via provider. Requires `ANTHROPIC_API_KEY`. Use for end-to-end agent behavior and semantic output validation.

**All Claude updates must update relevant docs in `docs/`.**
**Core system updates should be genericized and treated as reusable, single-use plugins shouldn't in `plugins` folder**

### Embedder API (library use)

Nexus embeds in another Go process (e.g. Wails desktop shell) without owning signals or process lifecycle. Embedder path:

1. `engine.New(configPath)` — creates engine. `configPath == ""` uses `DefaultConfig()`.
2. `eng.Registry.Register(id, factory)` — register plugins embedder wants. Embedder can mutate `eng.Config.Plugins.Active` and `eng.Config.Plugins.Configs` in memory; no on-disk YAML needed.
3. `eng.Boot(ctx)` — non-blocking. Starts session, boots plugins via lifecycle manager, installs run-scoped bus subs, starts tick heartbeat. Returns when engine ready.
4. `<-eng.SessionEnded()` — channel signals when plugin emits `io.session.end`. Embedders `select` on this plus own lifecycle signals.
5. `eng.Stop(ctx)` — tears down tick goroutine, unsubs run-scoped handlers, finalizes session metadata, shuts plugins in reverse dep order.

`eng.Run(ctx)` = CLI convenience wrapper: `Boot` + wait-for-signal-or-session-end + `Stop`. **Embedders must call `Boot`/`Stop` direct, never `Run`** — `Run` installs own `SIGINT`/`SIGTERM` handler, conflicts with host process (Wails, tests) owning signals.

### Plugin Interface

Every plugin implements `engine.Plugin` (`pkg/engine/plugin.go`):
- `ID() string` — Dotted ID (e.g. `nexus.tool.shell`)
- `Init(ctx PluginContext) error` — Gets config, bus, logger, data dir, session
- `Ready() error` — Called after all plugins init'd
- `Shutdown(ctx context.Context) error`
- `Subscriptions() []EventSubscription` — Events plugin listens to
- `Emissions() []string` — Event types plugin may emit

### Event Flow

Plugins subscribe with optional priority ordering + filtering. Dispatched synchronously. Vetoable events (`before:*` prefix) let handlers block actions.

### Plugin Directory Layout

Each plugin = single package under `plugins/`:
```
plugins/
  agents/react/          # ReAct agent loop
  apps/helloworld/       # Built-in hello-world placeholder agent
  providers/anthropic/   # Claude LLM provider (direct HTTP, no SDK; supports api_key config or api_key_env env var)
  providers/openai/      # OpenAI LLM provider (direct HTTP, no SDK; supports api_key config, api_key_env env var, base_url override)
  providers/fallback/    # Automatic provider failover coordinator (config-driven fallback chains in core.models)
  providers/fanout/      # Parallel multi-provider dispatch (config-driven fanout roles in core.models)
  tools/shell/           # Sandboxed shell execution
  tools/fileio/          # File read/write with base dir restriction
  io/tui/                # Terminal UI
  io/browser/            # Browser IO (HTTP/WS transport for the Nexus web UI)
  io/test/               # Non-interactive test IO (scripted inputs, event collection, auto-approvals)
  io/wails/              # Wails-native transport for desktop shells (config-driven event bridging)
  memory/conversation/   # Conversation history persistence
  memory/longterm/       # Cross-session long-term memory (file-per-entry, YAML frontmatter + markdown)
  observe/logger/        # Structured event logging
  observe/otel/          # OpenTelemetry trace export via OTLP
  observe/thinking/      # Thinking step persistence (JSONL)
  planners/dynamic/      # LLM-generated execution plans
  planners/static/       # Config-defined fixed execution plans
  skills/                # Skill discovery and catalog
  gates/endless_loop/    # Iteration limit (replaces agent max_iterations)
  gates/stop_words/      # Banned word checking (input + output)
  gates/token_budget/    # Session token ceiling
  gates/rate_limiter/    # LLM request rate throttling (pause, not reject)
  gates/prompt_injection/ # Input injection pattern detection
  gates/json_schema/     # Output JSON schema validation with LLM retry
  gates/output_length/   # Output length limit with LLM retry
  gates/content_safety/  # PII/secrets/sensitive content detection (block or redact)
  gates/context_window/  # Context size estimation, triggers compaction
  gates/tool_filter/     # Tool allowlist/blocklist filtering
  gates/internal/retry/  # Shared retry-with-LLM helper for gates
```

## Gates

Quality, safety, and operational gates. Standard plugins subscribing to `before:*` vetoable events at high priority (10). Activate per-profile via `plugins.active` list.

### Vetoable Event System

Gates use `EmitVetoable()` which wraps payloads in `VetoablePayload{Original, Veto}`. Handlers inspect `Original` (e.g. `*events.LLMRequest`), set `Veto` to block. Priority ordering: gates at 10, agents at 50 — gates always evaluate first.

Hook points: `before:llm.request` (input-side), `before:io.output` (output-side), `before:tool.invoke`, `before:tool.result`, `before:skill.activate`.

Resume mechanism: Gates that veto `before:llm.request` temporarily (rate limiter, context window) emit `gate.llm.retry` when the condition clears. All agent plugins (react, planexec, orchestrator) subscribe to this event and re-invoke `sendLLMRequest()` if they have an active turn — no user re-submission needed.

### Gate Config Reference

All gates are optional — only active when listed in `plugins.active`.

```yaml
# Iteration limiting (replaces agent max_iterations).
nexus.gate.endless_loop:
  max_iterations: 25    # default 25
  warning_at: 20        # emit warning N iterations before limit (0 = off)

# Banned word detection.
nexus.gate.stop_words:
  words: ["forbidden"]  # inline word list
  word_files: [/path/to/banned.txt]  # one word per line, # comments
  case_sensitive: false  # default false
  message: "Content blocked: contains prohibited terms."

# Session token ceiling.
nexus.gate.token_budget:
  max_tokens: 100000    # session total
  warning_threshold: 0.8  # warn at 80%
  message: "Token budget exhausted for this session."

# Request rate throttling (pauses, does not reject).
nexus.gate.rate_limiter:
  requests_per_minute: 60
  window_seconds: 60
  pause_message: "Rate limit reached. Pausing for {seconds}s..."

# Prompt injection detection.
nexus.gate.prompt_injection:
  action: block          # "block" or "warn"
  patterns: []           # additional regex patterns
  patterns_file: ""      # file with regex patterns, one per line
  message: "Input blocked: potential prompt injection detected."

# JSON schema validation with LLM retry.
nexus.gate.json_schema:
  schema: '{"type":"object","required":["name"]}'  # inline or
  schema_file: /path/to/schema.json
  max_retries: 3
  retry_prompt: "..."    # template with {schema}, {error}

# Output character limit with LLM retry.
nexus.gate.output_length:
  max_chars: 5000
  max_retries: 2
  retry_prompt: "..."    # template with {length}, {limit}

# PII/secrets detection (all checks on by default).
nexus.gate.content_safety:
  action: block          # "block" or "redact"
  check_pii_email: true
  check_pii_phone: true
  check_pii_ssn: true
  check_secrets_api_key: true
  check_secrets_private_key: true
  check_secrets_password: true
  check_credit_card: true
  check_ip_internal: true
  custom_patterns: []    # additional regex patterns
  message: "Content blocked: contains sensitive information ({checks})."

# Context window estimation, triggers compaction.
nexus.gate.context_window:
  max_context_tokens: 100000
  trigger_ratio: 0.85    # trigger at 85% of max
  chars_per_token: 4.0

# Tool filtering (allowlist or blocklist).
nexus.gate.tool_filter:
  include: [file_read, file_write]  # only these tools (empty = all)
  # or
  exclude: [shell]                  # remove these tools
```

## Tool Choice

Controls which tools the LLM is allowed or required to use per request. Three layers compose:

### LLMRequest fields

`ToolChoice *events.ToolChoice` — mode (`auto`|`required`|`none`|`tool`) + optional tool name.
`ToolFilter *events.ToolFilter` — include/exclude tool lists. Include takes precedence.

### Provider mapping

Providers map `ToolChoice` to native API format:

| Mode | Anthropic | OpenAI |
|------|-----------|--------|
| `auto` | `{"type": "auto"}` | `"auto"` |
| `required` | `{"type": "any"}` | `"required"` |
| `none` | strips tools | `"none"` |
| `tool` | `{"type": "tool", "name": "X"}` | `{"type": "function", "function": {"name": "X"}}` |

For providers without native support, simulation: `none` strips tools, `required`/`tool` use system prompt injection + tool restriction.

### Agent config

ReAct agent supports static default, shorthand, and per-iteration sequences:

```yaml
nexus.agent.react:
  tool_choice: required             # shorthand
  tool_choice:
    mode: auto                      # static default
  tool_choice:
    sequence:                       # per-iteration pattern
      - mode: required              # iteration 1
      - mode: tool                  # iteration 2
        name: shell
      - mode: auto                  # iteration 3+ (last entry repeats)
```

### Dynamic override via events

Any plugin can emit `agent.tool_choice` with `AgentToolChoice{Mode, ToolName, Duration}`:
- `Duration: "once"` — next request only, reverts to config default.
- `Duration: "sticky"` — persists until replaced. Reset on new turn.

### Evaluation order

1. All registered tools → 2. `nexus.gate.tool_filter` (config include/exclude) → 3. `before:llm.request` gate modifications → 4. Resolve tool choice (event override > config sequence > config default) → 5. Validate (named tool filtered out → fall back to required) → 6. Provider maps to native or simulates.

## Structured Output

Optional structured output enforcement for LLM responses. Three-layer design: schema declaration → request tagging → provider execution.

### ResponseFormat on LLMRequest

`ResponseFormat *events.ResponseFormat` — optional field on `LLMRequest`:
- `Type`: `"text"` | `"json_object"` | `"json_schema"`
- `Name`: schema name (OpenAI requires this)
- `Schema`: `map[string]any` JSON Schema
- `Strict`: enforce strict schema adherence

### Schema Registry (`pkg/engine/schema.go`)

Engine-level registry (like ModelRegistry, PromptRegistry). Passed to plugins via `PluginContext.Schemas`. Subscribes to bus events:
- `schema.register` / `schema.deregister` — plugins register/remove named schemas
- `before:llm.request` (priority 5) — attaches `ResponseFormat` when request has `Metadata["_expects_schema"]` tag

### Provider Behavior

| Provider | Native Support | Strategy |
|----------|---------------|----------|
| **OpenAI** | Yes | Maps to `response_format` API field |
| **Anthropic** | No | Simulates via tool-use-as-schema: injects synthetic `_structured_output` tool, forces tool choice, unwraps tool args back into `Content` |

Both providers set `LLMResponse.Metadata["_structured_output"] = true` when enforcement was used.

### Skill Integration

Skills declare `output_schema` (inline) or `output_schema_file` (path relative to skill dir) in SKILL.md frontmatter. On activation, skills plugin emits `schema.register` with name `skill.<name>.output`. During active skill, tags `before:llm.request` with `_expects_schema`. On deactivation, emits `schema.deregister`.

### json_schema Gate Interaction

Gate tracks `_structured_output` from `llm.response` metadata. Skips validation when provider enforced natively. Validates+retries as usual when not.

## Code Conventions

- **Logging**: Use `slog` (structured) everywhere. Plugins get logger via `PluginContext`.
- **Error wrapping**: Use `fmt.Errorf("context: %w", err)` for error chains.
- **Plugin IDs**: Dotted namespace — `nexus.<category>.<name>` (e.g. `nexus.tool.shell`).
- **Event types**: Dotted namespace — `core.boot`, `llm.request`, `tool.result`, etc.
- **Config**: YAML, loaded at startup. Plugin config passed as `map[string]any` during init.
- **No direct plugin-to-plugin calls**: All comms via event bus.
- **Dependencies**: Minimal — only `gopkg.in/yaml.v3` beyond stdlib. Anthropic API called via raw HTTP. JSON schema gate uses `github.com/santhosh-tekuri/jsonschema/v6`. Desktop shell adds `github.com/wailsapp/wails/v2`, `github.com/zalando/go-keyring`, `github.com/fsnotify/fsnotify`.

## IO Transport Plugins: `nexus.io.browser` ↔ `nexus.io.wails`

Two sibling IO transport plugins project engine event bus onto UI. Share Nexus web UI code via `pkg/ui` adapter contract, but differ deliberately in scope and lifetime.

**Scope and lifetime.**

- `nexus.io.browser` = **session-scoped**. One browser session, one engine session, then plugin shuts down. No multi-session mgmt, no session switching, no recall UI in plugin. CLI `main` (or user closing tab + emitting `io.session.end`) owns "what session am I in."
- `nexus.io.wails` = **process-scoped**, acts as desktop shell wrapper. Can start new session, recall old sessions, switch between them, surface OS-native file dialogs, menus, notifications, drag-and-drop. Owns long-lived webview process + session lifecycle *within* that process.

**Parity rule (enforced — do not violate).**

When extending either plugin, every change classified into one of two buckets:

1. **In-session UX feature** — something user does *inside* active session: rendering new event type, showing status indicator, surfacing new approval flow, adding keyboard shortcut, improving streaming display, etc. **Belongs in both plugins.** Add to `nexus.io.browser`, must also port to `nexus.io.wails`, and vice versa. Should live in shared code under `pkg/ui/` when practical so port is mechanical not rewrite.

2. **Shell/wrapper feature** — only makes sense at desktop-app or multi-session boundary: session list UI, recall-from-history, OS file dialogs, native menus, system tray, window mgmt, notifications, drag-and-drop, auto-update UI, app-level settings beyond `ANTHROPIC_API_KEY`. **Belongs only in `nexus.io.wails`.** Must not back-port to `nexus.io.browser` — `browser` intentionally thin session-scoped transport. Adding wrapper features makes it second desktop shell by accident and destroys simplicity that makes it useful as dev-mode + headless sibling.

**When in doubt, ask:** "Would this feature make sense if user already picked session and plugin's only job was rendering that one session's events?" If yes, in-session, goes in both. If feature implies user choosing *between* sessions, talking to OS, or living past single session's lifetime, wrapper feature, only `nexus.io.wails`.

**Shared code vs forked code.** Anything generic (event serialization, message envelope format, `UIAdapter` interface in `pkg/ui/adapter.go`, UI-side rendering logic) lives in shared packages so both plugins consume. Only transport layer differs: HTTP/WS server in `browser`, Wails runtime bindings in `wails`. If duplicating logic across both plugins that isn't transport-specific, stop and factor into `pkg/ui/` first.

### Config-driven event bridging (`nexus.io.wails`)

Wails IO plugin runs in two modes:

- **Legacy mode** (no config keys): hardcoded chat-event subs with typed handlers.
- **Config-driven mode** (`subscribe`/`accept` lists in YAML): generic passthrough bridging for arbitrary domain events. Developer controls exactly which events cross bus↔frontend boundary.

```yaml
plugins:
  nexus.io.wails:
    subscribe:              # bus → frontend
      - "match.result"
      - "ui.state.restore"
    accept:                 # frontend → bus
      - "match.request"
      - "ui.state.save"
```

Config-driven mode required for desktop shell example agents (hello-world, staffing-match). All domain comms flow through bus bridge, not Wails-bound methods.

### Multi-agent scoping

When desktop shell hosts multiple agents, each gets scoped `Runtime` adapter that namespaces event channels by agent ID:

- Outbound: `"{agentID}:nexus"` instead of `"nexus"`
- Inbound: `"{agentID}:nexus.input"` instead of `"nexus.input"`

Wails IO plugin itself unaware of scoping — talks to its `Runtime`, scoped wrapper handles namespace. No plugin changes needed for multi-agent.

## Desktop Shell Framework (`pkg/desktop/`)

Desktop shell framework provides everything to embed one or more Nexus agents in Wails desktop app. Framework parts:

- **`shell.go`** — Core orchestrator. Manages per-agent engine lifecycles, Wails app setup, session mgmt, file portal, all Wails-bound methods.
- **`settings.go`** — Settings schema types (`SettingsField`, `FieldType`, `SettingsSchema`).
- **`store.go`** — Persistent settings store. Plaintext JSON at `~/.nexus/desktop/settings.json`, secrets in OS keychain via `go-keyring`.
- **`resolve.go`** — `${var}` placeholder resolution in config YAML from settings store with scope fallback (agent → shell).
- **`sessions.go`** — Session metadata index (`SessionMeta`), persists to `~/.nexus/desktop/sessions.json`, cleanup + reconciliation.
- **`runtime.go`** — Scoped `Runtime` adapter for multi-agent event isolation. Enriches file dialog `DefaultDirectory` from settings.
- **`watcher.go`** — Filesystem watcher (`fsnotify`) for file browser panel. Watches one dir at a time with debounced change notifications.

### Usage

```go
desktop.Run(&desktop.Shell{
    Title:  "My App",
    Width:  900,
    Height: 720,
    Assets: assets,
    Agents: []desktop.Agent{{
        ID:         "my-agent",
        Name:       "My Agent",
        ConfigYAML: configYAML,
        Factories: map[string]func() engine.Plugin{
            "nexus.io.wails":   wailsio.New,
            "my.custom.plugin": myplugin.New,
        },
        Settings: []desktop.SettingsField{
            {Key: "shell.api_key", Display: "API Key", Type: desktop.FieldString, Secret: true, Required: true},
            {Key: "input_dir", Display: "Input Folder", Type: desktop.FieldPath, Required: true},
            {Key: "output_dir", Display: "Output Folder", Type: desktop.FieldPath},
        },
    }},
})
```

### Wails-bound shell methods

Only Wails-bound methods — all domain comms flow through bus.

**Agent lifecycle:**
- `ListAgents()` — Returns all agents with current status (idle/booting/running/error)
- `EnsureAgentRunning(agentID)` — Lazy boot on first selection
- `StopAgent(agentID)` — Stop engine, mark idle

**Session management:**
- `NewSession(agentID)` — Stop current engine, boot fresh
- `RecallSession(agentID, sessionID)` — Stop current, boot with `RecallSessionID` to replay history
- `ListSessions(agentID)` — Sorted session metadata for agent
- `DeleteSession(agentID, sessionID)` — Remove from index + delete engine session dir

**OS integration:**
- `PickFile(agentID, title, filter)` — Native file open dialog, rooted in agent's `input_dir`
- `PickFolder(agentID, title)` — Native folder selection
- `OpenExternal(url)` — Open URL in system browser
- `RevealInFinder(path)` — Open file manager at path
- `Notify(title, body)` — OS notification (placeholder)

**File portal:**
- `ListFiles(agentID, filter)` — List contents of agent's `input_dir` (non-recursive, glob filter)
- `OutputDir(agentID)` — Resolve agent's `output_dir`, creating if needed
- `CopyFileToInputDir(agentID, sourcePath)` — Copy file into agent's `input_dir` (for drag-and-drop)
- `WatchInputDir(agentID)` — Start fsnotify watcher on agent's `input_dir`

**Settings:**
- `GetSettingsSchema()` — Full schema for frontend rendering (shell + per-agent)
- `GetSettings()` — All current values (secrets masked as `"__keychain__"`)
- `UpdateSetting(scope, key, value)` — Write plaintext setting
- `UpdateSecret(scope, key, value)` — Write to OS keychain
- `DeleteSetting(scope, key, secret)` — Remove setting or secret
- `HasMissingRequired()` — Map of agentID → missing required keys

### Settings system

- **Agent-contributed schemas**: Each `Agent` declares `Settings []SettingsField`. Shell renders settings UI dynamically from these schemas with type-appropriate controls (text input, file picker, toggle, etc.).
- **Persistence**: Plaintext settings in `~/.nexus/desktop/settings.json`, secrets in OS keychain via `zalando/go-keyring` (service name `nexus-desktop`).
- **Config injection**: Agent `ConfigYAML` uses `${var}` placeholders. Shell resolves from settings store before calling `engine.NewFromBytes`. Scope fallback: agent scope first, then shell.
- **Shell-scoped secrets**: Keys prefixed `shell.` (e.g. `shell.anthropic_api_key`) shared across agents — entered once in General settings.
- **Required field gating**: Agents with missing required settings cannot boot; shell redirects to settings page with missing fields highlighted.

### Session management

Shell tracks session history per agent in `~/.nexus/desktop/sessions.json`.

**Session metadata events** (bus contract — agent plugins emit, shell subscribes):
- `session.meta.title` — Human-readable session title (e.g. "Hello, World!")
- `session.meta.preview` — Agent-specific summary data for session list (e.g. `{ candidateCount: 5, topCandidate: "Jane" }`)
- `session.meta.status` — Explicit status change (e.g. `{ status: "completed" }`)

**Session lifecycle:**
- **New session**: `Shell.NewSession(agentID)` stops current engine + boots fresh one.
- **Recall**: `Shell.RecallSession(agentID, sessionID)` stops current engine, boots with `RecallSessionID` set. Engine replays conversation history via `io.history.replay`.
- **Delete**: `Shell.DeleteSession(agentID, sessionID)` removes both index entry + engine session dir from disk.
- **Cleanup**: On startup, shell removes sessions older than configured retention period (default 30 days) + reconciles orphaned engine dirs.

**Frontend notifications**: Session list updates arrive via `{agentID}:sessions.updated` Wails events emitted direct by shell (not through wails IO plugin bridge).

### UI state persistence

Framework-agnostic mechanism for frontends to persist + restore UI state across sessions:

- **`ui.state.save`** (inbound: frontend → bus) — Frontend emits with opaque `{ state: { ... } }` payload after meaningful actions. Shell writes to `ui-state.json` in engine session dir (`~/.nexus/sessions/<id>/ui-state.json`).
- **`ui.state.restore`** (outbound: bus → frontend) — On session recall, shell reads `ui-state.json` and emits so frontend can rehydrate.

Both events must be in wails IO plugin's `accept`/`subscribe` config. Payload structure entirely up to frontend — shell treats as opaque JSON blob. Works with any frontend framework.

### File portal

Shell provides standardized file access layer for agents. Each agent declares `input_dir` and optionally `output_dir` settings. Shell uses these to:

- **Root file dialogs**: `PickFile` + `scopedRuntime.OpenFileDialog` enrich `DefaultDirectory` from agent's `input_dir`, falling back to `shell.shared_data_dir`, then `~/Documents`.
- **List files**: `ListFiles(agentID, filter)` returns agent's `input_dir` contents for file browser panel.
- **Resolve output directory**: `OutputDir(agentID)` returns (and creates) agent's `output_dir`. Also via bus events: `io.file.output_dir.request` → `io.file.output_dir.response`.
- **File browser panel**: Right-side collapsible panel showing active agent's dir contents. Supports click-to-select (emits `io.file.selected`), open in default app, reveal in finder, drag-and-drop import.
- **Filesystem watching**: `fsnotify` watcher notifies frontend via `{agentID}:files.changed` Wails events when files added/removed in watched dir. Debounced to 200ms.
- **Drag-and-drop**: File panel accepts OS file drops. Uses HTML5 drag-and-drop with Wails webview file path access (optimistic), with FileReader fallback path.
- **Shared data directory**: Shell-scoped `shared_data_dir` setting accessible to all agents as fallback when `input_dir` not configured.

**Bus events for file portal:**

| Event | Direction | Purpose |
|-------|-----------|---------|
| `io.file.open.request` | Plugin → shell | Request a file dialog (existing) |
| `io.file.open.response` | Shell → plugin | File dialog result (existing) |
| `io.file.output_dir.request` | Plugin → shell | Ask where to write outputs |
| `io.file.output_dir.response` | Shell → plugin | Output directory path |
| `io.file.selected` | Shell → plugin | User selected a file in the browser panel |
| `session.file.created` | Plugin → shell | Agent wrote an output file (existing) |

## Desktop App (`cmd/desktop/`)

Reference multi-agent desktop app with two agents:

1. **staffing-match** — AI candidate ranking. Uses Anthropic LLM provider to score candidates against job descriptions. Domain plugin at `cmd/desktop/internal/matcher/`. Emits `session.meta.title`, `session.meta.preview`, `session.meta.status` after completing match. Writes match result JSON to `output_dir`. Saves UI state (job text, candidates, cost) for session recall. File dialogs rooted in `input_dir`.
2. **hello-world** — Minimal bus bridge PoC. Plugin at `plugins/apps/helloworld/`. Responds to `hello.request` with configurable greeting, emits `session.meta.title`. Saves UI state (name, response) for session recall.

**Frontend**: Single `index.html` AlpineJS SPA with:
- Left-nav sidebar with agent icons, active indicator, collapsible to icon-only
- Session list panel (per-agent, left side) with new/recall/delete actions
- File browser panel (right side, collapsible) with file list, click-to-select, open/reveal actions, drag-and-drop import, fsnotify-driven auto-refresh
- Settings page with dynamic schema rendering, keychain secret support
- Per-agent UI sections (hello-world form, staffing-match file picker + results table)
- Shared `createBus(agentID)` helper for scoped event bridging

**Agent configs** = `//go:embed`'d YAML files (`config-hello.yaml`, `config-staffing.yaml`) loaded via `engine.NewFromBytes`. No filesystem dep at runtime.

## Session Data

Sessions persist to `~/.nexus/sessions/<id>/` with:
- `metadata/session.json` — Engine session metadata (timestamps, status, plugins, token counts)
- `metadata/config-snapshot.yaml` — Config used for session
- `context/` — Conversation context files
- `files/` — Session file workspace
- `plugins/<pluginID>/` — Per-plugin data dirs
- `ui-state.json` — Frontend UI state snapshot (written by shell on `ui.state.save` events)

## Planning System

Optional planning phase runs before ReAct agent iterates. Enabled via `planning: true` in ReAct agent config. Two planner types:

- **Dynamic** (`nexus.planner.dynamic`) — LLM generates plan. Tags its `llm.request` with `Metadata["_source"]` so ReAct agent ignores response.
- **Static** (`nexus.planner.static`) — Fixed steps from config. No LLM call.

Approval modes: `always` (user must approve), `never` (skip), `auto` (LLM decides — dynamic only, static defaults to never).

Plans persist to session: `plugins/{planner_id}/{planID}/plan.json`, `request.json`, `approval.json`. Thinking steps persist via observer to `plugins/nexus.observe.thinking/thinking.jsonl`.

Event flow: `plan.request` → planner generates → `plan.created` (UI display) → optional `plan.approval.request`/`plan.approval.response` → `plan.result` → ReAct injects plan into system prompt + iterates.

## Configuration Profiles

Five built-in profiles in `configs/`:
- `default.yaml` — General purpose, limited shell commands
- `coding.yaml` — Extended shell access (make, docker, npm, cargo, python)
- `research.yaml` — No shell access, larger context window, more iterations
- `planned.yaml` — Dynamic planner with auto-approval
- `planned-static.yaml` — Static planner with fixed coding workflow steps

## Skills

Discovered from `./skills/` and `~/.agents/skills/`. Each skill = dir with `SKILL.md` file containing YAML frontmatter + markdown instructions. System prompts live in `prompts/`.