# Nexus

Modular AI agent harness. Pure event-driven Go. Core manages event lifecycle + plugin registry only — all behavior via composable plugins.

## Quick Reference

```bash
make build        # Build binary to bin/nexus
make run          # Build and run with default config (configs/default.yaml)
make test         # Run all tests
make test-broker-integration  # Broker integration suite (tagged; no API key needed)
make test-objectstore-minio   # modules/objectstore-s3 against MinIO (tagged; starts/stops its own container)
make test-objectstore-fake-gcs # modules/objectstore-gcs against fake-gcs-server (tagged; builds/starts its own emulator, no container)
make fmt          # Format code (gofmt)
make submodules   # List the Go submodules under modules/ that every sweep covers
make vet          # Run go vet
make lint         # Run staticcheck (includes vet)
```

Run specific profile: `bin/nexus -config configs/coding.yaml`

Run engine integration tests: `go test -tags integration ./tests/integration/ -v` (live mode needs `ANTHROPIC_API_KEY`)

`make test` is untagged, so it skips every tagged suite. CI runs `make test`, then `make test-broker-integration` as its own step, and `make test-objectstore-minio` and `make test-objectstore-fake-gcs` as their own jobs; the engine suite under `tests/integration/` stays out of CI because live mode requires an API key. The two emulator targets run each object-store backend's conformance suite, and the shared kill-and-resume cycle from `pkg/engine/objectstore/enginetest`, against a real store on loopback — MinIO in a container `scripts/with-minio.sh` starts and stops, fake-gcs-server as a pinned Go binary `scripts/with-fake-gcs.sh` builds and starts, so the GCS one needs no container runtime. Neither needs a cloud account or a repo secret, and both fail rather than skip whenever the emulator was provisioned, so a green job means the tests actually ran. Neither covers IAM: no emulator reproduces IRSA, Workload Identity, ADC resolution or Workload Identity Federation.

Needs an LLM provider API key in env or `.env` file (e.g. `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`).

## Architecture

All comms via central typed event bus — plugins never call each other direct.

