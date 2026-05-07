# Context Engineering

The conversation history is the LLM's working memory. Every byte costs
tokens, slows the model, and competes with other content for attention.
Nexus exposes a layered context-curation stack so operators can shed
weight aggressively without losing the reasoning chain.

## Layers

The stack runs in cost order on every `before:llm.request`. Cheaper
deterministic layers fire first; LLM-touching layers run only when the
prior layers leave the request still over budget.

| # | Layer | Plugin | Cost | Cache Impact |
|---|-------|--------|------|--------------|
| 1 | Tool-result clearing | [`nexus.memory.tool_result_clear`](../plugins/memory/tool_result_clear.md) | None (heuristic) | volatile |
| 2 | Tool-def pruning | [`nexus.memory.tool_def_pruner`](../plugins/memory/tool_def_pruner.md) | None (heuristic) | session (re-cache once) |
| 3 | Topic-shift detection | [`nexus.memory.topic_pruner`](../plugins/memory/topic_pruner.md) | One classifier or phrase match | volatile |
| 4 | Reasoning-preserving summary | [`nexus.memory.summary_buffer`](../plugins/memory/summary_buffer.md) | One LLM call | session (re-cache once) |
| 5 | Compaction-and-restart | [`nexus.memory.compaction`](../plugins/memory/compaction.md) | One LLM call (full reset) | session (re-cache once) |

Curators never edit the static section — system prompt and operator-set
content are off-limits.

## Stability Descriptor

Every layer emits a `memory.curated` envelope event carrying a
stability-impact descriptor:

```go
type MemoryCurated struct {
    Layer            string             // which layer ran
    SectionsTouched  []CurationSection  // section_id, kind, tokens_delta
    CacheInvalidates bool               // does any touched section cross the cache prefix?
    AtTurn           int                // turn boundary
}
```

`CurationSection.Kind` is one of:

- `volatile` — recent turns; no cache impact.
- `session` — session-long content (compaction summary, tool definitions);
  controlled re-cache, charged once and amortised.
- `static` — system prompt / tool defs (forbidden — curator must not
  touch).

A future cache-aware prompt builder (Idea 05) consumes this descriptor
to scope re-cache cost. Until that lands, curations batch at turn
boundaries to keep cache invalidations predictable.

## Replay Determinism

Curation is heuristic and classifier-driven, so curators emit one
event per decision (`memory.tool_result_cleared`,
`memory.tool_def_pruned`, `memory.topic_shift_detected`,
`memory.summary_replaced`). The durable journal (Idea 01) records every
envelope so replay reproduces curation by replaying decisions, not by
re-running heuristics.

## Provider-side vs Harness-side

Anthropic's server-side `tool_result_clear` and `system_message_edit`
primitives, and OpenAI Responses API truncation policies, do similar
work in-provider. Nexus defaults to harness-side curation for
portability — every layer works the same regardless of which provider
the request lands on. Provider-native primitives can be enabled as an
opt-in optimisation when the configured provider supports them.

## Eval-Driven Tuning

The eval harness (Idea 07) supports curation-on/off pivots so operators
can compare task-success-rate against the cost savings of an aggressive
preset. Keep defaults conservative until eval data justifies tightening.

## Composing the Stack

A typical full stack:

```yaml
plugins:
  active:
    - nexus.agent.react
    - nexus.memory.summary_buffer       # base history with reasoning-preserving summary
    - nexus.memory.tool_result_clear    # layer 1
    - nexus.memory.tool_def_pruner      # layer 2
    - nexus.memory.topic_pruner         # layer 3
    - nexus.discovery.progressive       # complements layer 2 (class-level scoping)
    - nexus.gate.context_window         # last-resort compaction trigger
```

Layer 5 (compaction-and-restart) is implicit when the context-window
gate fires the existing `nexus.memory.compaction` coordinator.
