# Eval Harness Overview

Nexus ships a first-class eval workflow built directly on top of the durable
journal. The harness records, replays, and scores agent sessions offline —
no API key required for the deterministic path — and gates regressions in
CI.

This document covers the concept and the moving pieces. For YAML keys see
[`configuration/reference.md`](../configuration/reference.md#eval-harness);
for the `case.yaml` / `assertions.yaml` schema see
[`case-format.md`](./case-format.md).

## Why evals (in Nexus terms)

Once the journal exists, every session is replayable. An eval is a journal
plus a bundle of assertions that say "this trace is the desired behaviour".
Phase 1 shipped the runner; Phase 2 ships the CLI, the multi-case runner,
the baseline differ, and five seed cases.

Two modes:

| Mode               | Replay short-circuit | LLM judge | When |
|--------------------|----------------------|-----------|------|
| `--deterministic`  | yes                  | skipped   | every PR; gates merge |
| `--full`           | yes                  | run, `temperature=0`, cached | nightly; gates release |

The `--deterministic` contract is binding: PRs never block on judge flake.
`--full` runs judge calls but still drives them through the journal stash —
side effects are short-circuited even when the rubric is being graded.
(`--full` is declared in Phase 2 and lit up in Phase 5.)

## What a case is

A case is a directory under `tests/eval/cases/<id>/`:

```
tests/eval/cases/<id>/
  case.yaml         # name, description, tags, owner, freshness, model_baseline
  input/
    config.yaml     # engine config under test (typically mock provider)
    inputs.yaml     # scripted user inputs (record-side; live agent reads from journal)
  journal/          # full copy of the source session journal
    header.json
    events.jsonl
  assertions.yaml   # deterministic + (Phase 5) semantic assertions
  _record/          # optional: go-tagged recorder that regenerates journal/
    main.go
```

`journal/` is a 1:1 copy of `~/.nexus/sessions/<source-session>/journal/` at
promotion time. There is no second fixture format.

The runner reads `case.yaml`, builds the engine from `input/config.yaml`,
overrides `core.sessions.root` to a tempdir, calls `engine.Replay()`, and
collects events for assertion evaluation.

## What a report is

A report is one JSON document per `nexus eval run` invocation:

```
tests/eval/reports/<run-id>/
  report.json       # schema_version=1; per-case + summary
  summary.txt       # human-readable counterpart
  _sessions/<id>/   # per-case session workspace (transcript, config snapshot)
```

`schema_version` is stable: the baseline differ (`pkg/eval/baseline`) keys
off field names. Bumping the version is a deliberate event with an explicit
migration note.

## How a run flows

1. **Discovery.** The CLI walks `cases_dir`, parses each `case.yaml`,
   filters by `--tags`.
2. **Per-case engine.** Each case constructs its own engine from
   `input/config.yaml`. `core.sessions.root` is overridden to a per-run
   tempdir under `<reports_dir>/<run-id>/_sessions/`.
3. **Replay.** `journal.NewCoordinator` seeds the FIFO stash with
   `llm.response` / `tool.result` / `io.ask.response` payloads from the
   journal, then re-emits `io.input` events in seq order. The live agent
   reacts as if the inputs were fresh; side-effecting plugins detect
   `engine.Replay.Active()` and pop the stash instead of calling out.
4. **Observation.** After replay finishes, the runner reads the *live*
   session's freshly-written journal as the authoritative observed event
   stream. A wildcard collector is kept as a fallback only (wildcard
   dispatch order is post-order, which would skew sequence assertions).
5. **Assertion evaluation.** Each `Assertion.Evaluate(observed, golden)`
   produces an `AssertionResult`. The case passes iff every assertion
   passes.
6. **Aggregation.** `pkg/eval/report.Aggregate` rolls Results into a
   `Report` and writes JSON + summary.

## How baseline gating works

`nexus eval baseline --against <path>` loads two reports (file or directory)
and computes a `Diff`. Per-case it records pass/fail movement, latency p50/
p95 deltas, and token deltas. Per-run it records new/missing cases and the
pass-rate delta.

Two thresholds drive the exit code:

- `eval.baseline.fail_on_score_drop` — absolute pass-rate drop threshold.
  `0` disables.
- `eval.baseline.fail_on_latency_p95_drop` — relative p95-latency increase
  threshold (per case). `0` disables.

Plus a hard rule: any case that flipped `pass → fail` is treated as a
regression and fails the baseline run. The `Diff.Breached` field is the
machine-readable record of which gate (if any) tripped.

## Determinism contract

The journal is the source of truth. During replay:

- LLM providers (Anthropic / OpenAI / Gemini / mock) check
  `engine.Replay.Active()` and emit the next stashed `llm.response` instead
  of calling the API.
- Side-effecting tools (`shell`, `file`, `code_exec`, `web`, `pdf`,
  `ask_user`) follow the same pattern with `tool.result`.
- Boot-time emissions (`skill.discover`, `tool.register`, etc.) re-fire
  live during every replay — they are deterministic by construction.
- Live-emitted derived events (`plan.created`, `plan.result`,
  `agent.turn.start`/`end`) are also re-emitted live.

This means a case's assertions can name any of those event types and they
will all be present in the observed stream during a clean replay. What is
*not* re-emitted is anything that depended on a side effect that didn't
happen — most notably, `provider.fallback` only fires when the primary
errored. The `provider-fallback` seed case demonstrates how to write
assertions for the boot+config-validation half of that scenario; the live
error path is covered by the integration test under `tests/integration/`.

## Future phases

| Phase | Adds | Status |
|-------|------|--------|
| 1 | Core runner, 7 deterministic assertions, 1 seed case | Shipped |
| 2 | CLI, multi-case, baseline diff, 5 seed cases | Shipped |
| 3 | `nexus eval record / promote` (failure → case in one command) | Shipped — see [`promotion.md`](./promotion.md) |
| 4 | `plugins/observe/sampler/` (online sample capture) | Shipped — see [Online sampling](#online-sampling) |
| 5 | `--inspect-mode` JSON protocol for external harnesses | Shipped — see [External harness integration](#external-harness-integration). `--full` LLM judge remains stubbed. |

The case directory layout finalized in Phase 2 is forward-compatible with
Phase 3 promotion — `record` writes the same shape that Phase 2 reads. The
report's `schema_version: "1"` is the contract Phase 5's protocol mode and
external harnesses (Inspect AI, Braintrust) will pin against.

## Online sampling

Phase 4 adds the [`nexus.observe.sampler`](../plugins/observers/sampler.md)
plugin: an opt-in observer that snapshots a configurable fraction of live
sessions (plus every failed session, when `failure_capture` is on) into
`~/.nexus/eval/samples/<id>/`. Captured directories share the
case-compatible journal layout, so they feed straight into the promotion
pipeline once an operator picks one out of the sample set.

Activation is two opt-ins (off by default):

```yaml
plugins:
  active:
    - nexus.observe.sampler

  nexus.observe.sampler:
    enabled: true
    rate: 0.05
    failure_capture: true
    out_dir: ~/.nexus/eval/samples
```

See [the sampler plugin doc](../plugins/observers/sampler.md) for the
capture-decision rules, the `eval.candidate` event contract, the
pluggable redactor hook, and the integration story with
`nexus eval promote`.

## External harness integration

Phase 5 adds `nexus eval --inspect-mode`: a single-shot, headless,
stdin/stdout JSON protocol that lets external eval harnesses (AISI
Inspect AI, Braintrust, custom CI tooling) drive Nexus from any
language. Pipe a JSON request in, parse a JSON response out, exit code
indicates success.

```bash
echo '{"schema":1,"config_path":"configs/coding.yaml","user_input":"hello"}' \
  | nexus eval --inspect-mode
```

The wire format is the durable contract. Schema-stability snapshot tests
in `pkg/eval/protocol/` enforce that the byte layout cannot drift
silently — every change requires updating the snapshot deliberately, and
the version field (`schema: 1`) is the migration marker external shims
pin against.

Nexus does **not** ship a Python or Node shim. The protocol is the
contract; any out-of-tree integration is a thin subprocess wrapper. See
[`inspect-protocol.md`](./inspect-protocol.md) for the full reference,
field-by-field documentation, error code table, and a worked Python
shim example you can copy into your harness.