- **Engine** (`pkg/engine/`) — Event bus, plugin registry, lifecycle, session workspace, config loading, per-plugin SQLite storage (`pkg/engine/storage/`). Only "core" code.
- **Events** (`pkg/events/`) — Typed event payload structs by domain: `core.go`, `llm.go`, `agent.go`, `tool.go`, `io.go`, `memory.go`, `skill.go`, `session.go`, `schema.go`.
- **Plugins** (`plugins/`) — All behavior lives here. Each implements `engine.Plugin`.
- **Desktop shell** (`pkg/desktop/`) — Reusable framework to embed Nexus in Wails desktop app. Manages per-agent engine lifecycles, settings, sessions, shell services.
- **Desktop app** (`cmd/desktop/`) — Reference multi-agent desktop app hosting hello-world + staffing-match agents.
- **CLI entry point** (`cmd/nexus/main.go`) — Creates engine, registers plugins, runs with signal handling.
- **Session broker** (`cmd/nexus-broker/`) — Standalone HTTP/WS gateway service (NOT a plugin) that fronts OS-isolated `nexus` instances behind one ingress: clients `claim` a lease, the broker cold-spawns a `nexus` subprocess that dials back via the `nexus.io.broker` plugin, and instances `release` on demand/idle/crash with sessions persisted on disk. It is **protocol-aware for A2A** rather than a purely opaque pipe: each `agents:` profile publishes an Agent Card and both A2A bindings under `/agents/<name>/`, and the broker parses and translates the instance IO envelope itself — a message on an unknown `contextId` cold-spawns an instance, a known one routes to it, a released one re-spawns with `-recall`, concurrent tasks on one conversation are queued, and tasks stay readable from a durable store across release and restart. It is the second of the two A2A mappings judged by `pkg/a2a/a2aconform` (5 of 9 vectors; 4 skipped because the IO envelope carries no tool results). Everything outside the `agents:` namespace is still forwarded unparsed. `SIGHUP` re-reads the config file and atomically swaps the reloadable half (`binaries:`, `agents:` and the behavioural bounds) with live leases untouched; `auth:`, `listen_addr`, `advertise_addr`, `state_dir` and `broker_id` are boot-only and a change to them is reported and ignored. See `docs/src/guides/session-broker.md` and `docs/src/guides/a2a.md`.
- **Identity layer** (`pkg/nexusauth/`) — Shared credential verification: `Principal`, a `Validator` interface with `static`/`jwks`/`introspect`/`proxy_headers` implementations, and an ordered first-success `Chain` built from an `auth:` config block. Used by `cmd/nexus-broker`, `nexus.io.agui` and `nexus.io.a2a`; never issues credentials, only verifies them.
- **Plugin registry** (`pkg/engine/allplugins/`) — Shared `RegisterAll()` function used by both `cmd/nexus` and `pkg/testharness`. Single source of truth for plugin registration.
- **Go submodules** (`modules/`) — Code that must not add dependencies to the root module lives in its own
  `go.mod` under `modules/<name>/` (module path `github.com/frankbardon/nexus/modules/<name>`). Cloud
  object-store backends are the motivating case: the root module's direct dependency list is defended, so an
  AWS/GCP SDK goes in a submodule an embedder blank-imports. Two ship today — `modules/objectstore-s3` and
  `modules/objectstore-gcs` — plus `modules/objectstore-seamcheck`, the canary that keeps the seam usable
  from outside the root module. **No `go.work`** — it is gitignored, because a
  workspace merges build lists and would let submodule SDKs move the root module's transitive versions; each
  submodule carries `replace github.com/frankbardon/nexus => ../..` instead. `make build`, `test`, `test-race`,
  `fmt`, `vet` and `lint` all sweep `modules/` (a separate module is invisible to `./...`), and
  `make check-modules` fails a `go.mod` found anywhere else. Submodule tags are `modules/<name>/vX.Y.Z`, cut on
  demand and versioned independently of the core `vX.Y.Z`. See `docs/src/guides/go-modules.md`.
- **Test harness** (`pkg/testharness/`) — Integration test framework. Boots real engine with `nexus.io.test` plugin, provides two-tier assertions (deterministic + semantic LLM judge).
- **Contract harness** (`pkg/testharness/contract/`) — Unit-level harness for one plugin in isolation against a real `engine.Bus`. Asserts declared `Subscriptions()`/`Emissions()` match runtime behavior. Lives in a sub-package to avoid the `plugin → harness → allplugins → plugin` import cycle. See `docs/src/guides/plugin-contracts.md`.
- **Object-store contract suite** (`pkg/engine/objectstore/objectstoretest/`) — exported conformance suite every `objectstore.Backend` must pass (`RunSuite`), plus `NewMemory`, the in-memory backend that passes it and doubles as the substituted seam for untagged unit tests. Out-of-tree backend modules run the same suite. See `docs/src/architecture/sessions.md`.
- **Integration tests** (`tests/integration/`) — Go tests behind `//go:build integration` tag. Two modes:
  - **Mock mode** (`mock_responses` set): No LLM calls, no API key, sub-second.
  - **Live mode** (no `mock_responses`): Real LLM calls via provider. Requires `ANTHROPIC_API_KEY`.
- **Eval harness** (`pkg/eval/`) — Golden-trace runner, assertion engine, baseline differ, failure-promotion, Inspect-mode JSON protocol. CLI: `nexus eval ...`. Docs: `docs/src/eval/`.

**All Claude updates must update relevant docs in `docs/`.**
**Core system updates should be genericized and treated as reusable, single-use plugins shouldn't in `plugins` folder**

### Configuration Reference is the source of truth

`docs/src/configuration/reference.md` is the **authoritative**, single-page list of every YAML key the engine and plugins accept. Any commit that **adds, removes, renames, or changes the default/type of** a configuration key — at the engine level or in any plugin — **must update `docs/src/configuration/reference.md` in the same commit**. Per-plugin pages under `docs/src/plugins/**` may add narrative; the reference page is canonical when they disagree. Treat this rule as binding for every code change touching `Init()` config parsing, struct tags, or option parsers.

