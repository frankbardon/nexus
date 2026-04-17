# Nexus Test Configuration Guide

Manual testing configs covering major system permutations. Run any with:

```bash
bin/nexus -config configs/<name>.yaml
```

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
| File IO | `nexus.tool.file` | `base_dir`, `allow_external_writes` (default `false`), `tools` map (`read_file`, `read_file_chunk`, `write_file`, `check_file_size`, `list_files` — all default `true`) |

### Memory

| Plugin | ID | Key Config |
|--------|----|------------|
| Conversation | `nexus.memory.conversation` | `max_messages` (default `100`), `persist` (default `true`) |
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

### Baseline

| Config | What It Tests | Run |
|--------|---------------|-----|
| `test-minimal.yaml` | Bare minimum — no tools, no gates, no observers. Validates engine boots clean with conversation-only flow. | `bin/nexus -config configs/test-minimal.yaml` |
| `test-kitchen-sink.yaml` | Every plugin active simultaneously. Stress tests boot, dependency resolution, event bus under max load. | `bin/nexus -config configs/test-kitchen-sink.yaml` |

### Provider Variations

| Config | What It Tests | Run |
|--------|---------------|-----|
| `test-openai.yaml` | OpenAI provider — streaming, tool use mapping, response format. | `bin/nexus -config configs/test-openai.yaml` |
| `test-openai-compat.yaml` | OpenAI-compatible endpoint (Ollama, LM Studio, vLLM) via `base_url` override. | `bin/nexus -config configs/test-openai-compat.yaml` |
| `test-mixed-providers.yaml` | Both providers — Anthropic for reasoning, OpenAI for quick. Validates multi-provider model role routing. | `bin/nexus -config configs/test-mixed-providers.yaml` |

### Agent Architectures

| Config | What It Tests | Run |
|--------|---------------|-----|
| `test-orchestrator-full.yaml` | Orchestrator with tools + multi-model roles. Tests decomposition, parallel workers, synthesis. | `bin/nexus -config configs/test-orchestrator-full.yaml` |
| `test-planexec-approval.yaml` | PlanExec with approval-required dynamic planner. Tests plan → approval → execution → replan flow. | `bin/nexus -config configs/test-planexec-approval.yaml` |
| `test-multi-subagent.yaml` | 3 named subagent instances (researcher/coder/reviewer) with different model roles and prompts. | `bin/nexus -config configs/test-multi-subagent.yaml` |

### Planning

| Config | What It Tests | Run |
|--------|---------------|-----|
| `test-planned-react-tools.yaml` | ReAct + dynamic planner + tools + thinking observer. Tests planning → execution handoff with tool use. | `bin/nexus -config configs/test-planned-react-tools.yaml` |
| `test-static-plan-approval.yaml` | Static planner with approval + fixed code review steps. Tests static plan display and step execution. | `bin/nexus -config configs/test-static-plan-approval.yaml` |

### Gates

| Config | What It Tests | Run |
|--------|---------------|-----|
| `test-all-gates.yaml` | Every gate active simultaneously in warn mode. Tests priority ordering, veto chain, no conflicts. | `bin/nexus -config configs/test-all-gates.yaml` |
| `test-gates-strict.yaml` | Tight limits — 5 iterations, 5k tokens, 5 req/min, 500 char output. Tests veto behavior under pressure. | `bin/nexus -config configs/test-gates-strict.yaml` |
| `test-tool-choice.yaml` | Tool choice sequence pattern: required → forced `file_read` → auto. | `bin/nexus -config configs/test-tool-choice.yaml` |
| `test-tool-filter.yaml` | Tool filter allowlist — only file tools available, shell registered but hidden. | `bin/nexus -config configs/test-tool-filter.yaml` |
| `test-oneshot-json-schema.yaml` | One-shot + JSON schema validation gate. Tests structured output enforcement with retry. | `bin/nexus -config configs/test-oneshot-json-schema.yaml` |

### IO / Browser

| Config | What It Tests | Run |
|--------|---------------|-----|
| `test-browser-tools.yaml` | Browser UI + shell/file tools. Tests WebSocket streaming with tool calls. | `bin/nexus -config configs/test-browser-tools.yaml` |
| `test-browser-orchestrator.yaml` | Browser UI + orchestrator. Tests worker progress display in web UI. | `bin/nexus -config configs/test-browser-orchestrator.yaml` |

### Memory

| Config | What It Tests | Run |
|--------|---------------|-----|
| `test-longterm-memory.yaml` | Long-term memory with auto_load + auto_save_instructions. Tests index injection, CRUD tools, cross-session persistence. | `bin/nexus -config configs/test-longterm-memory.yaml` |

### Observers

| Config | What It Tests | Run |
|--------|---------------|-----|
| `test-observers-all.yaml` | Logger + OTel + thinking active together. Tests observer coexistence, span export, JSONL persistence. Requires OTLP collector on localhost:4317. | `bin/nexus -config configs/test-observers-all.yaml` |

### Skills

| Config | What It Tests | Run |
|--------|---------------|-----|
| `test-skills.yaml` | Skills discovery, catalog prompt injection, activation limit, trust modes. | `bin/nexus -config configs/test-skills.yaml` |

## Coverage Matrix

Dimensions each config exercises (marked when non-default):

| Config | Agent | Provider | IO | Tools | Gates | Memory | Planner | Observer | Skills |
|--------|-------|----------|----|-------|-------|--------|---------|----------|--------|
| test-minimal | react | anthropic | tui | - | - | conv | - | - | - |
| test-openai | react | **openai** | tui | - | loop | conv | - | logger | - |
| test-openai-compat | react | **openai+base_url** | tui | - | loop | conv | - | logger | - |
| test-mixed-providers | react | **both** | tui | - | loop | conv | - | logger | - |
| test-all-gates | react | anthropic | tui | both | **all 8** | conv | - | logger | - |
| test-gates-strict | react | anthropic | tui | - | **4 tight** | conv | - | logger | - |
| test-tool-choice | react | anthropic | tui | both | loop | conv | - | logger | - |
| test-tool-filter | react | anthropic | tui | both | loop+**filter** | conv | - | logger | - |
| test-orchestrator-full | **orch** | anthropic | tui | both | loop | conv | - | logger | - |
| test-planexec-approval | **planexec** | anthropic | tui | both | loop | conv | **dynamic** | logger | - |
| test-multi-subagent | react+**3 sub** | anthropic | tui | both | loop | conv | - | logger | - |
| test-longterm-memory | react | anthropic | tui | - | loop | conv+**lt** | - | logger | - |
| test-browser-tools | react | anthropic | **browser** | both | loop | conv | - | logger | - |
| test-browser-orchestrator | **orch** | anthropic | **browser** | both | loop | conv | - | logger | - |
| test-skills | react | anthropic | tui | - | loop | conv | - | logger | **yes** |
| test-observers-all | react | anthropic | tui | - | loop | conv | - | **all 3** | - |
| test-planned-react-tools | react | anthropic | tui | both | loop | conv | **dynamic** | logger+think | - |
| test-static-plan-approval | react | anthropic | tui | both | loop | conv | **static** | logger | - |
| test-oneshot-json-schema | react | anthropic | **oneshot** | - | **json** | conv | - | logger | - |
| test-kitchen-sink | react+sub | anthropic | tui | both | **all 8** | conv+lt | dynamic | logger+think | yes |
