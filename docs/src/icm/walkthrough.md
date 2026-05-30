# ICM Workflows — End-to-End Walkthrough

This page builds a working ICM workspace from an empty folder, runs it
through Nexus, and shows you exactly what to expect at each step: the
events that fire, the files that land on disk, the human gates that pause
the run, and the failure modes that bite first-time authors.

The example is a **two-stage research brief pipeline**. Stage 1 produces an
outline from a topic; stage 2 expands the outline into a brief, looping
until the brief passes a word-count rubric. It is deliberately small enough
to fit on one screen and deliberately rich enough to exercise loops,
predicates, human gates, and the full event surface.

By the end of this walkthrough you will have:

- A working workspace under `~/work/research-brief/`.
- A Nexus config that wires posture + delegate + HITL + ICM.
- A `bin/nexus` session that runs the workflow end-to-end against a real
  LLM provider.
- Familiarity with the exact `icm.*` events that drive any UI you build on
  top.

If you only want to read the surface area, the
[overview](./overview.md) is the conceptual map and the
[plugin reference](../plugins/workflows-icm.md) is the field-by-field
manual. This page sits between them.

## Prerequisites

You need:

1. A built `bin/nexus`. From the repo root: `make build`.
2. An LLM provider API key in env or a `.env` file. The walkthrough uses
   `ANTHROPIC_API_KEY`; switch to `OPENAI_API_KEY` or `GEMINI_API_KEY` if
   you prefer — the workspace itself is provider-agnostic because every
   stage names a `model_role`, not a raw model.
3. A place to put the workspace. The walkthrough uses
   `~/work/research-brief/`; adjust paths as needed.

You do **not** need an existing posture library. Step 1 below ships a
minimal two-posture file you can copy into place.

## Step 1 — Provision postures

ICM derives one posture per stage at boot, but each derived posture inherits
from a **base posture** registered with `nexus.agent.postures`. Wire a
minimal two-posture library so the workflow has somewhere to stand:

```bash
mkdir -p ~/.nexus/postures
```

`~/.nexus/postures/writer_base.yaml`:

```yaml
name: writer_base
description: Long-form writing posture for ICM stages.
system_prompt: |
  You are a writer. Follow the stage contract exactly. Produce one
  artifact in the format the contract specifies. Do not add
  commentary outside the artifact.
allowed_tools: []
model:
  model_role: writer
default_budget:
  timeout: 180s
  max_tokens: 8000
```

`~/.nexus/postures/judge_basic.yaml`:

```yaml
name: judge_basic
description: JSON-verdict judge for ICM `type: llm` predicates.
system_prompt: |
  You judge an artifact against a rubric and return a JSON verdict.
  Output only JSON conforming to the icm.judge.response schema.
allowed_tools: []
model:
  model_role: judge
default_budget:
  timeout: 60s
  max_tokens: 2000
```

The names matter — the workspace references them by name. The `model_role`
fields route to whatever your `core.models` config maps `writer` and
`judge` to. A single-provider setup with one model is fine; map both roles
to the same model entry.

## Step 2 — Author the workspace

Create the workspace folder. Every path inside is required unless flagged
optional.

```bash
mkdir -p ~/work/research-brief/stages/01_outline
mkdir -p ~/work/research-brief/stages/02_brief
mkdir -p ~/work/research-brief/rubrics
```

### Workspace brief

`~/work/research-brief/workspace.md`:

```markdown
# Research Brief Pipeline

A two-stage research pipeline. Stage 1 reads a topic and produces a
five-point outline. Stage 2 expands the outline into a brief between
600 and 1200 words. The brief loops until a writer-judge agrees it
covers every outline point.
```

Required, non-empty. Shows up at the top of every operator prompt so the
stage agent knows where it sits in the larger workflow.

### Workspace defaults

`~/work/research-brief/icm.yaml` is optional but useful for shared
defaults:

```yaml
defaults:
  agent:
    posture: writer_base
    model_role: writer
  judge_posture: judge_basic
```

Anything a stage doesn't override falls back here, then to plugin defaults.

### Stage 1 — outline

`~/work/research-brief/stages/01_outline/contract.md`:

```markdown
---
display: Outline the brief in five bullets
turns:
  policy: fixed
  max: 1
human_gate: end
output:
  format: text
  filename: outline.md
  validators:
    - type: native
      handler: contains_required_ids
      args:
        ids: ["1.", "2.", "3.", "4.", "5."]
inputs:
  artifacts:
    - 00_input/topic.txt
---

Read the topic in `<artifact path="00_input/topic.txt"/>`. Produce a
markdown outline with exactly five numbered points. Each point is one
sentence. No prose around the list.
```

What this does:

- `turns.policy: fixed, max: 1` — one LLM call, no retry.
- `human_gate: end` — pause after the artifact is written so the human can
  approve before stage 2 starts.
