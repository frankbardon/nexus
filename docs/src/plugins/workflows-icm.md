# nexus.workflows.icm

File-driven multi-stage workflow runner. A workspace is a folder on disk: each
stage is a subfolder with a YAML-fronted contract, optional grounding,
optional skills, and a declared output. The plugin loads + validates the
workspace at boot, registers one `AgentPosture` per stage, and dispatches each
stage as a sub-agent via a private `delegate.Runtime` whenever an `io.input`
arrives.

> Source: `plugins/workflows/icm/`. New to ICM? Start with the
> [overview](../icm/overview.md) for the rationale + mental model, then
> follow the [end-to-end walkthrough](../icm/walkthrough.md) to build a
> workspace from scratch. This page is the field-by-field reference.
> Configuration table:
> [configuration reference](../configuration/reference.md#nexusworkflowsicm).

## Overview

ICM (Interpretable Context Methodology, Van Clief & McDermott,
[arXiv:2603.16021](https://arxiv.org/abs/2603.16021)) treats an LLM workflow
as a sequence of small, human-reviewable contracts rather than one big agent
loop. The Nexus implementation reduces a workspace to disk structure: the
loader parses contracts, compiles regex + gojq + JSON-schema at load time,
and the orchestrator dispatches stages strictly in folder-numeric order.

What the plugin does:

- Resolves `posture.registry` (provided by `nexus.agent.postures`) at boot.
- Loads + validates the workspace; aggregates errors so the user can fix in
  one pass.
- Derives one `AgentPosture` per stage and per verifier; tools, model role,
  budget, and operator prompt are baked in at registration.
- Subscribes to `io.input`. Each input begins a fresh run with its own
  `<runID>` directory under the plugin's data dir.
- Dispatches stages as sub-agents through a private `delegate.Runtime`.
  Stages never talk to one another — they read declared inputs and write
  declared outputs through the session helper.
- Emits a stage-level `plan.created` + `plan.progress` surface plus a
  richer `icm.*` event family for iteration / turn / fan-out detail.
- Routes every human checkpoint through `hitl.requested`; cancellation is
  surfaced via `hitl.cancel`.

What the plugin does NOT do:

- It does not directly call an LLM provider. Every model call routes through
  `delegate` (sub-agents) or through the configured judge posture (LLM
  predicates).
- It does not mutate the workspace. Artifacts live in the session directory.
- It does not auto-discover skills. Skills are loaded from the stage-local
  `skills/` and `shared/skills/` folders inside the workspace.

## Quick start

1. Add the plugin to `plugins.active` and point it at a workspace.

```yaml
plugins:
  active:
    - nexus.agent.postures
    - nexus.control.hitl
    - nexus.workflows.icm

  nexus.workflows.icm:
    workspace: ~/work/screenplay-pipeline
    default_judge_posture: judge_strict
    default_workflow_posture: workflow_base
```

2. Scaffold the workspace. Below is a working three-stage screenplay
   pipeline. Every file shown is necessary; everything else is optional.

```
~/work/screenplay-pipeline/
  workspace.md
  stages/
    01_outline/
      contract.md
    02_script/
      contract.md
    03_review/
      contract.md
```

`workspace.md` — required, non-empty. Describes the workflow at the highest
level. Surfaces to the operator prompt and to anyone reading the folder.

```markdown
# Screenplay Pipeline

A three-stage pipeline that converts a one-line premise into a short-form
screenplay: outline, draft, peer review. Each stage hands a single artifact
to the next; humans gate the start and end of every stage.
```

`stages/01_outline/contract.md` — YAML frontmatter then process body. The
body is rendered into the operator system prompt for this stage.

```markdown
---
display: Outline a three-act structure
turns:
  policy: fixed
  max: 1
human_gate: end
output:
  format: text
  filename: outline.md
inputs:
  artifacts:
    - 00_input/premise.txt
agent:
  model_role: writer
  budget:
    max_tokens: 4000
---

Read the premise in `<artifact path="00_input/premise.txt"/>`. Write a
three-act outline with beat counts per act. No prose. Markdown headings
allowed.
```

`stages/02_script/contract.md` — consumes the outline; loops up to three
times until the validator passes.

```markdown
---
display: Draft the screenplay
turns:
  policy: until_valid
  max: 3
human_gate: end
output:
  format: text
  filename: script.md
  validators:
    - type: native
      handler: word_count_over
      args: { min_words: 800 }
    - type: native
      handler: word_count_under
      args: { max_words: 2400 }
inputs:
  artifacts:
    - 01_outline/outline.md
agent:
  model_role: writer
  budget:
    max_tokens: 8000
---

Expand the outline in `<artifact path="01_outline/outline.md"/>` into a
screenplay between 800 and 2400 words. Use industry-standard formatting
(INT./EXT., character names in caps before dialogue).
```

`stages/03_review/contract.md` — judges the script with an LLM rubric.

```markdown
---
display: Peer review the screenplay
turns:
  policy: fixed
  max: 1
human_gate: none
output:
  format: text
  filename: review.md
  validators:
    - type: llm
      rubric: rubrics/review_quality.md
      model: judge_strict
inputs:
  artifacts:
    - 02_script/script.md
agent:
  model_role: reviewer
---

Read the script in `<artifact path="02_script/script.md"/>`. Produce a
review covering structure, character voice, dialogue, and pacing. Be
specific; cite scene numbers.
```

3. Start Nexus. Send the initial premise via `io.input` (the engine writes
   it to `<runID>/00_input/premise.txt`). ICM creates the run, dispatches
   stage 01, gates at its end via HITL, then continues.

## Workspace layout

The workspace is the source of truth for the workflow; artifacts are the
source of truth for the run. They live in different directories.

```
<workspace>/
  icm.yaml                  optional — layer name overrides + defaults
  operator.md               optional — Layer 0 operator prompt template
  operator.overlay.md       optional — appended to operator body
  workspace.md              required — high-level workflow description
  stages/
    01_<slug>/
      contract.md           required per stage
      grounding/            optional — reference files inline at dispatch
      skills/<name>/        optional — stage-local skills
    02_<slug>/...
  shared/
    grounding/              optional — cross-stage reference files
    skills/<name>/          optional — workspace-wide skills
  schemas/                  optional — JSON schemas referenced from contracts
  scripts/                  optional — command-predicate executables
  rubrics/                  optional — LLM-predicate rubric files
  verifiers/                optional — verifier stage definitions
  inputs/                   optional — initial-run input fixtures
```

Folder-name rules enforced by the loader:

- Stage folders match `^\d+_[a-z0-9_]+$`. Execution order is the numeric
  prefix, sorted numerically (so `9_x` runs before `10_x`).
- `00_input` is reserved — it names the synthetic input stage in artifact
  refs. Declaring a stage folder named `00_input` is a load error.
- Duplicate numeric prefixes (`05_foo` + `05_bar`) are rejected: execution
  order would be ambiguous.
- A stage folder containing an `artifacts/` subdirectory is rejected.
  Artifacts live in the session, not the workspace.

## icm.yaml

`icm.yaml` is optional. When present it overrides any subset of the layer
filenames and supplies workspace-level defaults that stages inherit unless
they override.

```yaml
# All sections are optional.
layer_names:
  operator: operator.md       # default
  workspace: workspace.md     # default
  contract: contract.md       # default
  grounding: grounding        # default (folder name)

defaults:
  turn_policy: fixed          # fixed | until_valid | until_human_approves
  human_gate: none            # none | start | end | both
  on_error: halt              # halt | retry | human_gate
  judge_posture: judge_basic  # used for type: llm predicates with no `model:`
  agent:
    posture: workflow_base    # base posture each stage inherits
    model_role: writer
    tools: [read_file]
    budget:
      timeout_seconds: 120
      max_tokens: 8000
      max_tool_calls: 16
    max_recursion_depth: 3

operator:
  overlay: |
    Always cite section numbers when referencing scripts.
```

The judge posture and base workflow posture can also be configured at the
plugin level (`default_judge_posture`, `default_workflow_posture`). The
plugin-level keys win when both are set.

## Stage contracts

Each stage is a single file: `contract.md`. It is YAML frontmatter (between
`---` lines) followed by a markdown body. The body is the stage's role
instructions — it is rendered into the operator system prompt at dispatch
time. Verifiers may be a single `.md` file directly under `verifiers/`
with the same shape.

### Frontmatter

| Field | Type | Default | Notes |
|-------|------|---------|-------|
| `id` | string | folder name | When set, must match folder name. |
| `display` | string | first body line or ID | Truncated to 80 chars. |
| `turns` | object | see Turns | Inner-loop policy. |
| `human_gate` | string | `none` | `none` / `start` / `end` / `both`. |
| `on_error` | string | `halt` | Non-validator failure policy. |
| `loop` | object | (none) | Convergence-driven iteration. |
| `fan_out` | object | (none) | Data-driven iteration. |
| `output` | object | (required) | Artifact + validators. |
| `inputs` | object | (none) | Files the stage reads. |
| `agent` | object | inherits | Posture + tools + budget. |
| `verifiers` | list | (none) | Cross-stage verifier IDs. |

### Output spec

`output` declares the file the stage writes and any validators that run
against it. Validators that fail under `turns.policy: until_valid` trigger
a retry; under any other policy they surface as
`icm.predicate.failed` and (depending on `on_error`) may halt the run.

```yaml
output:
  format: text                  # text (default) | json
  persist: file_ref             # file_ref (default) | context | both
  filename: outline.md          # required; no path separators
  schema: schemas/outline.json  # required when format: json
  validators:
    - type: regex
      pattern: '^# '
      anchor: first_line
      message: "Outline must begin with an H1."
```

`persist: file_ref` writes the artifact and references it by logical
ref downstream. `context` keeps the body in conversation context only
(use sparingly — large bodies bloat downstream turns). `both` does both.
JSON outputs always parse + validate against `schema` before the validators
run.

### Inputs

`inputs` declares what files the stage reads. The loader verifies each path
exists at boot.

```yaml
inputs:
  grounding:
    - style_guide.md            # under <stage>/grounding/
    - examples/sample_act_1.md
  shared_grounding:
    - house_voice.md            # under shared/grounding/
  artifacts:
    - 00_input/premise.txt      # initial input from io.input
    - 01_outline/outline.md     # prior stage's declared output
  skills:
    - markdown-screenplay       # resolved through skill precedence
```

Artifact refs are validated cross-stage at load: the referenced stage must
run before this one, and the filename must match that stage's
`output.filename`. The reserved `00_input/...` prefix points at files
copied into the session by `io.input` (or by `workspace_inputs_dir`).

Inline-vs-ref selection happens at dispatch: artifacts under
`inline_artifact_limit_bytes` (default 32 KiB) inline as `<artifact>`;
larger or binary content emits `<artifact_ref/>` and the LLM picks it up
via `read_file`.

### Agent block (posture, model_role, tools, budget)

Stage-level agent fields override workspace defaults; absent fields fall
through to defaults, then to the registry defaults at runtime.

```yaml
agent:
  posture: writer_posture       # optional; existence checked at runtime
  model_role: writer            # role into the engine model registry
  tools:                        # AllowedTools for this stage's posture
    - read_file
    - run_code
  prompt_overlay: |             # appended to operator body for this stage
    Be concise. Bullet lists over prose.
  budget:
    timeout_seconds: 120
    max_tokens: 8000
    max_tool_calls: 16
  max_recursion_depth: 3        # caps sub-agent nesting
```

`posture` is a *base* posture from `nexus.agent.postures`. ICM derives a
per-stage posture on top of it, layering operator prompt + role body +
overlay + tools + budget + skill-tool registration. The derived posture
name appears as `icm.<runID>.<stage_id>` in `icm.stage.started` events.

`auto_include_skill_reference_tool: true` (the default) appends the
`read_skill_reference` tool to any stage that declares `inputs.skills`.
For multi-instance setups the tool name becomes `read_skill_reference_<suffix>`
so two ICM instances can coexist in one engine without colliding.

### Turns

Turns control the inner loop within a single stage invocation.

```yaml
turns:
  policy: until_valid      # fixed | until_valid | until_human_approves
  max: 3                   # default 1 for fixed, 3 for until_valid
```

- `fixed` runs `max` turns unconditionally. Most common.
- `until_valid` retries while any validator fails. Requires at least one
  `output.validators` entry — the loader rejects the contract otherwise.
- `until_human_approves` loops while the human selects `continue` at the
  per-turn HITL gate (`icm.stage.turn`). Free-text feedback from the human
  arrives in the next turn's `<previous_attempt><human_feedback>`.

### Human gates

Human gates fire at stage boundaries. They are independent from per-turn
loop gates and per-iteration loop predicates.

```yaml
human_gate: end           # none (default) | start | end | both
```

`start` emits a HITL `icm.stage.start` action before the stage's first
dispatch; `end` emits `icm.stage.end` after the artifact is written. The
end gate offers a `restart` choice that wipes the stage directory and
re-runs (subject to `loop_max_restarts`).

For looping stages the gate fires at the bounds of the entire stage, not
per iteration. Use a `type: human` predicate inside `loop.until` if you
need per-iteration human review.

### Loops

Loops drive convergence: the entire stage runs as a fresh invocation each
iteration, with prior-iteration artifact + exit failures included in the
next payload.

```yaml
loop:
  max_iterations: 5
  until:
    - type: native
      handler: word_count_over
      args: { min_words: 1200 }
    - type: llm
      rubric: rubrics/coherence.md
  on_exhausted: human_gate     # human_gate (default) | error
```

`until` predicates run after each iteration's artifact is written. All
must pass for the loop to exit. When `max_iterations` runs out without
convergence, `on_exhausted: human_gate` raises an `icm.loop.exhausted`
HITL request offering `accept` / `restart` / `error`; `on_exhausted: error`
halts the stage immediately. Restart-loop count is bounded globally by the
plugin's `loop_max_restarts` config.

Iteration artifacts persist under `<stageDir>/iter_NN/`. The aggregate
`<stageDir>/<filename>` is written from the last iteration after
convergence (or the last attempt if a human accepts).

### Fan-out

Fan-out runs the stage once per item in a JSON list. Distinct from `loop`:
loops are convergence-driven; fan-outs are data-driven. They compose — a
stage with both fans out per item and each item independently iterates.

```yaml
fan_out:
  source: 02_research/topics.json   # earlier stage's JSON output
  jsonpath: .topics                 # optional gojq expression; default "."
  item_var: topic                   # name surfaced into payload's <fan_out_item>
  item_id: .slug                    # optional gojq for per-item folder name
  max_parallel: 4                   # default 1
  on_item_failure: continue         # continue (default) | halt
```

`source` must resolve to a JSON artifact produced by an earlier stage (or
to `00_input/...`). The orchestrator parses it, applies `jsonpath`, and
expects an array; non-arrays surface as a stage error. Each item writes
under `<stageDir>/items/<itemID>/<filename>`, and the orchestrator emits
an `icm.fanout.item` event at each item lifecycle boundary
(`active` → `completed` | `failed`).

The aggregate output filename is also written at the plain stage path so
downstream stages can reference it via the normal `<stage_id>/<filename>`
ref. Aggregation is a flat join of every item's artifact for text outputs
and a JSON array for JSON outputs.

### Verifiers

Verifiers are reusable stage-shaped contracts kept under `verifiers/`. A
stage references them by ID via the top-level `verifiers:` list. The
loader validates that every referenced ID exists. They run after the
stage's own validators and may declare their own posture, tools, and
predicates.

```yaml
# stages/03_review/contract.md
---
verifiers:
  - house_voice_check
  - structural_balance
---
```

```markdown
# verifiers/house_voice_check.md
---
display: House voice check
output:
  format: text
  filename: house_voice.md
  validators:
    - type: llm
      rubric: rubrics/house_voice.md
inputs:
  artifacts:
    - 02_script/script.md
---

Compare the script against the house style guide and flag violations.
```

## Predicates

A predicate is the unified shape used by `output.validators`, `loop.until`,
and verifier outputs. The `type` field discriminates; the loader compiles
regex / gojq / JSON-schema at load time and rejects malformed predicates
before boot completes.

### schema

JSON-schema validation against the stage output.

```yaml
- type: schema
  schema: schemas/outline.json
  name: outline_shape          # optional; default "<type>_<index>"
```

Path resolves against the workspace root. The loader reads the file,
parses it as JSON, and compiles it as draft-2020 to catch malformed
schemas at boot.

### regex

Regex match against text output.

```yaml
- type: regex
  pattern: '^FADE IN:'
  anchor: first_line           # whole (default) | first_line | last_line
  message: "Screenplay must open with FADE IN:."
```

The loader compiles the pattern at load time; failures surface in the load
error aggregate. `message` populates the predicate's `Feedback` field, so
LLMs retrying under `until_valid` see your authored guidance.

### native (builtin handlers)

Native predicates dispatch to a Go handler registered in the plugin. ICM
ships four built-ins.

| Handler | Required args | Optional args | Behavior |
|---------|---------------|---------------|----------|
| `word_count_under` | `max_words: int > 0` | — | Passes when `len(strings.Fields(artifact)) < max_words`. |
| `word_count_over` | `min_words: int >= 0` | — | Passes when `len(strings.Fields(artifact)) > min_words`. |
| `contains_required_ids` | `ids: []string` (non-empty) | `case_insensitive: bool` (default `false`) | Passes when every id appears at least once in the artifact. Empty `ids` is treated as malformed args, not vacuous truth. |
| `json_path_exists` | `path: string` (gojq query) | `must_be_non_empty: bool` (default `true`) | Parses artifact as JSON and runs the query. Passes when at least one result is returned and (when `must_be_non_empty`) at least one is non-null / non-empty. |

```yaml
- type: native
  handler: contains_required_ids
  args:
    ids: [PROTAGONIST, ANTAGONIST]
    case_insensitive: false
```

### command

Shell-out predicate. The script reads the artifact on stdin and exits 0 to
pass, non-zero to fail. Stderr becomes the feedback.

```yaml
- type: command
  run: scripts/lint_screenplay.sh
  timeout_seconds: 30          # optional; falls back to plugin default
```

The loader resolves `run` against the workspace root, verifies the file
exists, and verifies it has the executable bit set. Scripts run inside the
engine sandbox (the same `engine.Sandbox` injected into the plugin), so
the workflow's command surface is constrained by the host's sandbox
policy.

### llm

LLM judge predicate. The judge sees the artifact and a rubric, and returns
a structured verdict.

```yaml
- type: llm
  rubric: rubrics/coherence.md
  model: judge_strict          # optional posture override
  name: coherence_check        # optional
```

`model` names a registered posture (not a raw model). When omitted, the
plugin uses `default_judge_posture` from its config. Workspaces that use
`type: llm` predicates without setting either are rejected at runtime
with `default_judge_posture is not configured`.

The judge posture must return JSON conforming to a baked-in judge schema
(verdict + score + feedback). The plugin registers this schema at boot.

### human

In-loop human predicate. The orchestrator emits a HITL request and waits.
This is the right tool when "looks good" is the gating criterion for
convergence.

```yaml
- type: human
  prompt: "Does this iteration meet the brief?"
  require_feedback_on_continue: true   # optional
```

`continue` (without selecting the explicit pass / fail choice) under
`turns.policy: until_human_approves` advances to the next turn and routes
the human's free-text response into `<previous_attempt><human_feedback>`.

## Skills

Skills are bundles a stage can load at dispatch. Each skill is a folder
containing `SKILL.md` (YAML frontmatter `name` + `description`, then a
body) and an optional `references/` subfolder with deferred-load files.

Discovery is workspace-scoped — ICM does not use the global
`nexus.skills` `scan_paths`. The loader walks two locations in precedence
order:

1. `stages/<NN_slug>/skills/<name>/` — stage-local (wins on conflict).
2. `shared/skills/<name>/` — workspace-wide.

Reference shape:

```
shared/skills/markdown-screenplay/
  SKILL.md
  references/
    fade_transitions.md
    standard_formatting.md
```

`SKILL.md`:

```markdown
---
name: markdown-screenplay
description: How to format screenplays in markdown for downstream parsing.
---

Always use INT./EXT. headers. Capitalize character names before dialogue.
See references/standard_formatting.md for the full house spec.
```

Stages opt in via `inputs.skills`. At dispatch the orchestrator inlines
`SKILL.md` body into `<grounding>` and registers the
`read_skill_reference[_<suffix>]` tool so the agent can pull a specific
reference on demand. References are NOT inlined by default — that is the
point of progressive disclosure.

## XML payload reference

Each turn assembles a single XML user message. The shape is uniform across
stage modes; loop iterations and fan-out items add their own elements
but the surrounding skeleton is invariant. Inline blocks contain the
content; `_ref` variants point at filesystem paths.

```xml
<icm_turn stage="02_script" turn="1" iteration="3" run_id="r_abc">
  <grounding>
    <skill name="markdown-screenplay" source="workspace">
      <description><![CDATA[How to format screenplays...]]></description>
      <body><![CDATA[Always use INT./EXT. headers...]]></body>
      <references_available>
        <ref path="standard_formatting.md" description="House spec"/>
      </references_available>
    </skill>
    <file path="style_guide.md"><![CDATA[...]]></file>
    <shared_file path="house_voice.md"><![CDATA[...]]></shared_file>
  </grounding>
  <layer_data>
    <artifact path="01_outline/outline.md"><![CDATA[# Act I...]]></artifact>
    <artifact_ref path="01_outline/huge.json" size_bytes="48000"/>
    <fan_out_item key="topic"><![CDATA[{"slug":"act1","title":"Setup"}]]></fan_out_item>
  </layer_data>
  <previous_attempt turn="2">
    <output><![CDATA[FADE IN: ...]]></output>
    <validator_feedback>
      <failure name="word_count_over" type="native">
        word count 420 is not strictly greater than min_words=800
      </failure>
    </validator_feedback>
    <human_feedback><![CDATA[Tighten Act 2.]]></human_feedback>
  </previous_attempt>
  <previous_iteration index="2">
    <artifact path="02_script/iter_02/script.md"><![CDATA[...]]></artifact>
    <exit_failures>
      <failure name="coherence_check" type="llm">Act II drifts.</failure>
    </exit_failures>
  </previous_iteration>
  <instructions><![CDATA[Expand the outline...]]></instructions>
</icm_turn>
```

Notes:

- `_ref` elements appear when an artifact exceeds `inline_artifact_limit_bytes`,
  fails resolution (`missing="true"`), or contains non-UTF-8 bytes.
- Passing validator / exit-condition results are filtered out — the agent
  only sees actionable failures.
- The instructions block contains the stage contract body verbatim.

## Plan + progress events

ICM emits the engine's generic `plan.created` once at run start and
`plan.progress` after each stage transition, so generic UIs that render a
plan see the workflow without ICM-specific knowledge. On top of that, the
following typed events surface richer detail. All payloads carry a
`_schema_version` field and live in
`plugins/workflows/icm/icmtypes/types.go`.

| Event | When | Notes |
|-------|------|-------|
| `icm.run.started` | After workspace load + `plan.created`, before stage 1 dispatches. | Carries `run_id`, `instance_id`, `workspace_root`, `stages` count. |
| `icm.run.completed` | All stages finished without halt. | Includes `elapsed_seconds`. |
| `icm.run.halted` | Stage error policy halts, gate rejects, or run context cancels. | `cancelled: true` distinguishes ctx cancellation from gate reject. |
| `icm.stage.started` | Stage execution begins, before any `human_gate: start` gate. | Carries derived posture name + 1-based stage order. |
| `icm.stage.completed` | Artifact written + any end gate resolved. | Includes `iterations_run` + `convergence_failed` for looping stages. |
| `icm.stage.failed` | Dispatch error policy halts, gate rejects, or `loop.on_exhausted: error` fires. | Carries free-text `reason`. |
| `icm.stage.iteration` | Once per loop iteration, immediately before the iteration's invocation. | Includes prior iteration's `exit_failures`. |
| `icm.turn` | After each turn within an invocation. | Richer-UI feed only; basic UIs already see stage transitions via `plan.progress`. |
| `icm.fanout.item` | Item lifecycle boundary in a fan-out stage. | `Status` is `active` / `completed` / `failed`. |
| `icm.predicate.failed` | Any predicate evaluation returns `Verdict=false`. | Single source of truth for failure visibility — pass paths are not emitted. |
| `workflow.progress` | Run start, stage start, every iteration, every fan-out item completion, stage / run completion, halt / failure. | Engine-generic structured payload (`events.WorkflowProgress`). Powers the dedicated workflow status panel in `nexus.io.tui` and the indicator chip in `nexus.io.browser`. Emitted *alongside* the `icm.*` family, not in place of it. |

In addition, with `emit_progress_thinking_steps: true` (default) ICM emits
`thinking.step` events tagged `Phase="icm.<stage_id>"` so UIs that render
thinking surfaces show inline stage transitions without subscribing to the
typed event family.

### UI feedback surfaces

Both bundled IO plugins (`nexus.io.tui`, `nexus.io.browser`) wire ICM
progress directly so users see real-time feedback during long runs
without enabling extra observers:

- **Scrollback (thinking-step stream)** — every `icm.*` event is formatted
  into a one-line audit row via the helpers in
  `plugins/workflows/icm/icmtypes/format.go` and rendered alongside other
  thinking steps. Long runs leave a complete trail of stage transitions,
  iteration retries, predicate failures, and fan-out item ticks.
- **Dedicated workflow panel** — the generic `workflow.progress` event
  drives a sticky surface (TUI right-rail panel, browser chip indicator)
  that updates in place: workflow name, stage X/Y, iter N/M, turn N/M,
  fan-out items done/total, status badge, and the names of any predicate
  failures from the last iteration.

The two surfaces complement each other: scrollback for "what happened",
dedicated panel for "where are we now".

## Session layout + artifacts

Every run owns a directory under `<dataDir>/<runID>/`. `dataDir` is the
plugin's per-session data dir provided by the engine, so multiple
concurrent runs do not collide.

```
<engine_session>/plugins/<instance>/<runID>/
  .icm/
    run.json              run metadata: workspace path, started at, plan
    state.json            mutable per-stage progress (updated as stages run)
  00_input/
    premise.txt           copied from io.input.Content or workspace_inputs_dir
  01_outline/
    outline.md
    outline.md.icm.json   per-artifact sidecar: writer, validators, schema
  02_script/
    iter_01/
      script.md
      script.md.icm.json
    iter_02/
      script.md
      script.md.icm.json
    script.md             aggregate (last iteration after convergence)
    script.md.icm.json
  03_review/
    items/                fan-out items live here
      act1/
        review.md
      act2/
        review.md
    review.md             flat aggregate written for downstream refs
    review.md.icm.json
```

Logical refs (`<stage_id>/<filename>` in `inputs.artifacts` or
`fan_out.source`) resolve as follows:

1. Plain stage path wins when present.
2. Otherwise the highest-numbered `iter_NN/<filename>` wins (numeric sort —
   `iter_10` beats `iter_9`).
3. For fan-out stages, the aggregate at the plain stage path is what
   downstream stages see; per-item files are addressable only inside the
   fan-out itself.

`<instance>` is `nexus.workflows.icm` for the default instance, or the
instance-suffixed ID (`nexus.workflows.icm.script`, etc.) for additional
instances. The engine maps slashes in instance IDs to dots when computing
the on-disk directory.

## Multi-instance setup

Multiple ICM instances coexist in one engine via the `nexus.workflows.icm/<suffix>`
form. Each instance pins its own workspace, judge posture, and tool
namespace.

```yaml
plugins:
  active:
    - nexus.agent.postures
    - nexus.control.hitl
    - nexus.workflows.icm/script
    - nexus.workflows.icm/research

  nexus.workflows.icm/script:
    workspace: ~/work/screenplay-pipeline
    default_judge_posture: judge_strict
    default_workflow_posture: writer_base

  nexus.workflows.icm/research:
    workspace: ~/work/topic-research
    default_judge_posture: judge_basic
    cache_size: 64        # research is read-heavy; caching is safe here
    auto_include_skill_reference_tool: false
```

Per-instance details:

- The skill-reference tool name is namespaced per instance:
  `read_skill_reference_script`, `read_skill_reference_research`. Default
  instances (no suffix) get the unsuffixed `read_skill_reference`.
- Derived posture names embed the instance suffix so the registry stays
  unambiguous.
- Each instance carries a distinct `<runID>` namespace and a distinct
  data-dir prefix.
- The `icm_validate` LLM tool is similarly suffixed
  (`icm_validate_script`, `icm_validate_research`).

A single judge / human-gate plugin services all instances. The HITL
request's `RequesterPlugin` field carries the suffixed instance ID so
operators can route gates per workflow.

## Troubleshooting

**Workspace fails to load.**

ICM aggregates every validation error before returning, so the boot log
shows them as a bulleted list (`workspace load failed (N errors):`).
Common causes:

- Duplicate numeric prefixes (`01_outline` + `01_intro`) — execution order
  is ambiguous; renumber one.
- Stage folder named `00_input` — reserved name; rename.
- `output.filename` containing `/` — must be a bare filename; the loader
  builds the path.
- `inputs.artifacts` ref to a later stage — refs must point at the
  reserved `00_input` or at a stage with a lower numeric prefix.
- `output.format: json` without `output.schema` — JSON outputs always
  require a schema.
- `turns.policy: until_valid` with empty `output.validators` — without
  validators there is nothing to retry against.
- `type: command` predicate pointing at a non-executable script — `chmod
  +x` the file.

The aggregated error message includes the exact file path and line number
when applicable.

**Stage halts with `default_judge_posture is not configured`.**

A `type: llm` predicate ran and the plugin had no posture to dispatch the
judge against. Wire a judge in one of two ways:

```yaml
# Plugin-level fallback (every llm predicate without an explicit `model:`).
plugins:
  nexus.workflows.icm:
    default_judge_posture: judge_basic
```

```yaml
# Per-predicate override.
output:
  validators:
    - type: llm
      rubric: rubrics/quality.md
      model: judge_strict
```

The named posture must be registered with `nexus.agent.postures` (typically
via its `scan_dirs` config) before ICM boots.

**Loop never converges.**

`loop.max_iterations` is the per-pass cap. When it runs out under
`on_exhausted: human_gate` (the default), the human is offered `accept` /
`restart` / `error`. `restart` wipes the stage directory and reruns from
iteration 1. The plugin's `loop_max_restarts` caps the number of restarts
per run (default `3`, `0` for unlimited) so a never-converging workspace
cannot spin forever. If you see repeated restarts, the root cause is
usually:

- Exit conditions that the writer model cannot satisfy (rubric too tight,
  or contradicts an earlier rubric).
- Operator prompt that does not reference the prior-iteration failure
  block. The default operator template handles this; custom operators must
  read `<previous_iteration><exit_failures>` and address each failure.
- `on_exhausted: error` makes failures loud and immediate — useful in CI.

**Fan-out source doesn't resolve.**

The loader validates `fan_out.source` shape and that the named stage runs
earlier, but the artifact filename is checked against the prior stage's
declared `output.filename`. Two common mistakes:

- Filename typo (`topics.json` vs `topic.json`) — change one or the other.
- Source stage's `output.format` is `text`; fan-out always parses JSON.
  Make the producer JSON or change the consumer.

At runtime, if the source produces a non-array (or the `jsonpath` lands on
a non-array), the stage emits `icm.stage.failed` with a reason of
`fan_out.source did not resolve to an array`. Inspect the source artifact
in the session directory directly — the orchestrator never modifies it.

**Run cancelled mid-HITL.**

When the run context cancels while ICM is blocked on a HITL response, the
plugin emits `hitl.cancel` with the pending request ID so the UI can clear
its pending prompt. The run then completes with `icm.run.halted`,
`cancelled: true`.

## See also

- [configuration reference](../configuration/reference.md#nexusworkflowsicm)
  — every config key with defaults.
- [agent postures](agents/postures.md) — base postures stages inherit
  from; required at boot.
- [HITL](control.md#nexuscontrolhitl) — the registry ICM routes gates
  through.
- [skills](skills.md) — the engine-level skill machinery; ICM's per-workspace
  skills are independent but share the same `SKILL.md` shape.
- `plugins/workflows/icm/schema.json` — JSON schema for the config block.
- `plugins/workflows/icm/workspace/types.go` — full Go type model.
- `plugins/workflows/icm/icmtypes/types.go` — `icm.*` event payload structs.
