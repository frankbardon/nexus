# ICM Workflows — Overview

Nexus ships an agent loop for **file-driven multi-stage workflows**:
`nexus.workflows.icm`. The shorthand for what it does is short — a folder on
disk is your workflow — but the consequences are worth a section of its own.
This page makes the case for the feature and gives you the mental model
needed to read the [walkthrough](./walkthrough.md) and the
[plugin reference](../plugins/workflows-icm.md) without getting lost.

## What is ICM?

ICM is short for **Interpretable Context Methodology** (Van Clief &
McDermott, [arXiv:2603.16021](https://arxiv.org/abs/2603.16021)). The
methodology argues that production LLM workflows should be assembled out of
**small, human-reviewable contracts** rather than handed off to one large,
opinionated agent loop. Each step has a written brief, declared inputs, a
single declared output, and validators that decide whether the step is
done. The methodology is paper-shaped — the Nexus plugin gives it a runtime.

The plugin reduces the methodology to a single rule: **the workflow is a
folder on disk**. Stages are subfolders. Contracts are markdown files with
YAML frontmatter. Predicates are inline. Skills are subfolders. Artifacts
land in the session directory at run time, never in the workspace.

```
~/work/screenplay-pipeline/
  workspace.md              # the brief — required, non-empty
  stages/
    01_outline/contract.md  # YAML frontmatter + body
    02_script/contract.md
    03_review/contract.md
  shared/skills/...         # optional reusable bundles
  scripts/...               # optional command predicates
  rubrics/...               # optional LLM-judge rubrics
```

That folder is the entire workflow. The plugin loads + validates the
workspace at boot, derives one `AgentPosture` per stage, and dispatches each
stage as a sub-agent through a private `delegate.Runtime` whenever an
`io.input` arrives.

## Why this deserves its own surface

A normal ReAct agent is excellent at exploratory work — give it a goal and
some tools and let it churn. But three pressures push real workflows past
what a single ReAct loop can hold:

1. **Repeatability.** A research pipeline that runs every Monday morning
   needs the same shape every time. Free-form ReAct drift makes "the same
   shape" hard to guarantee.
2. **Reviewability.** Stakeholders need to read what the agent will do
   *before* it runs, not infer it from a transcript afterward.
3. **Surgical iteration.** When stage 3 is wrong, you want to fix stage 3 in
   isolation — not re-prompt a system message that drives all 7 stages.

The `nexus.workflows.icm` plugin solves all three by making the workflow a
literal folder you can read, diff, and ship. It also proves a deeper claim
about Nexus's design:

> A non-trivial multi-stage agent system can be expressed without writing
> Go. Posture registration, sub-agent dispatch, schema-validated outputs,
> HITL gates, loop convergence, and fan-out parallelism are already in the
> engine — you compose them with YAML and markdown.

This is the first plugin to combine **all** of them. The walkthrough exists
because doing so end-to-end is a worked example for everything Nexus offers,
not because the ICM plugin itself is unusually complex.

## Mental model

Three vocabulary boundaries decide whether the docs read clearly.

### Workspace, run, session

| Concept | Lives at | Lifespan |
|---------|----------|----------|
| **Workspace** | `<your workspace dir>/` (anywhere on disk) | Authored once; edits land in source control. |
| **Run** | `<engine session>/plugins/<instance>/<runID>/` | One per `io.input`; ephemeral. |
| **Session** | `~/.nexus/sessions/<id>/` | One engine boot. |

The workspace is the **source of truth for the workflow**; the run directory
is the **source of truth for the artifacts**. Edits to the workspace are
authoring; edits to a run directory are tampering. Treating them as
different objects is the single biggest reason the plugin's surface stays
simple.

### Stage, iteration, turn, item

A stage is one folder with one `contract.md`. Inside a stage's lifetime
three nested loops are possible:

- **Turns** — the inner conversation. `turns.policy: fixed | until_valid |
  until_human_approves`. A turn is one LLM call within a single stage
  invocation.
- **Iterations** — convergence loops. `loop.max_iterations: N` with `until`
  predicates. The entire stage runs as a fresh invocation each iteration,
  with the prior iteration's exit failures included in the next payload.
- **Items** — data-driven fan-out. `fan_out.source: ...` points at a JSON
  array produced by an earlier stage; the stage runs once per item, up to
  `max_parallel` concurrently.

Turns sit inside iterations, iterations sit inside the stage, and fan-out
is the stage being dispatched once per item. You almost never mix all three
in a single stage — but the orchestrator supports it because fan-out + loop
composes cleanly (each item independently iterates).

### Posture, delegate, predicate

These are reused engine concepts; ICM does not reinvent any of them.

- **Posture** — `pkg/posture.AgentPosture` from `nexus.agent.postures`.
  ICM *derives* one per stage at `Ready()`, layering operator prompt + body
  + overlay + tools + budget on top of a base posture, then registers it.
- **Delegate** — `pkg/delegate.Runtime`. The same primitive `delegate`
  plugin uses to dispatch sub-agents. ICM keeps its own private
  `delegate.Runtime` so workflow stages don't pollute caller tool budgets.
- **Predicate** — the unified shape used by `output.validators`,
  `loop.until`, and verifier outputs. Six types: `schema`, `regex`,
  `native`, `command`, `llm`, `human`. The loader compiles each at boot.

If you've configured postures + delegate + HITL + the schema registry
before, ICM should feel like wiring those four together into a single user
surface.

## What ICM is not

It is not a replacement for `nexus.agent.react` or
`nexus.agent.orchestrator`. They solve different problems:

| Need | Use |
|------|-----|
| Open-ended exploration with tools | `nexus.agent.react` |
| Decompose-then-parallelize one big request | `nexus.agent.orchestrator` |
| Plan first, then execute the plan | `nexus.agent.planexec` |
| Same N-step shape every time, reviewable in source control | `nexus.workflows.icm` |
| Embed a workflow inside a desktop app | ICM as the agent loop, your shell as the chrome |

It is **not** a new abstraction over LLM providers. Every model call routes
through `delegate` (sub-agents) or through the configured judge posture
(LLM predicates). It does not mutate the workspace — artifacts live in the
session. It does not auto-discover skills from `nexus.skills.scan_paths` —
skills live under the workspace and are loaded only when a stage's
`inputs.skills` references them.

## What you get out of the box

The plugin is fully implemented today. The walkthrough exercises everything
in this list:

- Workspace + contract loader with **aggregated** validation errors
  (boot once to see every problem, fix in one pass).
- One derived `AgentPosture` per stage, registered before the engine
  finishes boot. Tools, model role, budget, and operator prompt are baked
  in.
- Stage-level `plan.created` + `plan.progress` surface so generic UIs
  render the workflow without ICM-specific knowledge, plus a richer
  `icm.*` event family for iteration / turn / item detail.
- Six predicate types with four shipped native handlers
  (`word_count_under`, `word_count_over`, `contains_required_ids`,
  `json_path_exists`).
- Loop convergence with `on_exhausted: human_gate | error` and a
  configurable restart ceiling.
- Fan-out with optional parallelism, per-item folders, and an aggregate
  artifact downstream stages can reference.
- Per-workspace skill resolution (`stages/<NN>/skills/<name>` wins over
  `shared/skills/<name>`) plus an LLM-facing `read_skill_reference` tool
  that surfaces only when a stage actually uses skills.
- Multi-instance support via `nexus.workflows.icm/<suffix>` so two
  workspaces can coexist in one engine.

## Authoring tools

You do not need to write contracts by hand. The repo ships a Claude Code
skill that interviews you about the workflow you want, validates each
answer against the loader's rules, and writes a workspace that loads on the
first try:

- `.claude/skills/icm-workspace-builder/SKILL.md` — invoke with
  `/icm-workspace-builder` once the skills plugin is configured to scan
  `.claude/skills/`. The skill enforces the same rules the loader does
  (folder regex, reserved `00_input`, predicate args, fan-out sources).

The walkthrough is hand-authored so you can read every file; the skill is
the on-ramp for everything else.

## Where to go next

- **[End-to-end walkthrough](./walkthrough.md)** — build a real workspace
  from an empty folder, run it, watch the events, inspect the artifacts.
- **[Plugin reference](../plugins/workflows-icm.md)** — every field on
  every block, every event, every troubleshooting case.
- **[Configuration reference](../configuration/reference.md#nexusworkflowsicm)**
  — every plugin config key with defaults.
- **[Postures](../plugins/agents/postures.md)** — the base postures stages
  inherit from.
- **[Sub-agent delegation](../architecture/delegate.md)** — the dispatch
  primitive ICM stands on.
- **[HITL](../plugins/control.md#nexuscontrolhitl)** — where every human
  gate is routed.
