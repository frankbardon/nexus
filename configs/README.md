# Nexus Configuration Guide

The `configs/` directory holds two kinds of YAML profiles, distinguished by filename prefix:

- **`test-*.yaml`** — wired into the integration test harness (`pkg/testharness/`). Each
  has a matching test in `tests/integration/`. Most use `nexus.io.test` with mocked LLM
  responses, so they run with no API key. The few that hit real providers gate on
  `ANTHROPIC_API_KEY` (Anthropic-only tests) or `t.Skip` on missing keys.
- **`demo-*.yaml`** — manual exploration profiles. Use `nexus.io.tui` or
  `nexus.io.browser` for interactive use. Run with `bin/nexus -config configs/<name>.yaml`.
  Not exercised by CI; surface for hands-on testing of features that are hard to assert
  in code (visual UI, cross-session persistence, third-party services).

`default.yaml` keeps its name as the bundled default for `make run`.

## Core System Functionalities

| Area | Key Knobs |
|------|-----------|
| Engine | `log_level`, `tick_interval`, `max_concurrent_events` |
| Models | `default` role, named roles (`reasoning`/`balanced`/`quick`), provider routing per role |
| Sessions | `root`, `retention`, `id_format` |
| Plugin lifecycle | `plugins.active` list, dependency ordering, instanced IDs (`plugin/name`) |

## Plugin Inventory (28 plugins)

### Agents

| Plugin | ID | Key Config |
|--------|----|------------|
| ReAct | `nexus.agent.react` | `planning`, `model_role`, `system_prompt`/`system_prompt_file`, `tool_choice` (shorthand, static, or sequence) |
| Orchestrator | `nexus.agent.orchestrator` | `max_workers`, `max_subtasks`, `worker_max_iterations`, `orchestrator_model_role`, `worker_model_role`, `synthesis_model_role`, `fail_fast` |
| PlanExec | `nexus.agent.planexec` | `execution_model_role`, `replan_on_failure`, `approval` (always/never) |
| Subagent | `nexus.agent.subagent` | `model_role`, `tool_name`, `tool_description`, `system_prompt`. Supports instanced IDs: `nexus.agent.subagent/name` |

### Providers

| Plugin | ID | Key Config |
|--------|----|------------|
| Anthropic | `nexus.llm.anthropic` | `api_key`, `api_key_env` (default `ANTHROPIC_API_KEY`), `debug`, `pricing` |
| OpenAI | `nexus.llm.openai` | `api_key`, `api_key_env` (default `OPENAI_API_KEY`), `base_url` (for compatible endpoints), `debug`, `pricing` |

### IO Transports

| Plugin | ID | Key Config |
|--------|----|------------|
| TUI | `nexus.io.tui` | (none) |
| Browser | `nexus.io.browser` | `host` (default `localhost`), `port` (default `8080`), `open_browser` (default `true`) |
| One-shot | `nexus.io.oneshot` | `input`, `pretty`, `read_stdin` |
| Wails | `nexus.io.wails` | `subscribe` (bus→frontend), `accept` (frontend→bus) — config-driven event bridging |

### Tools

| Plugin | ID | Key Config |
|--------|----|------------|
| Shell | `nexus.tool.shell` | `allowed_commands` ([]string), `timeout` (default `30s`), `sandbox` (default `false`) |
| File IO | `nexus.tool.file` | `base_dir`, `allow_external_writes` (default `false`), `tools` map (`read_file`, `write_file`, `check_file_size`, `list_files` — all default `true`) |

### Memory

| Plugin | ID | Key Config |
|--------|----|------------|
| Conversation | `nexus.memory.capped` | `max_messages` (default `100`), `persist` (default `true`) |
| Long-term | `nexus.memory.longterm` | `scope` (`agent`/`global`/`both`), `auto_load` (default `true`), `path`, `agent_id`, `auto_save_instructions` |

### Planners

| Plugin | ID | Key Config |
|--------|----|------------|
| Dynamic | `nexus.planner.dynamic` | `approval` (`auto`/`always`/`never`), `max_steps` (default `10`), `model_role`, `plan_prompt`/`plan_prompt_file` |
| Static | `nexus.planner.static` | `approval` (`always`/`never`), `summary`, `steps` ([]map with `description` + `instructions`) |

### Gates

All gates subscribe to `before:*` vetoable events at priority 10.

| Plugin | ID | Key Config |
|--------|----|------------|
| Endless Loop | `nexus.gate.endless_loop` | `max_iterations` (default `25`), `warning_at` (default `0`) |
| Stop Words | `nexus.gate.stop_words` | `words`, `word_files`, `case_sensitive` (default `false`) |
| Token Budget | `nexus.gate.token_budget` | `max_tokens` (default `100000`), `warning_threshold` (default `0.8`) |
| Rate Limiter | `nexus.gate.rate_limiter` | `requests_per_minute` (default `60`), `window_seconds` (default `60`), `pause_message` |
| Prompt Injection | `nexus.gate.prompt_injection` | `action` (`block`/`warn`), `patterns`, `patterns_file` |
| JSON Schema | `nexus.gate.json_schema` | `schema`/`schema_file`, `max_retries` (default `3`), `retry_prompt` |
| Output Length | `nexus.gate.output_length` | `max_chars` (default `5000`), `max_retries` (default `2`) |
| Content Safety | `nexus.gate.content_safety` | `action` (`block`/`redact`), `check_pii_*`, `check_secrets_*`, `check_credit_card`, `check_ip_internal`, `custom_patterns` |
| Context Window | `nexus.gate.context_window` | `max_context_tokens` (default `100000`), `trigger_ratio` (default `0.85`), `chars_per_token` (default `4.0`) |
| Tool Filter | `nexus.gate.tool_filter` | `include` (allowlist, takes precedence), `exclude` (blocklist) |

