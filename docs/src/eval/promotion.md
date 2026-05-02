# Promoting a Session to an Eval Case

`nexus eval promote` (alias `nexus eval record`) turns a live session
directory under `~/.nexus/sessions/<id>/` into a deterministic eval case
under `tests/eval/cases/<case-id>/`. The whole flow runs offline — the
recorded journal is the source of truth, and replay short-circuits every
side effect.

This page covers:

- The failure → case workflow and when to use it.
- Exactly what `promote` writes (and what it deliberately doesn't).
- How to edit the synthesized `assertions.yaml` afterwards.
- Warnings the tool surfaces and how to interpret them.
- The non-replayable caveat (provider fallbacks, retries, errors).

For YAML schema details see [`case-format.md`](./case-format.md). For the
CLI flag list, run `nexus eval promote -h`.

## When to promote

The intended workflow is **production failure → offline regression case**:

1. A real session goes wrong. The journal at
   `~/.nexus/sessions/<id>/journal/` already captured the full event
   stream — every `llm.response`, every `tool.result`, every
   `io.input`. No additional logging needed.
2. You run `nexus eval promote --session <id> --case <new-id>`.
3. The promoted case is checked in, replays deterministically in CI, and
   keeps the failure from regressing.

Promotion is also the easiest way to bootstrap a new case from a clean
session you already ran by hand — just point `promote` at it instead of
hand-crafting the journal under `_record/`.

## What `promote` does

The implementation lives in `pkg/eval/promote/`. The pipeline:

1. **Validate** the source session has a journal and metadata
   ([`promote.go:validateSessionDir`](https://github.com/frankbardon/nexus/blob/main/pkg/eval/promote/promote.go)).
2. **Copy the journal** byte-for-byte from
   `<session>/journal/` into `<case>/journal/` — header, active segment,
   and any rotated segments. Replay reads the same on-disk shape, so a
   verbatim copy is the right contract.
3. **Copy the config snapshot** from
   `<session>/metadata/config-snapshot.yaml` into `<case>/input/config.yaml`.
   The case runs against the same engine config the recorded session used —
   no rewriting, no redaction (today). If the snapshot is missing
   (rare — only happens when a session crashed before the engine wrote
   metadata), `promote` writes a placeholder and warns.
4. **Reconstruct `inputs.yaml`** from journaled `io.input` events
   ([`inputs.go:ExtractInputs`](https://github.com/frankbardon/nexus/blob/main/pkg/eval/promote/inputs.go)).
   The runner doesn't read this file — it re-fires inputs from the
   journal directly — but `inputs.yaml` is the canonical record of the
   user side of the dialogue, kept for human review.
5. **Synthesize a starter `assertions.yaml`** from a single journal pass
   ([`scaffold.go:SynthesizeAssertions`](https://github.com/frankbardon/nexus/blob/main/pkg/eval/promote/scaffold.go)):
    - `event_count_bounds` — every distinct event type with `min=max=count`.
    - `token_budget` — observed input/output totals + 10% slack.
    - `latency` — observed turn-pair p50/p95 + 50% slack.
    - `tool_invocation_parity` — one block when at least one tool was used,
      with `count_tolerance: 0` and `arg_keys: true`.
    - `semantic: []` — empty, with a TODO comment for Phase 5 LLM-judge
      rubrics.
6. **Write `case.yaml`** with `name`, `description` (synthesized when
   omitted), `tags`, `owner` (falling back to `$USER`, then `unknown`),
   `freshness_days: 30`, `model_baseline` (extracted from
   `core.models.default`), and `recorded_at = now`.
7. **Optionally launch `$EDITOR`** on `<case>/assertions.yaml`. The
   fallback chain is `$EDITOR → $VISUAL → nano → vi`. Pass `--no-edit` to
   skip.

The CLI prints a one-line summary plus warnings to stderr and exits 0 on
success. On any error after the case directory was created, it removes the
partial directory so a retried `promote` is not blocked by stale shell.

## Editing `assertions.yaml`

The synthesized YAML is intentionally **strict** — every event-type bound
is `min=max=count`, every tool count tolerance is 0. This is the right
default: a freshly-promoted case should reproduce the source session
exactly, byte-for-byte. As you fold the case into a broader scenario, you
loosen the parts that legitimately vary:

- **Loosen `event_count_bounds`** for noise types: `core.tick`,
  `status.update`, `thinking.step`. These vary across runs even when the
  load-bearing behaviour is identical.
- **Tighten `tool_invocation_parity`** by adding new tools as your case
  evolves — but keep `arg_keys: true` to catch shape regressions.
- **Add an `event_sequence_distance`** assertion (with `threshold: 0.15`
  as a starting point) to gate trace drift across model upgrades.
- **Add an `event_emitted` with `where`** for the load-bearing tool calls
  (e.g. `tool.invoke where {name: read_file}` with `count: {min: 1}`).
- **Phase 5: add an `llm_judge` rubric** to the `semantic:` block once the
  case is stable. The TODO comment in the scaffold reminds you.

The deterministic checks are the regression gate; the rubric is the
quality gate. The two answer different questions and should both grow over
time.

## Warnings

`PromoteResult.Warnings` (printed to stderr in the CLI) flags conditions
that produced a valid case but might surprise you on replay:

- **`session ended with status=<X>`** — the source session's
  `metadata/session.json` has a non-completed/non-active status (typically
  `failed`). The case still promotes; this warning just tells you the
  authoring intent is "reproduce the failure", not "lock in a green
  baseline".
- **`config-snapshot.yaml not found`** — rare; only happens for sessions
  that crashed before the engine could write metadata. `input/config.yaml`
  is left as a stub with instructions; you must paste in the engine config
  manually.
- **`journal contains non-replayable event types …`** — see the next
  section.

Warnings never fail the command. The CLI prints them and exits 0; library
callers see them on `PromoteResult.Warnings`.

## The non-replayable caveat

Replay short-circuits side effects via the journal stash. That means
**any event whose presence depended on a side effect failure will not
re-fire under replay**. Promoting such a session produces a case where the
non-replayable branch is silently absent from the live observed stream.

The promoter recognises a small allow-list of these types
([`scaffold.go:nonReplayableTypes`](https://github.com/frankbardon/nexus/blob/main/pkg/eval/promote/scaffold.go)):

- `provider.fallback.error` / `provider.fallback.advance` /
  `provider.fallback.exhausted` — fired when a primary provider errored
  and the chain advanced. Replay pops the stashed `llm.response` from the
  primary without raising the error, so the fallback never fires.
- `llm.error` / `tool.error` — same shape; replay never re-raises the
  failure.
- `agent.iteration.exceeded` — depends on the agent loop's runtime
  behaviour. Re-derived during replay only if the same run-to-completion
  state holds.

When `promote` sees any of these in the source journal, it warns. The
case still lands on disk — the deterministic-replay half of the session
(the success path that *did* run) remains testable. But two follow-ups
are usually appropriate:

1. **Tighten the synthesized `event_count_bounds`** to exclude the
   non-replayable types — otherwise the case will fail with `count=0`
   for those entries on every replay.
2. **Move the failure-mode behaviour to a live integration test** under
   `tests/integration/`. The `provider-fallback` seed case
   ([`tests/eval/cases/provider-fallback/case.yaml`](https://github.com/frankbardon/nexus/blob/main/tests/eval/cases/provider-fallback/case.yaml))
   shows the pattern: the eval case validates the boot/configuration
   path, while a live integration test exercises the actual fallback.

## Round-trip example

```sh
# 1. You ran a session yesterday — find its ID under ~/.nexus/sessions/
ls ~/.nexus/sessions/

# 2. Promote it. --no-edit skips $EDITOR; --force overwrites existing.
nexus eval promote \
  --session 20260501T143000Z-abc123 \
  --case my-regression \
  --tags reproduction,react \
  --description "Repro of the missing-tool-result bug from Apr 30." \
  --no-edit

# 3. Replay it deterministically. No API key needed.
nexus eval run --case my-regression --deterministic

# 4. Edit the assertions to taste, commit, and CI is gated against
# a future regression of the same bug.
$EDITOR tests/eval/cases/my-regression/assertions.yaml
git add tests/eval/cases/my-regression
```

The full pipeline runs in seconds for the typical 8–30-event session.
Larger sessions scale linearly with journal length — `promote` does one
journal pass during synthesis and copies bytes verbatim for the journal
clone, so disk I/O dominates.

## Library API

For embedders the surface is `promote.Promote(ctx, opts)`:

```go
import "github.com/frankbardon/nexus/pkg/eval/promote"

res, err := promote.Promote(ctx, promote.PromoteOptions{
    SessionDir:  "/Users/me/.nexus/sessions/<id>",
    CaseID:      "my-regression",
    CasesDir:    "/repo/tests/eval/cases",
    Owner:       "me@example.com",
    Tags:        []string{"reproduction"},
    Description: "Optional override; defaults to a synthesized stub.",
    OpenEditor:  false,
    Force:       false,
})
```

Reference: [`pkg/eval/promote/promote.go:PromoteOptions`](https://github.com/frankbardon/nexus/blob/main/pkg/eval/promote/promote.go).
The Phase 4 sampler will reuse this entry point: a sampled failed session
becomes a Promote candidate the user can accept with one command.
