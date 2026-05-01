# Case Format

This page is the schema reference for the files inside a
`tests/eval/cases/<id>/` bundle. Conceptual background lives in
[`overview.md`](./overview.md). Top-level `eval:` keys live in
[`configuration/reference.md`](../configuration/reference.md#eval-harness).

## Directory layout

```
tests/eval/cases/<id>/
  case.yaml         # case metadata
  input/
    config.yaml     # engine config (typically mock provider)
    inputs.yaml     # scripted user inputs
  journal/          # session journal (header + events)
    header.json
    events.jsonl
  assertions.yaml   # deterministic + (Phase 5) semantic assertions
  _record/          # optional: build-tagged recorder for the journal
    main.go         # //go:build evalrecord
```

The directory's basename becomes the case ID. Hidden directories and
underscore-prefixed children (`_record`) are ignored by the discovery walk.

## `case.yaml`

| Key              | Type     | Required | Description |
|------------------|----------|----------|-------------|
| `name`           | string   | yes      | Human-readable case name. Loader rejects empty values. |
| `description`    | string   | no       | Multi-line free text; appears in summaries. |
| `tags`           | list     | no       | Strings used by `--tags` CLI filtering. Filter is a superset match. |
| `owner`          | string   | no       | Email or handle of the case owner — useful when promoting from real failures. |
| `freshness_days` | int      | no       | Soft hint that the journal should be re-recorded after N days. Not enforced in v1. |
| `model_baseline` | string   | no       | The model the journal was originally captured against (e.g. `mock`, `claude-sonnet-4-6`). Phase 5 uses this for cross-version drift gating. |
| `recorded_at`    | datetime | no       | ISO 8601 timestamp of last journal recording. |

Reference: `pkg/eval/case/case.go:46-54`.

## `input/config.yaml`

A standard Nexus engine config — same schema as any `configs/*.yaml`. Two
constraints worth noting:

- The runner overrides `core.sessions.root` automatically, so the
  case-supplied value is irrelevant in practice (the runner uses a
  per-run tempdir).
- The case must use mock-mode providers or real-credential-free configs.
  No `ANTHROPIC_API_KEY`, no `OPENAI_API_KEY` — replay short-circuits
  hide the actual provider call, but plugin construction must still
  succeed without one.

## `input/inputs.yaml`

```yaml
inputs:
  - "Why won't main.go build?"
  - "Show me the fixed version."
```

This file is the canonical record of the user side of the dialogue. The
runner does *not* read it directly during replay — the journal coordinator
re-emits the journaled `io.input` events instead. `inputs.yaml` exists for
human auditability and for Phase 3 promotion round-tripping.

## `journal/`

A drop-in copy of `~/.nexus/sessions/<id>/journal/`. `header.json` is
required (provides `schema_version` for compatibility checks); `events.jsonl`
contains the full event stream as JSONL envelopes. Rotated segments
(`events-NNN.jsonl.zst`) are supported transparently by the journal
reader.

For seed cases that are easier to hand-craft than to record, drop a
`//go:build evalrecord` recorder in `_record/main.go`:

```go
//go:build evalrecord

package main

import (
    "context"
    "time"
    "github.com/frankbardon/nexus/pkg/engine/journal"
    "github.com/frankbardon/nexus/pkg/events"
)

func main() {
    w, _ := journal.NewWriter("tests/eval/cases/<id>/journal", journal.WriterOptions{
        FsyncMode:  journal.FsyncEveryEvent,
        BufferSize: 16,
        SessionID:  "<id>-golden",
    })
    t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
    envs := []journal.Envelope{
        {Seq: 1, Ts: t0, Type: "io.session.start", Payload: map[string]any{...}},
        {Seq: 2, Ts: t0.Add(10 * time.Millisecond), Type: "io.input", Payload: events.UserInput{Content: "..."}},
        // ...
    }
    for i := range envs { w.Append(&envs[i]) }
    ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
    _ = w.Close(ctx)
}
```

Run with:

```sh
go run -tags evalrecord ./tests/eval/cases/<id>/_record/
```

## `assertions.yaml`

Top-level shape:

```yaml
deterministic:    # list of assertion entries
  - kind: <one of the 7 deterministic kinds>
    ...spec fields...
semantic: []      # reserved for Phase 5
```

Every entry must have a `kind` field. Unknown kinds error out at load time
— typos are loud, not silent.

### `event_emitted`

Pass when at least `Min` and at most `Max` envelopes match the type and
optional `where` payload filter. The default is "at least 1, no upper".

```yaml
- kind: event_emitted
  type: tool.invoke
  where:
    name: read_file
  count: { min: 1, max: 1 }
```

`where` does shallow equality on the payload (case-insensitive on field
names). Reference: `pkg/eval/case/assertions.go:36-49`.

### `event_count_bounds`

Per-event-type count range across the observed stream. Use this for
turn-frame expectations.

```yaml
- kind: event_count_bounds
  bounds:
    agent.turn.start: { min: 2, max: 2 }
    agent.turn.end:   { min: 2, max: 2 }
    io.input:         { min: 2, max: 2 }
```

Reference: `pkg/eval/case/assertions.go:71-74`.

### `event_sequence_strict`

Exact match against an ordered pattern, optionally filtered to a subset of
event types. The escape hatch — fragile across model upgrades; use sparingly.

```yaml
- kind: event_sequence_strict
  filter: [io.input, agent.turn.start, agent.turn.end, llm.response, tool.invoke, tool.result]
  pattern:
    - io.input
    - agent.turn.start
    - llm.response
    - tool.invoke
    - tool.result
    - llm.response
    - agent.turn.end
```

Reference: `pkg/eval/case/assertions.go:76-80`.

### `event_sequence_distance`

Levenshtein ratio (0.0 = identical, 1.0 = totally different) between the
observed and golden filtered streams. The permissive form of
`event_sequence_strict`; default-pick for trace-drift gating.

```yaml
- kind: event_sequence_distance
  threshold: 0.15
  filter: [io.input, agent.turn.start, agent.turn.end, llm.response, tool.invoke, tool.result]
```

Reference: `pkg/eval/case/assertions.go:53-58`.

### `tool_invocation_parity`

Per-tool count parity (within `count_tolerance`) and, when `arg_keys: true`,
that each tool's observed argument key set matches the golden's. Arg-value
parity is intentionally not checked — values vary across model upgrades.

```yaml
- kind: tool_invocation_parity
  count_tolerance: 0
  arg_keys: true
```

Reference: `pkg/eval/case/assertions.go:64-67`.

### `token_budget`

Caps tokens read off `llm.response` events. `per_turn: true` enforces the
limits per agent turn; otherwise they cap the session total.

```yaml
- kind: token_budget
  max_input_tokens: 1000
  max_output_tokens: 500
  per_turn: false
```

Reference: `pkg/eval/case/assertions.go:84-88`.

### `latency`

Caps p50/p95 turn latency, computed from event timestamp deltas (not
wall-clock). A turn = `agent.turn.start` … `agent.turn.end`.

```yaml
- kind: latency
  p50_ms: 500
  p95_ms: 2000
```

Reference: `pkg/eval/case/assertions.go:92-95`.

## Authoring tips

- **Start broad, tighten over time.** The first version of a case typically
  uses `event_count_bounds` plus `event_emitted` for the load-bearing
  events. Add `event_sequence_distance` once you've seen one or two clean
  runs.
- **Filter ruthlessly.** `event_sequence_*` assertions over an unfiltered
  stream are fragile because tick events, status updates, and thinking
  steps slip in and out of runs. Filter to the events you actually care
  about.
- **Know what re-emits.** Side-effecting events (`llm.response`,
  `tool.result`) are stashed and re-emitted from the stash; live-emitted
  events (`agent.turn.*`, `plan.*`, `tool.invoke`, `skill.discover`) are
  generated fresh every replay. Boot-time emissions (e.g. capability
  registration, `tool.register`) fire live too. Anything that depends on
  a side-effect failure (`provider.fallback`, retry events) will *not*
  re-fire — assert on the success path or move that case to the live
  integration test suite.
- **Tags are a superset filter.** `--tags react,mock` matches a case
  tagged `[react, mock, planner]` but skips `[react]` alone. Tag
  generously.
