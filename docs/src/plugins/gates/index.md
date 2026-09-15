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
| `nexus.gate.stop_words`           | `before:llm.request`, `before:io.output`, `before:llm.response` *(opt-in)*, `before:llm.stream.chunk` *(opt-in)* | Block messages containing banned terms. |
| `nexus.gate.token_budget`         | `before:llm.request`    | Cap session token usage. |
| `nexus.gate.rate_limiter`         | `before:llm.request`    | Throttle LLM call frequency (pause via `gate.llm.retry`, not reject). |
| `nexus.gate.prompt_injection`     | `before:llm.request`    | Detect and block prompt-injection patterns in user input. |
| `nexus.gate.json_schema`          | `before:io.output`      | Validate output against JSON Schema; LLM-retry on failure. |
| `nexus.gate.output_length`        | `before:io.output`      | Cap response length; LLM-retry to compress. |
| `nexus.gate.content_safety`       | `before:io.output`, `before:tool.result` *(opt-in)*, `before:llm.response` *(opt-in)*, `before:llm.stream.chunk` *(opt-in)* | Block or redact PII / secrets / sensitive content. |
| `nexus.gate.context_window`       | `before:llm.request`    | Estimate context size; trigger compaction when approaching the limit. |
| `nexus.gate.tool_filter`          | `before:llm.request`    | Modify the tool list (allowlist / blocklist). |
| `nexus.gate.approval_policy`      | `before:tool.invoke`, `before:llm.request` | Policy-driven HITL approvals; emits `before:hitl.requested` then `hitl.requested` and applies the operator's allow/reject/edit. |

## Configuration

Every gate's full YAML config — keys, types, defaults — is in the
[Configuration Reference](../../configuration/reference.md#gates).

## Gating a streaming response

A gate that exists to prevent *disclosure* has a problem with streaming:
`before:llm.response` fires only once the stream is finished, so its veto lands
after the text has been rendered. `before:llm.stream.chunk` is the answer — it
offers each text delta to handlers *before* any of it reaches the bus, so a
block prevents disclosure rather than asking transports to retract it.

`stop_words` and `content_safety` both implement it, enabled by the same
`scan_llm_responses` flag that enables their response-level gating (override
with `scan_stream`). The pattern for a custom gate:

1. **Hold the fragment you cannot yet judge.** `seg.HoldFrom(i)` withholds
   `Content` from offset `i`, and the publisher re-offers it with the next
   delta. This is what lets a gate defer rather than gamble on a trailing
   `123-45-` that may or may not become an SSN.
2. **Scan `seg.Full()`, act on `seg.PendingOffset()`.** Scanning the cumulative
   turn is what makes detection independent of how the provider chunked;
   ignoring matches that end at or before the pending offset is what stops a
   gate cutting a stream over bytes it already released.
3. **Set `vp.Veto` to block.** Nothing further is published for the turn. Do
   not set replacement text — a stream has no substitute, only an end; the
   user-facing message comes from your `before:llm.response` handler when the
   provider publishes the completed response.

A gate that does *not* implement the hook is not silently left unprotected: the
engine turns streaming off for the whole deployment and logs which plugin cost
it. See [`core.streaming`](../../configuration/reference.md#corestreaming).

## Mechanics

The vetoable event system, priority ordering, and the shared `gate.llm.retry`
pattern are documented in [`.claude/docs/gates.md`](../../../.claude/docs/gates.md).
That document is the design-level reference; the configuration reference is the
keys-and-defaults reference.
