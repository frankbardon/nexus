# Tool System Deep Reference

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

## Parallel Tool Dispatch

ReAct agent can fan out multiple tool calls from a single LLM response across bounded goroutines instead of running them one at a time. Opt-in, ReAct-only.

```yaml
nexus.agent.react:
  parallel_tools: false   # default: sequential, one-at-a-time
  max_concurrent: 4       # worker cap when parallel_tools is true
```

**Flow when enabled (LLM returns N>1 tool calls):**

1. `before:tool.invoke` gates evaluate **serially**, preserving gate state across the batch (priority 10 → 50 ordering).
2. Vetoed calls emit a synthetic `tool.result` with `Error: "Tool call vetoed: …"` directly — no `before:tool.result` round-trip, since the notice is agent-generated policy, not tool output.
3. Passing calls dispatch in goroutines guarded by a `max_concurrent` semaphore. Each worker emits `tool.invoke`, which the tool plugin runs inline on that goroutine and which drives a matching `tool.result`.
4. `handleToolResult` buffers results by `ToolCall.ID` until all N arrive, then flushes to `history` in **LLM-returned order** (not completion order). The next `llm.request` sees `tool_call_id`s in the order the provider expects.
5. `ToolCall.Sequence` carries the 0-based LLM-returned index so observers can reorder completion-order events (thinking logs, UIs) back into request order.

**Falls back to the sequential path when** `parallel_tools: false`, or when the batch has only one call (fan-out overhead pointless).

**Cancellation.** `turnCtx` (created per parallel batch, cancelled on user interrupt or new turn) gates the semaphore. Not-yet-dispatched workers short-circuit with a synthetic `"tool dispatch cancelled"` error so the barrier fills and the turn can unwind. In-flight tools already executing aren't preempted — tools that honor context (e.g. shell's `exec.CommandContext`) do so via their own internal timeouts, not this cancellation.

**Implementation pointers.**
- Dispatch branch: `plugins/agents/react/plugin.go` (`handleLLMResponse`, `parallel := p.parallelTools && len(resp.ToolCalls) > 1`)
- Ordered-flush barrier: `handleToolResult` — uses `expectedToolIDs` + `pendingResults` (non-nil only while a parallel batch is in flight)
- `ParentCallID`-flagged internal calls (run_code-style sub-calls) bypass the barrier and do not consume a slot.

**Out of scope for v1** (see issue #14): per-tool concurrency caps, batch-wide timeout, cross-turn parallelism, `planexec`/`orchestrator` agents, provider-driven parallelism.

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