### Plugin Interface

Every plugin implements `engine.Plugin` (`pkg/engine/plugin.go`):
- `ID() string` — Dotted ID (e.g. `nexus.tool.shell`)
- `Dependencies() []string` — IDs this plugin needs **already in the active set** for ordering; does NOT activate anything.
- `Requires() []Requirement` — IDs this plugin needs to **activate** if absent; engine appends them to the active set at boot.
- `Init(ctx PluginContext) error` — Gets config, bus, logger, data dir, session, storage opener (`ctx.Storage(scope)` for app/agent/session-scoped SQLite)
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
  agents/planexec/       # Plan-then-execute agent
  agents/subagent/       # Multi-instance subagent (spawn_* tool per instance)
  agents/orchestrator/   # Decompose → parallel workers → synthesis pipeline
  agents/postures/       # AgentPosture registry: loads YAML from scan_dirs, fsnotify hot reload, advertises posture.registry capability
  agents/delegate/       # Sub-agent invocation primitive: 'delegate' tool, posture-driven budgets+depth+cache
  agents/aguiremote/     # Remote AG-UI agents as delegate targets: registers a delegate_agui_<name> tool per configured endpoint, runs it via pkg/agui/aguiclient, maps the SSE run onto the bus
  agents/a2aremote/      # Remote A2A agents as delegate targets: registers a delegate_a2a_<name> tool per configured remote, lazily resolves each Agent Card on first use, folds remote artifacts + final text into an XML-tagged tool result, posture-driven budgets, successes-only LRU, per-remote credentials (bearer / OAuth2 client credentials with a single-flight token cache / mTLS) validated at Init; a remote parking at INPUT_REQUIRED raises a local hitl.requested and the human's answer resumes the SAME taskId/contextId (bounded by hitl.input_timeout AND the whole-call budget, capped by hitl.max_rounds; needs stream: false against a remote that holds the SSE stream open across the park, which nexus.io.a2a does); remote progress is republished as io.output + subagent.iteration; cancel.active issues CancelTask to the remote
  scene/                 # Scene store: scene_create/patch/get/list/delete tools, JSONL patch journal under <session>/plugins/nexus.scene/
  apps/helloworld/       # Built-in hello-world placeholder agent
  control/cancel/        # control.cancel capability + /resume slash command
  control/hitl/          # human-in-the-loop registry; owns ask_user tool, emits hitl.requested, routes hitl.responded
  discovery/progressive/ # Hierarchical tool discovery; LLM sees class summaries, drills via "discover" meta-tool
  providers/anthropic/   # Claude LLM provider (direct HTTP, no SDK; supports api_key config or api_key_env env var)
  providers/openai/      # OpenAI LLM provider (direct HTTP, no SDK; supports api_key config, api_key_env env var, base_url override)
  providers/gemini/      # Google Gemini LLM provider (direct HTTP, no SDK; api-key + Vertex AI auth, thinking, code execution, multimodal, prompt caching)
  providers/fallback/    # Automatic provider failover coordinator (config-driven fallback chains in core.models)
  providers/fanout/      # Parallel multi-provider dispatch (config-driven fanout roles in core.models)
  llm/batch/             # Cross-provider batch coordinator (Anthropic Messages Batches + OpenAI Batch API); persisted state, resumable across restarts
  mcp/client/            # MCP (Model Context Protocol) client bridge — connects developer-configured MCP servers (stdio + streamable HTTP) and projects their tools/resources/prompts into the Nexus catalog and slash-command surface
  tools/shell/           # Sandboxed shell execution (supports working_dir, allowed_commands, timeout, sandbox config)
  tools/fileio/          # File read/write with base dir restriction
  tools/catalog/         # Shared tool registry; agents query via "tool.catalog.query"
  tools/web/             # web_search + web_fetch tools; search routed via search.provider capability, fetch via go-readability
  tools/codeexec/        # run_code tool (Go via Yaegi interpreter); parallel constructs, stdlib whitelist
  tools/pdf/             # read_pdf tool via poppler-utils (pdftotext, pdfinfo)
  tools/opener/          # open_path tool (platform-aware: open / xdg-open / start)
  tools/knowledge_search/ # LLM-facing "knowledge_search" tool; queries configured namespaces via vector.store + embeddings.provider, returns top-k with source paths for citation
  search/brave/          # search.provider adapter: Brave Search REST API
  search/anthropic_native/ # search.provider adapter: Anthropic's server-side web_search tool (direct HTTP)
  search/openai_native/  # search.provider adapter: OpenAI's server-side web_search via Responses API
  search/gemini_native/  # search.provider adapter: Gemini's google_search grounding tool
  io/tui/                # Terminal UI
  io/browser/            # Browser IO (HTTP/WS transport for the Nexus web UI)
  io/oneshot/            # Scripting/batch IO; reads stdin/file/inline prompt, writes JSON transcript
  io/test/               # Non-interactive test IO (scripted inputs, event collection, auto-approvals, scripted/disable-able hitl answers via hitl_responses + hitl_auto_respond)
  io/wails/              # Wails-native transport for desktop shells (config-driven event bridging)
  io/broker/             # Dial-back IO transport: instances spawned by cmd/nexus-broker dial OUT to the broker gateway (not a listener)
  io/a2a/                # A2A serve transport: /.well-known/agent-card.json + JSON-RPC and HTTP+JSON bindings; SendMessage/SendStreamingMessage map onto io.input and stream one Task per turn (SUBMITTED->WORKING->COMPLETED, final text, tool results, written files and structured output as Artifacts, bounded by size caps and retention); GetTask/ListTasks/SubscribeToTask read them back from a principal-scoped session SQLite store (foreign task == unknown task); hitl.requested parks a task at INPUT_REQUIRED and a message naming the same taskId resumes the SAME turn; CancelTask settles at CANCELED via the control.cancel capability; task lifetime is detached from the HTTP request; hand-authored agent card, securitySchemes derived from pkg/nexusauth validators; wire format from pkg/a2a; shared conformance corpus in pkg/a2a/a2aconform pins this mapping and the broker's against one set of vectors
  io/agui/               # AG-UI serve transport: POST /agui accepts RunAgentInput, streams canonical AG-UI SSE (RunStarted→text/tool/reasoning→RunFinished); external-facing interop via pkg/agui wire, loopback+bearer+CORS
  memory/simple/         # Unbounded append-only history; reference/test impl for memory.history
  memory/capped/         # Default memory.history provider: sliding window, JSONL persistence, pair-safe truncation
  memory/summary_buffer/ # Inline auto-compacting history; keeps recent N verbatim, LLM-summarizes older (memory.history + memory.compaction)
  memory/compaction/     # External compaction coordinator; summarizes, emits memory.compacted for history buffers to adopt
  memory/longterm/       # Cross-session structured notes (file-per-entry, YAML frontmatter + markdown). Key-addressed, LLM-managed via memory_read/write/list/delete tools
  memory/vector/         # Cross-session semantic recall (memory.vector capability). Embedding-addressed via vector.store; auto-stores compaction summaries, retrieves on io.input
  embeddings/openai/     # embeddings.provider adapter: OpenAI embeddings API (text-embedding-3-*)
  embeddings/mock/       # embeddings.provider adapter: deterministic hash-based vectors; no network, opt-in via plugins.active
  vectorstore/chromem/   # vector.store adapter: philippgille/chromem-go, pure Go, JSON on-disk persistence; namespaces map to collections
  vectorstore/sqlite_fts/ # search.lexical adapter: SQLite FTS5 (modernc.org/sqlite, pure Go); BM25 ranking; namespaces map to FTS5 virtual tables; backed by per-plugin storage capability
  rag/citations/         # rag.citations: parse <cite/> tags or Anthropic native Citations from llm.response, validate against rag.retrieved, emit llm.response.cited
  rag/hybrid/            # search.hybrid orchestrator: parallel vector + lexical retrieval, RRF or weighted fusion, per-query LexicalBias, optional reranker pass
  rag/reranker/cohere/   # search.reranker adapter: Cohere Rerank v2 API
  rag/reranker/jina/     # search.reranker adapter: Jina Reranker API
  rag/reranker/local/    # search.reranker adapter: pure-Go TF-IDF cosine (offline; baseline quality, zero deps)
  rag/ingest/            # RAG file ingestion: recursive-character chunker + embedding cache + fsnotify watcher + rag.ingest event handler; backs the "nexus ingest" CLI subcommand; dual-writes to search.lexical when active
  observe/otel/          # OpenTelemetry trace export via OTLP
  observe/thinking/      # Thinking step persistence (JSONL) — bus-driven, also visible in journal
  observe/sampler/       # Online journal sampler — opt-in, FS-only
  planners/dynamic/      # LLM-generated execution plans
  planners/static/       # Config-defined fixed execution plans
  skills/                # Skill discovery and catalog
  system/dynvars/        # Dynamic prompt variables (date, time, cwd, session_dir, os) — opt-in
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
- `plugins/<pluginID>/` — Per-plugin data dirs (also holds session-scoped `store.db` if the plugin uses `ctx.Storage(ScopeSession)`)
- `ui-state.json` — Frontend UI state snapshot (written by shell on `ui.state.save` events)