- `contains_required_ids` validator — refuses any output that drops one of
  the five numbered points. Without `until_valid` semantics this just
  surfaces as `icm.predicate.failed`; we keep it for visibility.
- `inputs.artifacts: 00_input/topic.txt` — the reserved `00_input` namespace
  is where `io.input` content lands.

### Stage 2 — brief

`~/work/research-brief/stages/02_brief/contract.md`:

```markdown
---
display: Expand the outline into a 600-1200 word brief
turns:
  policy: until_valid
  max: 3
human_gate: end
output:
  format: text
  filename: brief.md
  validators:
    - type: native
      handler: word_count_over
      args:
        min_words: 600
    - type: native
      handler: word_count_under
      args:
        max_words: 1200
loop:
  max_iterations: 3
  until:
    - type: llm
      rubric: rubrics/coverage.md
  on_exhausted: human_gate
inputs:
  artifacts:
    - 01_outline/outline.md
agent:
  budget:
    max_tokens: 4000
---

Expand the outline in `<artifact path="01_outline/outline.md"/>` into a
research brief between 600 and 1200 words. Cover every outline point.
Use a brief introduction, one paragraph per point, and a one-sentence
conclusion. Markdown allowed.
```

This stage exercises three loops at once:

- **Turns** — `until_valid` retries up to 3 times within an iteration if
  the word-count validators fail. The retry payload includes the prior
  attempt's failure feedback.
- **Iterations** — `loop.max_iterations: 3` reruns the entire stage if the
  `coverage.md` LLM rubric is not satisfied. Each iteration writes its
  artifact to `02_brief/iter_NN/brief.md`.
- **Human gate** — `human_gate: end` pauses after the loop exits so the
  human can approve, restart, or fail.

### Rubric

`~/work/research-brief/rubrics/coverage.md`:

```markdown
The brief passes coverage when every numbered point in the outline is
addressed in its own paragraph. A point that is mentioned but not
expanded is a failure. Return JSON only.
```

The judge posture (`judge_basic`) sees this rubric plus the candidate
artifact and must return JSON conforming to the baked-in
`icm.judge.response` schema (`verdict` + `score` + `feedback`).

## Step 3 — Wire the Nexus config

`~/work/research-brief.yaml`:

```yaml
core:
  models:
    writer:
      provider: anthropic
      model: claude-sonnet-4-6
    judge:
      provider: anthropic
      model: claude-sonnet-4-6

plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.postures
    - nexus.agent.delegate
    - nexus.control.hitl
    - nexus.memory.capped
    - nexus.workflows.icm

  nexus.agent.postures:
    scan_dirs:
      - ~/.nexus/postures

  nexus.workflows.icm:
    workspace: ~/work/research-brief
    default_judge_posture: judge_basic
    default_workflow_posture: writer_base
```

Note what's **not** here: no `nexus.agent.react`, no
`nexus.agent.orchestrator`, no separate planner. ICM is itself the agent
loop — when `io.input` arrives it owns the conversation until the run
completes.

## Step 4 — Run it

Start the TUI:

```bash
bin/nexus -config ~/work/research-brief.yaml
```

At the prompt, type a topic — `the economics of mechanical keyboards`,
say — and hit enter.

