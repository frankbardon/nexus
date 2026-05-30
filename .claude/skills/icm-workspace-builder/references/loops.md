# Looping stages

A stage can iterate as a whole: re-dispatch the entire sub-agent, with read access to its prior iteration's artifact, until the exit conditions all pass or `max_iterations` is reached. This is distinct from the inner-turn loop controlled by `turns.policy`.

Two levels of repetition exist; do not confuse them:

- **Turn loop** (`turns.policy`, `output.validators`): same sub-agent invocation, retries within a stage when validators fail. Cheap; the agent never leaves its session. Capped by `turns.max`.
- **Stage loop** (`loop.until`): full re-dispatch of the stage. Each iteration is a fresh sub-agent that sees its prior iteration via a `<previous_iteration>` block in the payload.

Use the turn loop for "fix this mistake." Use the stage loop for "iterate toward convergence" or "refine across attempts."

## Loop block

```yaml
loop:
  max_iterations: 10           # positive int; loader rejects non-positive
  until:
    - type: llm
      name: draft_approved
      rubric: validators/draft_approved.md
    - type: native
      name: under_word_cap
      handler: word_count_under
      args:
        max_words: 800
  on_exhausted: human_gate     # human_gate | error; default human_gate
```

`until` uses the same six predicate types as `output.validators` (see [`predicates.md`](predicates.md)), with the semantic inverted: validators say "is this acceptable?" — loop conditions say "are we done?" ALL conditions must pass for the loop to exit.

The loader validates the loop block at `plugins/workflows/icm/workspace/validate.go:392`:

- `max_iterations` must be a positive integer.
- `until` must be non-empty.
- `on_exhausted` defaults to `human_gate` when omitted; valid values are `human_gate` and `error`.
- Every entry in `until` is validated as a normal predicate.

## Iteration artifacts

Looping stages write iteration-indexed files in the session dir:

```
<session>/<runID>/<stage_id>/
  iter_01/<filename>
  iter_01/<filename>.icm.json
  iter_02/<filename>
  ...
  <filename>                   # finalized: written by orchestrator from latest accepted iter
```

Downstream stages referencing a looping stage's artifact resolve to the latest finalized iteration via the top-level filename. There is no syntax for referencing a specific prior iteration from a downstream stage — deliberate. If you want that, the loop should be split into discrete stages.

## Per-iteration payload

On iteration N > 1, the orchestrator auto-injects a `<previous_iteration>` block alongside the usual `<grounding>` and `<layer_data>`:

```xml
<icm_turn stage="02_draft" iteration="2" turn="1">
  <grounding>...</grounding>
  <layer_data>...</layer_data>
  <previous_iteration index="1">
    <artifact path="02_draft/iter_01/draft.md">...</artifact>
    <exit_failures>
      <failure name="draft_approved" type="llm">
        Reviewer wants under 800 words; current is 1140.
      </failure>
    </exit_failures>
  </previous_iteration>
  <instructions>...</instructions>
</icm_turn>
```

Same machinery as the turn-loop validator feedback, one level up.

## Human gates and loops

`human_gate: start | end | both` fires at the bounds of the *entire stage*, not per iteration. For per-iteration human input, add a `type: human` predicate to `loop.until`. The human-check pauses for input each iteration; "continue" requires a feedback string (by default), which injects into the next iteration's `<exit_failures>` block identically to LLM-judge feedback. The agent treats human and LLM feedback uniformly.

## When the loop exhausts without converging

`on_exhausted: human_gate` (the default): the final iteration's artifact is written normally, plus a sidecar metadata file at `<filename>.icm.json` carrying iteration count, convergence flag, and unmet conditions. The orchestrator then opens a HITL gate with three actions:

- **approve** — treat the final iteration as the stage's output; downstream stages proceed.
- **reject** — halt the run.
- **restart** — clear `iter_NN/` folders and begin again from iteration 1, counted against the plugin-level `loop_max_restarts` budget.

`on_exhausted: error` skips the gate and halts immediately. Use this when no human is in the loop and you want hard guarantees.

The `.icm.json` sidecar is reserved for ICM metadata generally — provenance, validator history, iteration counts. Artifacts themselves stay clean and schema-conformant.

## `loop_max_restarts`

The plugin config key `loop_max_restarts` (`plugins/workflows/icm/schema.json`) caps how many times a looping stage may restart from iteration 1 within a single run. Default 3; 0 means unlimited. This guards against infinite restart cycles when a workspace cannot converge.

The restart budget is per-stage, per-run. If a stage exhausts its restart budget the orchestrator halts the run regardless of `on_exhausted`.

## When to make a stage loop

Good fits:

- Iterative refinement until an LLM judge approves.
- Negotiation or back-and-forth where convergence is not predictable.
- Refining a draft against a hard constraint the first pass usually misses.

Bad fits:

- Anything with a known, fixed number of passes (use multiple stages or `turns.policy: fixed`).
- "Process every item in a list" — that is fan-out, not iteration.
- Anywhere the exit condition is vague. Vague conditions produce infinite-loop-by-default workflows that always exhaust to the human gate.

## Composing loop + fan-out

Loop and fan-out are orthogonal and compose. A fan-out stage where each item also loops to convergence is well-defined: each item independently iterates until its exit conditions pass, then the aggregate is built. The on-disk layout becomes:

```
<session>/<runID>/<stage_id>/
  items/<id>/iter_01/<filename>
  items/<id>/iter_02/<filename>
  ...
  items/<id>/<filename>        # finalized per-item
  <filename>                   # aggregate
```

See [`fan-out.md`](fan-out.md).

## Anti-patterns

- **Vague `loop.until` rubrics.** Same failure mode as vague validator rubrics, but worse: the loop runs to `max_iterations` every time and the work is wasted. Every predicate must be specific enough to be falsifiable.
- **`max_iterations` past 10 without thinking.** If you genuinely need 20 iterations, the stage is doing too much; split it.
- **Looping when fan-out fits.** If you are iterating to process each item in a list, that is `fan_out`, not `loop`.
- **No exit-condition feedback strategy.** A `loop.until` predicate that fails without producing actionable feedback (a bare regex with no `message`, an LLM rubric that just says "fail") makes every iteration redundant. Either the predicate carries a clear `message`/`feedback`, or the agent will not improve across iterations.
- **`on_exhausted: error` with high `max_iterations`.** Combines worst of both worlds: long, expensive failure. Either lower `max_iterations` to fail fast, or accept the human-gate fallback.
