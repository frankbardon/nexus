# Embedding: Extra Plugins, Live Runs & Custom Scores

This page is for a caller that drives `pkg/eval` directly from Go — an
embedder building its own eval CLI or CI job on top of the harness,
rather than using `nexus eval run`/`baseline` as shipped. It covers four
`pkg/eval` capabilities that only exist at the Go API level today:

- **`ExtraPlugins`** — register your own (non-`nexus.*`) plugin factories
  onto a case's engine, so a case can exercise a plugin that only your
  embedding module knows about.
- **`runner.RunLive`** — run a case's inputs against a real, live engine
  boot instead of `runner.Run`'s replay-from-journal path, and get back a
  rendered transcript.
- **`response_contains` / `contains_value`** — two deterministic
  assertion kinds for cases that assert on response *text*, not just
  event shape. (`ExtraPlugins` and these two return to
  [`case-format.md`](./case-format.md), which is where the other seven
  deterministic kinds live — see that page for those.)
- **`CustomScores`** — attach an arbitrary named score (a judge verdict,
  a domain-specific correctness check, anything you compute) to a
  `runner.Result`, and have it flow through `report.Aggregate` and gate
  `baseline.Decide` alongside the built-in latency/pass-rate metrics.

None of this requires touching `pkg/eval` source. All four are exported
seams on `runner.Options`, `runner.Result`, `evalcase.Assertion`, and
`baseline.Thresholds`.

## Registering extra plugins (`ExtraPlugins`)

`runner.Options.ExtraPlugins` is a `map[string]engine.PluginFactory`.
Every `Run` and `RunLive` call registers these onto the case's engine
*after* the built-in plugins (`allplugins.RegisterAll`), so an ID that
collides with a built-in silently wins — that's deliberate, not a bug,
and lets an embedder shadow a built-in for testing. Registering an ID
here only makes the factory available to the engine's registry; the
case's own `plugins.active` list must still name the ID for the engine
to construct and `Init` it during boot, exactly like any built-in
plugin.

A minimal example plugin (full `engine.Plugin` interface — see
[Creating a Custom Plugin](../skills/custom-plugin.md) for the deeper
authoring guide):

```go
package acmemetrics

import (
	"context"

	"github.com/frankbardon/nexus/pkg/engine"
)

const PluginID = "acme.metrics"

type Plugin struct{}

func New() engine.Plugin { return &Plugin{} }

func (p *Plugin) ID() string                                { return PluginID }
func (p *Plugin) Name() string                              { return "Acme Metrics" }
func (p *Plugin) Version() string                           { return "0.1.0" }
func (p *Plugin) Dependencies() []string                    { return nil }
func (p *Plugin) Requires() []engine.Requirement            { return nil }
func (p *Plugin) Capabilities() []engine.Capability         { return nil }
func (p *Plugin) Init(ctx engine.PluginContext) error       { return nil }
func (p *Plugin) Ready() error                              { return nil }
func (p *Plugin) Shutdown(ctx context.Context) error        { return nil }
func (p *Plugin) Subscriptions() []engine.EventSubscription { return nil }
func (p *Plugin) Emissions() []string                       { return nil }
```

Wire it into a case run through `runner.Options`:

```go
res, err := runner.Run(ctx, c, runner.Options{
	ExtraPlugins: map[string]engine.PluginFactory{
		acmemetrics.PluginID: acmemetrics.New,
	},
})
```

The case's `input/config.yaml` must list the ID under `plugins.active`
for the engine to actually boot it:

```yaml
plugins:
  active:
    - nexus.llm.anthropic
    - nexus.agent.react
    - acme.metrics
```

`ExtraPlugins` is honored identically by `Run` and `RunLive` — both route
through the same `bootEngine` helper, so there is exactly one call site
that ever registers it.

## Live runs (`RunLive`) and the rendered transcript