App- and agent-scope per-plugin storage live outside the session tree at
`~/.nexus/plugins/<pluginID>/store.db` and
`~/.nexus/agents/<agentID>/plugins/<pluginID>/store.db`. See
`docs/src/architecture/storage.md`.

## Planning System

Optional planning phase runs before ReAct agent iterates. Enabled via `planning: true` in ReAct agent config. Two planner types:

- **Dynamic** (`nexus.planner.dynamic`) — LLM generates plan. Tags its `llm.request` with `Metadata["_source"]` so ReAct agent ignores response.
- **Static** (`nexus.planner.static`) — Fixed steps from config. No LLM call.

Approval modes: `always` (user must approve), `never` (skip), `auto` (LLM decides — dynamic only, static defaults to never).

## Skills

Discovered exclusively from directories listed in the `nexus.skills` plugin's `scan_paths` config — there are no implicit defaults. If `scan_paths` is empty, no skills are loaded. Each skill = dir with `SKILL.md` file containing YAML frontmatter + markdown instructions.

## Code Conventions

- **Logging**: Use `slog` (structured) everywhere. Plugins get logger via `PluginContext`.
- **Error wrapping**: Use `fmt.Errorf("context: %w", err)` for error chains.
- **Plugin IDs**: Dotted namespace — `nexus.<category>.<name>` (e.g. `nexus.tool.shell`).
- **Event types**: Dotted namespace — `core.boot`, `llm.request`, `tool.result`, etc.
- **Config**: YAML, loaded at startup. Plugin config passed as `map[string]any` during init.
- **No direct plugin-to-plugin calls**: All comms via event bus.
- **Dependencies**: Core engine and LLM providers are raw `net/http` — no provider SDKs. Beyond stdlib, direct deps include: `gopkg.in/yaml.v3` (config), `github.com/santhosh-tekuri/jsonschema/v6` (JSON schema gate), `github.com/philippgille/chromem-go` (vector store, pure Go), `modernc.org/sqlite` (per-plugin storage, pure Go, FTS5 included), `github.com/wailsapp/wails/v2` + `github.com/zalando/go-keyring` + `github.com/fsnotify/fsnotify` (desktop shell), OpenTelemetry SDK (observe), `github.com/charmbracelet/bubbletea` (TUI), `github.com/traefik/yaegi` (codeexec), `github.com/tetratelabs/wazero` (wasm sandbox), `github.com/modelcontextprotocol/go-sdk` (official MCP client SDK). See `go.mod` for the canonical list (~28 direct deps as of 2026-06).
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
