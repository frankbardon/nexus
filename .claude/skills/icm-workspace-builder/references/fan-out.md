# Fan-out stages

A stage can run once per item in a JSON array, dispatching a fresh sub-agent per item. Distinct from looping:

- **Loop** is convergence-driven: each iteration refines the prior one's output for the same target.
- **Fan-out** is data-driven: each invocation processes a different item.

The two are orthogonal and can compose. A fan-out stage where each item also loops to convergence is well-defined: each item independently iterates until its exit conditions pass, then the aggregate is built.

The loader validates fan-out at `plugins/workflows/icm/workspace/validate.go:410`.

## Fan-out block

```yaml
fan_out:
  source: 01_research/topics.json   # logical artifact ref; must point at a JSON array (after jsonpath)
  jsonpath: .topics                 # optional; default "." (whole document). gojq expression.
  item_var: topic                   # required; variable name in the XML payload
  item_id: .slug                    # optional; gojq expression evaluated per item for folder naming
  max_parallel: 1                   # default 1 (sequential); >1 dispatches in parallel
  on_item_failure: continue         # continue | halt; default continue
```

### `source`

A logical artifact reference matching `^([0-9]+_[a-z0-9_]+|00_input)/([^/]+)$`. The loader enforces shape and cross-stage ordering: `source` must point at a stage that runs before this one, and the filename must match that stage's declared `output.filename`.

At dispatch, the file is read. After applying `jsonpath`, the result MUST be a JSON array. If it does not resolve to an array (the source file is empty, the jsonpath misses, the document is not JSON), the stage fails per its `on_error` policy.

The reserved stage ID `00_input` is allowed as a source — useful for fanning out over an input list provided to the run.

### `jsonpath`

A gojq expression that navigates into the source document to locate the array. Optional; when omitted, the whole document is taken as the array.

Examples:
- `.topics` — array nested under top-level `topics`.
- `.results[] | select(.kind == "fact")` — flatten and filter.
- `.` (or empty) — the document itself is an array.

The loader compiles the expression at load via `github.com/itchyny/gojq`. Compile failure is a load error (`validate.go:432`).

### `item_var`

The variable name the per-invocation item is bound to in the XML payload's `<fan_out_item key="...">`. Required; non-empty string.

### `item_id`

Optional. A gojq expression evaluated against each item to produce a string used as the per-item folder name. Falls back to `item_NN` (1-indexed) when omitted or when the expression yields no scalar string.

Use this to make per-item artifact paths human-recoverable. `.slug`, `.id`, `.name | ascii_downcase | gsub(" "; "_")` are typical.

### `max_parallel`

Caps concurrent item invocations. Default 1 (sequential). Increase only when items are genuinely independent and the LLM provider's rate limits will tolerate the burst.

### `on_item_failure`

`continue` (default) records the failure in the per-item sidecar and keeps going. `halt` aborts the whole fan-out and applies `on_error` for the stage. Use `halt` only when a single item failure makes the aggregate meaningless.

## Per-item payload

The agent sees one item per invocation in a `<fan_out_item>` block alongside the usual grounding and layer_data:

```xml
<icm_turn stage="02_draft" item="topic_a" turn="1">
  <grounding>...</grounding>
  <layer_data>
    <fan_out_item key="topic">{"slug":"topic_a","name":"...","brief":"..."}</fan_out_item>
    <artifact path="01_research/research.md">...</artifact>
  </layer_data>
  <instructions>...</instructions>
</icm_turn>
```

The `key` attribute matches `fan_out.item_var`. The item is treated by the agent as just another piece of layer_data — material to transform.

## Session dir layout

- Per-item raw output: `<session>/<runID>/<stage_id>/items/<id>/<filename>` (plus `<filename>.icm.json` sidecar).
- Aggregate: `<session>/<runID>/<stage_id>/<filename>` — built mechanically after all items complete.

With looping inside fan-out: `<session>/<runID>/<stage_id>/items/<id>/iter_NN/<filename>`.

Downstream stages reference the aggregate by the usual `<stage_id>/<filename>` path. Per-item files exist for inspection and replay but are not first-class to the workflow.

## Aggregation

Mechanical, no LLM:

- `output.format: json` → array of `{item, result, path}` objects. Failed items appear as `{item, result: null, error: "..."}`.
- `output.format: text` → concatenation with `## Item: <id>` headers between sections. Failed items appear with `## Item: <id> (failed)` and the error message.

Anything fancier (synthesis, deduplication, reranking) is a separate downstream stage. Don't blur the boundary.

## Partial failures

With `on_item_failure: continue`, individual item failures don't stop the stage. The aggregate includes every item; failed ones have null results and error messages. Each failed item's `.icm.json` sidecar carries the full failure detail.

A downstream verifier (a cheap `type: native` predicate is usually right) can decide whether the partial success is acceptable. Common patterns:

- "At least N items must succeed."
- "No critical items (by ID) may fail" — use `contains_required_ids` over the aggregate.
- "Failure rate must be under X%."

With `on_item_failure: halt`, the first item failure halts the fan-out and applies `on_error` for the stage.

## Composition

- **With loop.** Per-item convergence. Each item independently iterates until its loop.until passes.
- **With validators.** Per-item. Validators run on each item's output; failure triggers the turn loop for that item only.
- **With human gates.** `human_gate` fires at stage bounds — before any item dispatches, after the aggregate is built. For per-item human review use a `type: human` predicate in `output.validators` or `loop.until`.

## Empty source

If the source resolves to an empty array, no items dispatch and the aggregate is empty (`[]` for json, empty file for text). The stage counts as success. Downstream stages see an empty artifact.

If empty input should be a failure mode, declare a `native:json_path_exists` (or similar) predicate on the *previous* stage's output that rejects empty arrays. Don't make the fan-out stage responsible for input validation.

## When to fan out

Good fits:

- "Generate a draft for each topic in a list."
- "Run a check against every section of a document."
- "Produce a translation for each language in a target list."

Bad fits:

- Recursive or tree-shaped work. Use sub-stage delegation patterns (verifier stages, sub-workflow plugins).
- Cases where items depend on each other's outputs. That is a sequence of stages or a loop, not fan-out.
- Cases where the number of items is fixed and small (1–3). Just write that many stages; clearer for readers.

## Anti-patterns

- **Fan-out source that does not reliably resolve to an array.** If the previous stage sometimes emits an array and sometimes an object, fix the previous stage's schema — don't paper over with conditional logic in `jsonpath`.
- **Fan-out source pointing at a text stage.** The source must be a JSON document (after `jsonpath`). A markdown artifact is not a valid source — change the upstream stage's `output.format` to `json` with a schema that defines an array of items.
- **Aggregate-level validators.** Validators run per-item. If aggregate quality matters, write a downstream stage that reads the aggregate.
- **`max_parallel` higher than your provider's rate limit.** The orchestrator does not throttle to provider limits. Set conservatively and monitor.
- **Reusing the same `item_id` across items.** If `item_id` yields the same string for two items, they collide in `items/<id>/`. Use a unique field, or fall back to the `item_NN` default.