### Observers

| Plugin | ID | Key Config |
|--------|----|------------|
| Logger | `nexus.observe.logger` | `output` (`stdout`/`stderr`/`file`), `file_path`, `level` |
| OpenTelemetry | `nexus.observe.otel` | `endpoint`, `protocol` (`grpc`/`http`), `service_name`, `exclude_events` (supports `prefix.*` wildcards) |
| Thinking | `nexus.observe.thinking` | (none — persists thinking steps to session JSONL) |

### Skills

| Plugin | ID | Key Config |
|--------|----|------------|
| Skills Manager | `nexus.skills` | `scan_paths`, `trust_project` (`ask`/`always`/`never`), `max_active_skills` (default `10`), `catalog_in_system_prompt` (default `true`), `disabled_skills` |

## Test Configurations

Each `test-*.yaml` is consumed by an integration test in `tests/integration/`. Run the
suite with:

```bash
go test -tags integration ./tests/integration/ -v
```

| Config | Test file | Mode | What it covers |
|--------|-----------|------|----------------|
| `test-minimal.yaml` | `minimal_test.go` | live (Anthropic) | Bare-engine boot, no tools/gates/observers |
| `test-all-gates.yaml` | `all_gates_test.go` | live + mock variants | All 8 gates active, veto chain, gate interactions |
| `test-code-exec.yaml` | `code_exec_test.go` | mock | `run_code` tool dispatching shell via in-process Yaegi |
| `test-fallback.yaml` | `fallback_test.go` | live | Provider fallback chain (primary fails → fallback responds) |
| `test-fanout.yaml` | `fanout_test.go` | mock | Parallel multi-provider dispatch + collection |
| `test-memory-simple.yaml` | `memory_test.go` | mock | Unbounded `memory.simple` provider |
| `test-memory-summary-buffer.yaml` | `memory_test.go` | mock | Inline summarisation trigger + buffer collapse |
| `test-oneshot-json-schema.yaml` | `oneshot_json_schema_test.go` | live | One-shot + JSON schema validation gate retry |
| `test-static-plan-approval.yaml` | `static_plan_approval_test.go` | mock | Static planner approve/deny paths |
| `test-tool-choice.yaml` | `tool_choice_test.go` | mock | ReAct rotates ToolChoice across iterations |
| `test-tool-filter.yaml` | `tool_filter_test.go` | mock | Tool filter gate populates `req.ToolFilter` |
| `test-web-search.yaml` | `web_search_test.go` | live | `web_search` tool via Anthropic native search adapter |

## Demo Configurations

Manual / interactive only. Run with `bin/nexus -config configs/<name>.yaml`. Not in CI.

### Provider variations

| Config | What it shows | Prereqs |
|--------|---------------|---------|
| `demo-openai.yaml` | OpenAI provider — streaming, tool use mapping | `OPENAI_API_KEY` |
| `demo-openai-compat.yaml` | OpenAI-compatible endpoint via `base_url` (Ollama, LM Studio, vLLM) | local LLM running |
| `demo-gemini.yaml` | Gemini api-key auth | `GEMINI_API_KEY` |
| `demo-gemini-thinking.yaml` | Gemini 2.5 thinking parts + thinking observer JSONL | `GEMINI_API_KEY` |
| `demo-gemini-vertex.yaml` | Gemini via Vertex AI service-account auth | `GOOGLE_APPLICATION_CREDENTIALS` |
| `demo-mixed-providers.yaml` | Anthropic + OpenAI together, model role routing | both keys |

### Agent architectures

| Config | What it shows |
|--------|---------------|
| `demo-orchestrator-full.yaml` | Orchestrator decomposition + parallel workers + synthesis |
| `demo-planexec-approval.yaml` | PlanExec with approval-required dynamic planner |
| `demo-multi-subagent.yaml` | 3 named subagent instances with different roles + prompts |
| `demo-planned-react-tools.yaml` | ReAct + dynamic planner + tools + thinking observer |

### IO / Browser

| Config | What it shows |
|--------|---------------|
| `demo-browser-tools.yaml` | Browser UI + tool call rendering, WebSocket streaming |
| `demo-browser-orchestrator.yaml` | Browser UI + orchestrator worker progress display |

### Other

| Config | What it shows |
|--------|---------------|
| `demo-kitchen-sink.yaml` | Every plugin active simultaneously — boot stress test |
| `demo-gates-strict.yaml` | Tight gate limits, manual veto exploration |
| `demo-longterm-memory.yaml` | Cross-session memory CRUD walkthrough |
| `demo-rag.yaml` | RAG primitives (embeddings + vector store + ingest + knowledge_search) |
| `demo-skills.yaml` | Skill discovery + catalog injection + activation |
| `demo-observers-all.yaml` | Logger + OTel + thinking observers together (optional Jaeger collector) |