`runner.Run` replays a case's *golden journal* — no real LLM call, no API
key. `runner.RunLive` is a structurally different, third execution path:
it overlays the case's config so a live `nexus.io.test` transport fires
`c.Inputs` for real against a live engine boot (a real LLM call, unless
the case's own `nexus.io.test.mock_responses` scripts the response — the
same mocking pattern the deterministic path's cases use), then waits for
natural completion.

```go
res, err := runner.RunLive(ctx, c, runner.Options{
	SessionsRoot: t.TempDir(), // or "" to accept the case-config default
})
if err != nil {
	// ...
}
```

`RunLive` returns the same `*runner.Result` shape as `Run` — including
`Assertions` evaluated against the live-observed stream, so
`event_sequence_distance`/`tool_invocation_parity` still catch structural
drift even though the LLM call itself isn't replayed from a stash. What's
new is `Result.Transcript`: a rendered, legible plain-text rendering of
the *entire, unfiltered* observed event stream — every event, in order,
as a numbered `[n] <RFC3339 timestamp> <type>` header line followed by
its payload as indented JSON. See
[`pkg/eval/runner/transcript.go`](https://github.com/frankbardon/nexus/blob/main/pkg/eval/runner/transcript.go)
for `RenderTranscript`'s exact rendering rules — v1 is deliberately
unfiltered and untruncated; don't add filtering client-side without
first checking whether that source has since changed.

### Feeding the transcript to an LLM judge

`Result.Transcript` exists primarily to be handed to `protocol.Run` (the
existing, unmodified inspect-mode Go entry point documented at
[`inspect-protocol.md`](./inspect-protocol.md)) as `Request.UserInput`,
alongside a rubric, for LLM-judge scoring. **This is not a new judge
mechanism** — `protocol.Run` already exists and is untouched; the only
new thing is a convenient way to produce the transcript that becomes the
judge's input. (Note this is also distinct from `nexus eval run --full`,
which remains a stubbed CLI mode — see [`overview.md`](./overview.md#future-phases).
The pattern below is a Go-API judge call you wire up yourself.)

```go
// 1. RunLive the case under test.
subjectRes, err := runner.RunLive(ctx, c, runner.Options{SessionsRoot: sessionsRoot})
if err != nil {
	// ...
}

// 2. Build a judge request: a rubric + the rendered transcript as
// UserInput, against a config naming a judge model and a structured-
// output schema (nexus.gate.json_schema — the same belt-and-suspenders
// pattern as docs/src/guides/structured-output.md, scenario 5).
const rubric = "You are grading an AI assistant's transcript against a " +
	"rubric: did the assistant correctly answer the user's question? " +
	"Respond with ONLY a JSON object matching the required schema."

req := &protocol.Request{
	Schema:       protocol.SchemaVersion,
	ConfigInline: judgeConfigYAML, // names nexus.gate.json_schema + a judge model role
	UserInput:    rubric + "\n\n" + subjectRes.Transcript,
}

// 3. Run the judge through the real, unmodified protocol.Run.
resp, err := protocol.Run(ctx, req)
if err != nil {
	// ...
}

// 4. The verdict is the existing, unmodified
// Response.FinalAssistantMessage field — parse it as whatever schema the
// judge config's json_schema gate enforces.
var verdict struct {
	Score     float64 `json:"score"`
	Reasoning string  `json:"reasoning"`
}
_ = json.Unmarshal([]byte(resp.FinalAssistantMessage), &verdict)
```

This is exactly the pattern proven end to end by
[`pkg/eval/protocol/e2e_judge_test.go`](https://github.com/frankbardon/nexus/blob/main/pkg/eval/protocol/e2e_judge_test.go)
(`TestE2E_RunLiveTranscriptFeedsJudge`) — read that test for a complete,
runnable, mock-mode version of both the subject case and the judge
config (no real LLM call, no API key, no network access). Both the
subject run and the judge run in that test are driven entirely by
`nexus.io.test`'s `mock_responses`.

## `response_contains` and `contains_value`

Two new deterministic assertion kinds, alongside `case-format.md`'s
existing seven, for asserting on the *text content* of a response rather
than event shape or counts. Both default to searching the **final
assistant response** — the last `llm.response` event with no pending
tool calls and non-empty content — unless `event_type` names a different
event to search instead.

### `response_contains`

Passes when the selected text contains every string in `contains`
(if given) and at least one string in `contains_any` (if given).
Matching is ASCII case-insensitive. At least one of `contains`/
`contains_any` is required.

```yaml
- kind: response_contains
  contains: ["revenue", "$4.2M"]
```

```yaml
- kind: response_contains
  contains_any: ["chart", "graph", "table"]
```

Redirect the search to a non-default event:

```yaml
- kind: response_contains
  event_type: io.output
  contains: ["Revenue Summary"]
```

### `contains_value`

Passes when the selected text contains at least one numeric token within
`tolerance` of `value`. Extraction is word-boundary-aware — searching for
`4.2` never matches inside `24.2` — and thousands-separator commas and a
`unit` string (e.g. `"%"`, `"$"`) are stripped before numbers are
extracted. The tolerance boundary is inclusive: a candidate exactly
`tolerance` away from `value` passes.

```yaml
- kind: contains_value
  value: 42.3
  unit: "%"
```

**Tolerance inference.** When `tolerance` is omitted (or explicitly `0`
— the two are indistinguishable, since Go's zero value for `float64` is
`0`), it defaults to half the smallest decimal unit implied by how many
digits were written after `value`'s decimal point:

| `value` | inferred `tolerance` |
|---|---|
| `42.3` | `0.05` |
| `100` | `0.5` |
| `3.14` | `0.005` |

Set `tolerance` explicitly whenever that default isn't what you want —
e.g. `value: 42.30` infers the *same* default as `value: 42.3` (both are
the float64 `42.3`; YAML doesn't preserve trailing-zero precision), so
if the distinction between "accurate to one decimal" and "accurate to
two" matters for your case, say so with an explicit `tolerance`:

```yaml
- kind: contains_value
  value: 42.30
  tolerance: 0.005
```

## Custom scores → baseline gating

`runner.Result.CustomScores` is a `map[string]float64` that neither `Run`
nor `RunLive` ever populates — it's purely caller-set. Compute whatever
you want after a run (a judge verdict from the pattern above, a
domain-specific correctness check, anything scoreable as a float) and set
it on the `Result` you already hold before it goes into a report:

```go
res, err := runner.RunLive(ctx, c, runner.Options{SessionsRoot: sessionsRoot})
// ... run the judge as shown above, parse verdict.Score (0-10) ...
res.CustomScores = map[string]float64{
	"judge_score": verdict.Score / 10.0, // normalize to 0-1, your convention
}
```

`report.Aggregate` copies `CustomScores` straight onto the matching
`report.CaseEntry.CustomScores` — no extra wiring needed:

```go
rep := report.Aggregate("full", []*runner.Result{res})
// rep.Cases[0].CustomScores == map[string]float64{"judge_score": 0.9}
```

`baseline.Compute` then derives `CaseDelta.CustomScoreDeltas` — candidate
minus baseline, for the union of both sides' `CustomScores` keys (a name
present on only one side is treated as zero on the missing side, same
convention as the existing `TokensDelta`/latency fields):

```go
diff, err := baseline.Compute(againstReport, freshReport)
// diff.Cases[i].CustomScoreDeltas["judge_score"] == fresh - against
```

Finally, `baseline.Thresholds.FailOnCustomScoreDrop` gates on it exactly
like the existing latency/pass-rate thresholds: a `map[string]float64`
keyed by the same custom-score name, where each value is the absolute
drop (baseline minus candidate) that fails the run for that name. A name
absent from the map isn't gated at all; zero or negative disables that
name's gate.

```go
thresholds := baseline.Thresholds{
	FailOnCustomScoreDrop: map[string]float64{
		"judge_score": 0.1, // a drop of 0.1 or more fails
	},
}
shouldFailCI := diff.Decide(thresholds) // also sets diff.Breached.CustomScoreDrop
```

**Important — Go API only, not a CLI flag today.** Unlike
`fail_on_score_drop`/`fail_on_latency_p95_drop`, which `nexus eval
baseline` reads out of `eval.baseline` config keys (see
[`configuration/reference.md`](../configuration/reference.md#eval-harness)),
`FailOnCustomScoreDrop` has **no** `nexus eval baseline` CLI/config
wiring yet — `cmd/nexus/eval.go`'s `baseline.Thresholds{...}` construction
only sets `FailOnScoreDrop`/`FailOnLatencyP95Drop` from flags/config. To
use `FailOnCustomScoreDrop` today, call `baseline.Compute`/`Decide`
yourself from Go (as above), the way this whole page's four capabilities
assume you're driving `pkg/eval` from your own embedding code rather than
the `nexus eval` binary.

## See also

- [Case Format](./case-format.md) — the other seven deterministic
  assertion kinds, and the on-disk case bundle shape.
- [Inspect-Mode Protocol](./inspect-protocol.md) — the `protocol.Run`
  wire format used by the judge pattern above.
- [Creating a Custom Plugin](../skills/custom-plugin.md) — the full
  `engine.Plugin` authoring guide, for `ExtraPlugins` entries more
  involved than the stub above.
- `pkg/eval/protocol/e2e_judge_test.go` — the real, runnable, mock-mode
  worked example this page's judge pattern is based on.
