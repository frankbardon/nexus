# Quickstart — From Session to Green CI

This is the linear walkthrough: capture a real session, promote it into a
case, tighten the rubric, run it locally, lock in a baseline, and wire the
result into CI. By the end you have one regression test for one bad trace,
gating future PRs.

For schema details see [`case-format.md`](./case-format.md). For background
concepts see [`overview.md`](./overview.md).

## 1. Capture a session

Boot Nexus normally and run a session that demonstrates the failure or the
behaviour you want to lock in:

```sh
bin/nexus -config configs/coding.yaml
```

Drive it through whatever interaction surfaces the bug. When the session
ends (Ctrl-D in the TUI, or close the browser tab), the journal is flushed
to disk under `~/.nexus/sessions/<id>/`.

To find the session ID of the run you just made:

```sh
ls -t ~/.nexus/sessions/ | head -1
```

The IDs are timestamped (`20260501T143022Z`-style), so the most-recent
directory is the one you want.

## 2. Promote the session into a case

`nexus eval promote` copies the session journal verbatim into a case
directory and synthesizes a starter `assertions.yaml`:

```sh
bin/nexus eval promote \
  --session 20260501T143022Z \
  --case   my-regression \
  --no-edit
```

Useful flags:

- `--owner you@example.com` — recorded in `case.yaml`.
- `--tags react,coding,regression` — used by `--tags` filtering at run time.
- `--description "Reproduces the off-by-one in the file diff path."` —
  appears in run summaries.

The promoter prints any warnings (failed-session journals, non-replayable
events) to stderr — read them. They tell you where the case will be
brittle on replay.

## 3. Edit the rubric

Open the synthesized assertions file:

```sh
$EDITOR tests/eval/cases/my-regression/assertions.yaml
```

The starter file uses broad `event_count_bounds` and one
`event_sequence_distance`. Tighten the bounds where you have signal —
e.g. drop the slack on `tool.invoke` counts to exactly the number you
care about, or shrink the sequence-distance threshold from `0.30` to
`0.15`. For latency, lower the `p50_ms` / `p95_ms` budgets to whatever
the captured run actually exhibits, plus a small headroom.

See [`case-format.md`](./case-format.md) for the seven assertion kinds
and their fields.

## 4. Run the case

```sh
bin/nexus eval run --case my-regression
```

Expected output is a one-liner human summary on stdout plus the report
location on stderr:

```
PASS my-regression  3 assertions  234ms

report: tests/eval/reports/20260501T144517Z/report.json
```

The full report (JSON) and a human counterpart (`summary.txt`) land
under `tests/eval/reports/<run-id>/`. Inspect them when something fails.

## 5. Establish a baseline

Once the case is green and you trust the trace, copy the run-id directory
aside as the reference baseline:

```sh
cp -r tests/eval/reports/20260501T144517Z tests/eval/reports/baseline
```

From any future run, diff against the baseline:

```sh
bin/nexus eval baseline \
  --against tests/eval/reports/baseline \
  --report  tests/eval/reports/20260502T081200Z
```

> **Run-id subdir convention.** `eval run --report-dir X` writes the
> report to `X/<run-id>/report.json` — `X` itself does not contain a
> `report.json`. `baseline --against` accepts either the run-id
> subdirectory or a `report.json` file directly, but does **not**
> auto-descend a parent. Point at the run-id dir, not its parent.

## 6. Wire to CI

Both subcommands use exit codes that gate cleanly:

- `nexus eval run` exits `1` on any failed case, `0` otherwise.
- `nexus eval baseline` exits `1` on a threshold breach
  (`fail_on_score_drop`, `fail_on_latency_p95_drop`, or any `pass→fail`
  flip), `0` otherwise.

A natural Makefile target:

```make
eval-deterministic:
	bin/nexus eval run
	bin/nexus eval baseline --against tests/eval/reports/baseline
```

Wire that into your CI's PR pipeline. The deterministic path needs no
LLM credentials — it replays journals.