You will see this sequence of events. The TUI renders the plan progress
inline; richer UIs subscribe to the typed events documented in the
[plugin reference](../plugins/workflows-icm.md#plan--progress-events).

```
io.input                            { content: "the economics of mechanical keyboards" }
plan.created                        { steps: 2 (01_outline, 02_brief) }
icm.run.started                     { run_id: r_<id>, stages: 2 }
plan.progress                       { step: 01_outline, status: active }
icm.stage.started                   { stage_id: 01_outline, order: 1 }
llm.request → llm.response          (stage 1 turn 1)
icm.turn                            { stage_id: 01_outline, turn: 1 }
hitl.requested                      { action: icm.stage.end, stage_id: 01_outline }
```

The run pauses at `hitl.requested`. The TUI surfaces a prompt with three
choices: **continue**, **restart this stage**, **abort the run**. Press
**continue**.

```
hitl.responded                      { action: icm.stage.end, choice: continue }
plan.progress                       { step: 01_outline, status: completed }
icm.stage.completed                 { stage_id: 01_outline, artifact_path: ... }
plan.progress                       { step: 02_brief, status: active }
icm.stage.started                   { stage_id: 02_brief, order: 2 }
icm.stage.iteration                 { iteration: 1, max: 3 }
llm.request → llm.response          (stage 2 iter 1 turn 1)
```

If stage 2's first attempt is under 600 words, the `until_valid` retry
fires inside the same iteration. If three turns can't satisfy the word
count, the iteration writes its best attempt to `02_brief/iter_01/brief.md`
and the LLM judge runs against the `coverage.md` rubric. If the judge says
no, iteration 2 starts.

When the loop exits — convergent or exhausted — `human_gate: end` fires
again. Approve, and:

```
icm.stage.completed                 { stage_id: 02_brief, iterations_run: 2, convergence_failed: false }
icm.run.completed                   { stages_run: 2, elapsed_seconds: 47 }
```

## Step 5 — Inspect the run

Every artifact, every iteration, and every per-artifact sidecar lives in
the engine session under
`<session>/plugins/nexus.workflows.icm/<runID>/`:

```
~/.nexus/sessions/<sid>/plugins/nexus.workflows.icm/r_<runID>/
  .icm/
    run.json              # workspace path, started at, plan
    state.json            # per-stage progress
  00_input/
    topic.txt             # what you typed
  01_outline/
    outline.md
    outline.md.icm.json   # sidecar: writer + validator results
  02_brief/
    iter_01/
      brief.md
      brief.md.icm.json
    iter_02/
      brief.md
      brief.md.icm.json
    brief.md              # aggregate (last iteration after convergence)
    brief.md.icm.json
```

The sidecar files (`.icm.json`) record which posture wrote the artifact
and which validators passed or failed. They are how downstream tooling
can answer *"why did stage 2 take two iterations?"* without re-reading the
artifact.

You can re-run the workflow on a different topic by sending another
`io.input`. ICM creates a fresh `r_<runID>` directory; prior runs are
untouched.

## Step 6 — Watch a failure

Edit `~/work/research-brief/stages/01_outline/contract.md` and change
`filename: outline.md` to `filename: outline/v1.md`. Restart Nexus.

```
workspace load failed (1 errors):
  stages/01_outline/contract.md: output.filename must not contain path
  separators (got "outline/v1.md")
```

ICM aggregates every validation error into one message at boot so you can
fix all of them in a single pass instead of restart-fix-restart cycles.
Revert the change and ICM boots cleanly.

Other failure modes worth provoking once for muscle memory:

- Duplicate stage prefix (`01_outline` + `01_summary`) — boot fails with
  *"duplicate stage prefix 01"*.
- A `type: llm` predicate with no `default_judge_posture` configured —
  runtime fails the predicate with *"default_judge_posture is not
  configured"*.
- An `inputs.artifacts` ref to a later stage (`02_brief/...` inside
  `01_outline/contract.md`) — boot fails at load.

Each failure surfaces with the workspace-relative path + line number.

## Step 7 — Iterate the workspace

The workflow you just built is the floor. Everything else ICM offers is
incremental:

- **Add a fan-out stage.** Replace the brief stage's monolithic output with
  one paragraph per outline point. Produce a `topics.json` from stage 1
  and `fan_out.source: 01_outline/topics.json` from stage 2.
- **Add a `type: command` predicate.** Drop a `scripts/lint_brief.sh` into
  the workspace, mark it executable, and reference it from stage 2's
  validators. The script reads the artifact on stdin and exits 0/non-0.
- **Add skills.** Put a `shared/skills/house_voice/SKILL.md` under the
  workspace and reference it from `stages/02_brief/contract.md` via
  `inputs.skills: [house_voice]`. The skill body inlines into the
  grounding; references load on demand via the `read_skill_reference`
  tool.
- **Add verifiers.** Move the rubric check out of stage 2's loop and into
  a reusable `verifiers/coverage.md`. Stages that need coverage reference
  it via top-level `verifiers: [coverage]`.

Each is one or two contract edits. The walkthrough deliberately stops
short of all of them so the on-ramp stays short; the
[plugin reference](../plugins/workflows-icm.md) covers each in detail.

## Using the workspace builder skill

If hand-authoring contracts feels tedious, the
`.claude/skills/icm-workspace-builder/SKILL.md` skill interviews you for
the workflow shape and writes the workspace for you. Invoke it via the
configured Claude Code skill surface — the skill validates each answer
against the same loader rules above and produces a workspace that boots
cleanly on the first try.

## What you proved

- A non-trivial agent workflow — two stages, loop convergence, an LLM
  judge, native validators, a human gate, and a `00_input` initial
  artifact — fit in **five files under 100 lines**.
- The same workflow runs in any IO surface Nexus supports (TUI, browser,
  oneshot, wails) without a code change. Adding a new IO means activating
  a new plugin, not modifying the workspace.
- The `icm.*` event surface plus the engine-generic `plan.*` events give
  any UI everything it needs to render workflow progress, including
  fan-out items and loop iterations.
- The workspace is source-control-friendly — every change to the workflow
  is a markdown / YAML diff you can review on a PR.

## Where to go next

- **[Overview](./overview.md)** — the rationale, the mental model, the
  full list of orthogonal features.
- **[Plugin reference](../plugins/workflows-icm.md)** — every contract
  field, predicate type, event payload, and troubleshooting case.
- **[Configuration reference](../configuration/reference.md#nexusworkflowsicm)**
  — every plugin config key with defaults.
- **[Postures](../plugins/agents/postures.md)** — the base postures stages
  inherit from.
- **[HITL](../plugins/control.md#nexuscontrolhitl)** — how human gates
  route.
