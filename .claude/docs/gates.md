# Gates

Quality, safety, and operational gates. Standard plugins subscribing to `before:*` vetoable events at high priority (10). Activate per-profile via `plugins.active` list.

## Vetoable Event System

Gates use `EmitVetoable()` which wraps payloads in `VetoablePayload{Original, Veto}`. Handlers inspect `Original` (e.g. `*events.LLMRequest`), set `Veto` to block. Priority ordering: gates at 10, agents at 50 — gates always evaluate first.

Hook points: `before:llm.request` (input-side), `before:llm.response` (model output, loop-facing), `before:io.output` (user-facing output), `before:tool.invoke`, `before:tool.result`, `before:skill.activate`.

Output-side gates (`content_safety`, `stop_words`, `json_schema`, `output_length`) skip any `AgentOutput` marked by a `before:llm.response` substitution — `engine.IsVetoSubstituted(output.Metadata)`. Re-judging a gate-authored refusal could veto it into a blank. Corollary: never interpolate raw model output into a veto reason; it becomes user-visible content that those gates no longer scan.

`before:llm.response` is the exception to the veto contract: a veto **substitutes** rather than blocks, because agent loops block waiting for `llm.response`. The substitute always has `ToolCalls` cleared, so a blocked response can't drive tool execution — which is what `before:io.output` can't do, since the loop has already run the tools by then. Producers publish via `engine.PublishLLMResponse`, never a bare `bus.Emit("llm.response", ...)`. See `docs/src/architecture/event-bus.md`.

`before:llm.stream.chunk` is the streaming counterpart, and the hook a disclosure-preventing gate needs. It fires once per candidate text release, before any of it reaches the bus, with an `*events.StreamSegment`: handlers pass, rewrite `Content` to redact, **suspend** via `HoldFrom` (withhold a trailing fragment; the publisher re-offers it with the next delta), or veto to block the rest of the turn. Withheld text is never emitted, so a block prevents disclosure rather than retracting it. Scan `seg.Full()` and act on matches ending at or after `seg.PendingOffset()` — that makes detection independent of provider chunking, and stops a gate cutting a stream over bytes it already released. A cut stream carries no replacement text; the user-facing message comes from the same gate's `before:llm.response` handler. Producers publish deltas via `engine.StreamPublisher`, never a bare `bus.Emit("llm.stream.chunk", ...)`.

A gate that holds `before:llm.response` without `before:llm.stream.chunk` costs the whole deployment streaming: the engine reads declared `Subscriptions()` at boot and clears `Stream` on outbound requests, rather than letting a veto that cannot prevent disclosure look like one that can. Override with `core.streaming.response_gate_policy: allow`.

Resume mechanism: Gates that veto `before:llm.request` temporarily (rate limiter, context window) emit `gate.llm.retry` when the condition clears. All agent plugins (react, planexec, orchestrator) subscribe to this event and re-invoke `sendLLMRequest()` if they have an active turn — no user re-submission needed.

## Gate Config Reference

All gates are optional — only active when listed in `plugins.active`.

```yaml
# Iteration limiting (replaces agent max_iterations).
nexus.gate.endless_loop:
  max_iterations: 25    # default 25
  warning_at: 20        # emit warning N iterations before limit (0 = off)

# Banned word detection.
nexus.gate.stop_words:
  words: ["forbidden"]  # inline word list
  scan_llm_responses: false  # also gate before:llm.response (a match drops tool calls)
  scan_stream: <follows scan_llm_responses>  # also gate before:llm.stream.chunk (blocks before the text is shown)
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
  scan_llm_responses: false  # also gate before:llm.response (block mode drops tool calls)
  scan_stream: <follows scan_llm_responses>  # also gate before:llm.stream.chunk (blocks before the text is shown)
  stream_hold_back: 0        # extra trailing bytes to withhold; only for whitespace-spanning custom patterns
  scan_tool_results: false   # also gate before:tool.result
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
