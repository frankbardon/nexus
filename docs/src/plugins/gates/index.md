# Gates

Gates are plugins that subscribe to `before:*` events and may **veto** them
before the corresponding action takes effect. They are how Nexus enforces
iteration limits, banned content, token budgets, schema validation, rate
limits, and similar guardrails — without baking the policy into agents or
providers.

## Plugins

| Plugin                            | Vetoes                  | Purpose |
|-----------------------------------|-------------------------|---------|
| `nexus.gate.endless_loop`         | `before:llm.request`    | Cap LLM calls per turn (replaces agent `max_iterations`). |
| `nexus.gate.stop_words`           | `before:llm.request`, `before:io.output` | Block messages containing banned terms. |
| `nexus.gate.token_budget`         | `before:llm.request`    | Cap session token usage. |
| `nexus.gate.rate_limiter`         | `before:llm.request`    | Throttle LLM call frequency (pause via `gate.llm.retry`, not reject). |
| `nexus.gate.prompt_injection`     | `before:llm.request`    | Detect and block prompt-injection patterns in user input. |
| `nexus.gate.json_schema`          | `before:io.output`      | Validate output against JSON Schema; LLM-retry on failure. |
| `nexus.gate.output_length`        | `before:io.output`      | Cap response length; LLM-retry to compress. |
| `nexus.gate.content_safety`       | `before:io.output`      | Block or redact PII / secrets / sensitive content. |
| `nexus.gate.context_window`       | `before:llm.request`    | Estimate context size; trigger compaction when approaching the limit. |
| `nexus.gate.tool_filter`          | `before:llm.request`    | Modify the tool list (allowlist / blocklist). |
| `nexus.gate.approval_policy`      | `before:tool.invoke`, `before:llm.request` | Policy-driven HITL approvals; emits `hitl.requested` and applies the operator's allow/reject/edit. |

## Configuration

Every gate's full YAML config — keys, types, defaults — is in the
[Configuration Reference](../../configuration/reference.md#gates).

## Mechanics

The vetoable event system, priority ordering, and the shared `gate.llm.retry`
pattern are documented in [`.claude/docs/gates.md`](../../../.claude/docs/gates.md).
That document is the design-level reference; the configuration reference is the
keys-and-defaults reference.
