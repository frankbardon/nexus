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
- **Integration tests** (`tests/integration/`) — Go tests behind `//go:build integration` tag. Two modes:
  - **Mock mode** (`mock_responses` set): No LLM calls, no API key, sub-second.
  - **Live mode** (no `mock_responses`): Real LLM calls via provider. Requires `ANTHROPIC_API_KEY`.

**All Claude updates must update relevant docs in `docs/`.**
**Core system updates should be genericized and treated as reusable, single-use plugins shouldn't in `plugins` folder**

### Plugin Interface

Every plugin implements `engine.Plugin` (`pkg/engine/plugin.go`):
- `ID() string` — Dotted ID (e.g. `nexus.tool.shell`)
- `Dependencies() []string` — IDs this plugin needs **already in the active set** for ordering; does NOT activate anything.
- `Requires() []Requirement` — IDs this plugin needs to **activate** if absent; engine appends them to the active set at boot.
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
  providers/gemini/      # Google Gemini LLM provider (direct HTTP, no SDK; api-key + Vertex AI auth, thinking, code execution, multimodal, prompt caching)
  providers/fallback/    # Automatic provider failover coordinator (config-driven fallback chains in core.models)
  providers/fanout/      # Parallel multi-provider dispatch (config-driven fanout roles in core.models)
  tools/shell/           # Sandboxed shell execution (supports working_dir, allowed_commands, timeout, sandbox config)
  tools/fileio/          # File read/write with base dir restriction
  tools/catalog/         # Shared tool registry; agents query via "tool.catalog.query"
  tools/web/             # web_search + web_fetch tools; search routed via search.provider capability, fetch via go-readability
  tools/pulse/           # Pulse skill sync; fetches skills from `pulse skills list/show` CLI at boot, writes SKILL.md files for nexus.skills discovery. Actual execution via shell plugin
  tools/knowledge_search/ # LLM-facing "knowledge_search" tool; queries configured namespaces via vector.store + embeddings.provider, returns top-k with source paths for citation
  search/brave/          # search.provider adapter: Brave Search REST API
  search/anthropic_native/ # search.provider adapter: Anthropic's server-side web_search tool (direct HTTP)
  search/openai_native/  # search.provider adapter: OpenAI's server-side web_search via Responses API
  search/gemini_native/  # search.provider adapter: Gemini's google_search grounding tool
  io/tui/                # Terminal UI
  io/browser/            # Browser IO (HTTP/WS transport for the Nexus web UI)
  io/test/               # Non-interactive test IO (scripted inputs, event collection, auto-approvals)
  io/wails/              # Wails-native transport for desktop shells (config-driven event bridging)
  memory/simple/         # Unbounded append-only history; reference/test impl for memory.history
  memory/capped/         # Default memory.history provider: sliding window, JSONL persistence, pair-safe truncation
  memory/summary_buffer/ # Inline auto-compacting history; keeps recent N verbatim, LLM-summarizes older (memory.history + memory.compaction)
  memory/compaction/     # External compaction coordinator; summarizes, emits memory.compacted for history buffers to adopt
  memory/longterm/       # Cross-session structured notes (file-per-entry, YAML frontmatter + markdown). Key-addressed, LLM-managed via memory_read/write/list/delete tools
  memory/vector/         # Cross-session semantic recall (memory.vector capability). Embedding-addressed via vector.store; auto-stores compaction summaries, retrieves on io.input
  embeddings/openai/     # embeddings.provider adapter: OpenAI embeddings API (text-embedding-3-*)
  vectorstore/chromem/   # vector.store adapter: philippgille/chromem-go, pure Go, JSON on-disk persistence; namespaces map to collections
  rag/ingest/            # RAG file ingestion: recursive-character chunker + embedding cache + fsnotify watcher + rag.ingest event handler; backs the "nexus ingest" CLI subcommand
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

## Configuration Profiles

Seven built-in profiles in `configs/`:
- `default.yaml` — General purpose, limited shell commands
- `coding.yaml` — Extended shell access (make, docker, npm, cargo, python)
- `research.yaml` — No shell access, larger context window, more iterations
- `planned.yaml` — Dynamic planner with auto-approval
- `planned-static.yaml` — Static planner with fixed coding workflow steps
- `rag.yaml` — RAG primitives + knowledge_search tool + vector memory, wired to Anthropic LLM + OpenAI embeddings + chromem-go vector store
- `pulse.yaml` — Pulse data processing tools with auto-predict

## Skills

Discovered exclusively from directories listed in the `nexus.skills` plugin's `scan_paths` config — there are no implicit defaults. If `scan_paths` is empty, no skills are loaded. Each skill = dir with `SKILL.md` file containing YAML frontmatter + markdown instructions. System prompts live in `prompts/`.

## Code Conventions

- **Logging**: Use `slog` (structured) everywhere. Plugins get logger via `PluginContext`.
- **Error wrapping**: Use `fmt.Errorf("context: %w", err)` for error chains.
- **Plugin IDs**: Dotted namespace — `nexus.<category>.<name>` (e.g. `nexus.tool.shell`).
- **Event types**: Dotted namespace — `core.boot`, `llm.request`, `tool.result`, etc.
- **Config**: YAML, loaded at startup. Plugin config passed as `map[string]any` during init.
- **No direct plugin-to-plugin calls**: All comms via event bus.
- **Dependencies**: Minimal — only `gopkg.in/yaml.v3` beyond stdlib. Anthropic API called via raw HTTP. JSON schema gate uses `github.com/santhosh-tekuri/jsonschema/v6`. Vector store uses `github.com/philippgille/chromem-go` (pure Go, no CGO). Desktop shell adds `github.com/wailsapp/wails/v2`, `github.com/zalando/go-keyring`, `github.com/fsnotify/fsnotify`.
- **Prompt construction**: All content injected into LLM prompts must use XML tag boundaries to separate structural sections. Use semantic tags (`<execution_plan>`, `<current_task>`, `<prior_results>`, `<user_request>`, `<skill_context>`, etc.) not markdown headers or bare concatenation. See `plugins/skills/catalog.go` for reference pattern. Shared XML helpers live in `pkg/engine/`.
- **Path expansion**: Every config-supplied filesystem path must be funneled through `engine.ExpandPath` (`pkg/engine/paths.go`) at the read site so users can write `~` or `~/...` anywhere a path is accepted. There is exactly one helper — do not add new local `expandHome` copies.

## Deep Reference

Detailed docs for specific subsystems live in `.claude/docs/`. Load these only when working on the relevant area:

- **[Plugin System](.claude/docs/plugin-system.md)** — Embedder API, auto-activation (`Requires()`), capabilities system, resolution order
- **[Gates](.claude/docs/gates.md)** — Vetoable event system, gate config reference (all gate YAML options)
- **[Tool System](.claude/docs/tool-system.md)** — Tool choice (provider mapping, agent config, dynamic override), parallel tool dispatch, structured output, schema registry
- **[RAG](.claude/docs/rag.md)** — Embeddings/vector primitives, ingestion, knowledge search, vector memory, CLI ingest
- **[IO Transport](.claude/docs/io-transport.md)** — Browser vs Wails plugin scoping, parity rule, config-driven event bridging, multi-agent scoping
- **[Desktop Shell](.claude/docs/desktop-shell.md)** — Shell framework (`pkg/desktop/`), settings system, session management, file portal, desktop app reference
