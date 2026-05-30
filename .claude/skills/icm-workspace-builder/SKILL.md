---
name: icm-workspace-builder
description: Scaffold ICM (Interpretable Context Methodology) workspaces for the Nexus nexus.workflows.icm plugin. Use when the user wants to create, scaffold, design, edit, or validate a multi-stage file-driven agent workflow workspace — phrases like "create an ICM workspace", "scaffold an ICM workflow", "build a multi-stage pipeline workspace", "add a stage", "workflow workspace for nexus.workflows.icm", "set up a content/research/script/translation pipeline", or any mention of operator.md, workspace.md, contract.md, grounding/, or stages/NN_slug/. Interviews the user about stages, gates, and predicates, then writes a workspace folder that conforms to every Nexus rule: stage folder regex, reserved 00_input, agent.model_role + agent.posture + agent.budget block, posture-name (not raw model) judge_posture, four shipped native predicates (word_count_under, word_count_over, contains_required_ids, json_path_exists), fan-out source pointing at JSON artifacts, two-source skill resolution (stage-local + workspace-shared), and SKILL.md frontmatter limited to name + description.
---

# ICM Workspace Builder

An ICM workspace is a folder on disk. The folder structure encodes a sequential, human-reviewable agent workflow that the `nexus.workflows.icm` plugin loads at boot, registers as a posture per stage, and dispatches stage-by-stage when an `io.input` arrives.

Your job in this skill is to interview the user about the workflow they want, then produce a folder that the Nexus loader (`plugins/workflows/icm/workspace/loader.go`) will accept on the first try.

You should treat this as a five-phase process: discovery, stage mapping, per-stage detail, conformance validation, and on-disk scaffolding. Do not skip ahead — workspaces that look reasonable but skip the validation phase tend to fail load with reference errors that are tedious to fix retroactively.

## Workspace shape at a glance

```
<workspace>/
  icm.yaml                  optional — layer_names overrides + workspace defaults
  operator.md               optional — Layer 0 operator identity; plugin ships a default
  operator.overlay.md       optional — appended to whichever operator.md was loaded
  workspace.md              REQUIRED — Layer 1 workflow description, non-empty
  stages/
    01_<slug>/
      contract.md           REQUIRED — YAML frontmatter + process body
      grounding/            optional — Layer 3 reference material for this stage
      skills/<name>/        optional — stage-local skills
    02_<slug>/...
  shared/
    grounding/              optional — workspace-wide grounding files
    skills/<name>/          optional — workspace-shared skills
  schemas/<name>.json       optional — JSON schemas for json outputs / schema predicates
  scripts/<name>.sh         optional — command-predicate scripts (must be executable)
  validators/<name>.md      optional — markdown rubrics for llm-predicate judges
  verifiers/<id>/           optional — verifier stages (same shape as stages)
```

Artifacts do NOT live in the workspace. The plugin writes every artifact under `<session>/<runID>/<stage_id>/<filename>`. A workspace that ships with an `artifacts/` directory is broken — the loader rejects it (see `plugins/workflows/icm/workspace/validate.go:226`).

For the precise on-disk layout that the orchestrator produces during a run (iteration folders, fan-out `items/<id>/`, sidecar `.icm.json`), see [`references/architecture.md`](references/architecture.md).

## When to use this skill

Trigger this skill when the user wants any of:

- A new ICM workspace from scratch ("scaffold a research-then-draft workflow").
- A new stage added to an existing workspace.
- A predicate, loop, or fan-out block added to a stage.
- A conformance check of an existing workspace before they run `nexus icm_validate`.
- An explanation of one of the configuration knobs while editing.

If the user only wants generic Nexus help (configuring a different plugin, debugging an LLM call), this skill is the wrong tool — defer.

## The five-phase interview protocol

Do not write any files until Phase 5. Work through phases in order. After each phase, summarize back what you heard and confirm before proceeding.

### Phase 1: Workflow purpose

Goal: get the one-sentence purpose and the final-artifact shape in your head.

Ask only what is not obvious from the conversation:

1. What is the single deliverable this workflow produces? (one sentence, e.g. "a 60-second video script with shot list")
2. What input does it start with? (a brief, a URL, a structured object, a file?)
3. Walk through the steps a human would take to produce this manually.
4. Where in that flow does a human need to review or edit between steps?
5. What persistent constraints — style, voice, schema, IDs — must hold across every run?

Resist diving into mechanics here. You are mapping intent.

### Phase 2: Stage decomposition

Goal: convert the manual process into a stage sequence.

Apply the rule **one stage, one job**. Combine when the seam is artificial; split when a single contract.md would be doing two things.

For each candidate stage, write:

- Stage ID (folder name: `NN_<lowercase_slug>`, e.g. `02_script`).
- Display label (one-line human-readable).
- Inputs: prior artifacts, grounding files, shared grounding, skills.
- Output: filename + format + persist mode + schema path (if json).
- Human gate: none / start / end / both.
- Stage shape:
  - **Plain** — single invocation, single output.
  - **Looping** — iterates the whole stage until exit predicates pass. See [`references/loops.md`](references/loops.md).
  - **Fan-out** — runs once per item in a JSON array. See [`references/fan-out.md`](references/fan-out.md).
  - **Fan-out + loop** — compose: per-item convergence.

Iterate the list with the user until they agree it is the right granularity. Still no files.

### Phase 3: Per-stage detail

Goal: fill out the contract.md frontmatter for each stage.

For every stage you decided to keep, work out:

- `turns.policy` (fixed / until_valid / until_human_approves) + `turns.max`.
- `output.validators[]` — walk the predicate selection ladder cheapest-first; see [`references/predicates.md`](references/predicates.md).
- `loop` block (only for looping stages).
- `fan_out` block (only for fan-out stages); confirm the source is a JSON array artifact.
- `agent` block: `posture`, `model_role`, `tools[]`, `budget`, `prompt_overlay`.
- `inputs.skills` — only resolvable as stage-local or workspace-shared (v1 has no Nexus-registered skill source for ICM). See [`references/skills.md`](references/skills.md).
- `verifiers[]` if any.

For each `type: llm` predicate, sketch the rubric in plain English now so you know what `validators/<name>.md` should contain.

Full field reference: [`references/contract-format.md`](references/contract-format.md).

### Phase 4: Conformance validation

Goal: dry-run the Nexus loader rules against the design before writing files.

This is the most valuable phase — every conformance violation you catch here saves a load-error iteration after writing. The full checklist is [`references/validation-checks.md`](references/validation-checks.md). Pay attention to these load-time rules in particular (sourced from `plugins/workflows/icm/workspace/loader.go` and `validate.go`):

- Stage folder names match `^\d+_[a-z0-9_]+$`. No uppercase, no dashes.
- `00_input` is reserved — never define a `stages/00_input/` folder.
- No duplicate numeric prefixes across stages.
- No `artifacts/` subfolder anywhere in `stages/<NN_slug>/`.
- `inputs.artifacts` entries must be `<stage_id>/<filename>` and must reference a stage that executes *before* this one.
- The `<filename>` in an artifact reference must exactly equal the source stage's declared `output.filename`.
- Every `inputs.grounding` and `inputs.shared_grounding` path must point at an existing file (loader does `os.Stat`).
- Every `inputs.skills` name matches `^[a-z][a-z0-9-]*$`.
- If `output.format: json`, then `output.schema` is set and the schema parses + compiles as draft-2020 JSON Schema.
- If `turns.policy: until_valid`, then `output.validators` is non-empty.
- For `type: command` predicates, the `run` script exists and is executable (mode bits & 0o111).
- For `type: llm` predicates, `model:` (if set) is a posture *name* matching `^[a-z][a-z0-9_.-]*$`, NOT a raw model identifier.
- For `type: native` predicates, the `handler` field is non-empty. The four shipped handlers are `word_count_under`, `word_count_over`, `contains_required_ids`, `json_path_exists`. Other names load but will fail at dispatch unless a host plugin registers them.
- `fan_out.source` must match `^([0-9]+_[a-z0-9_]+|00_input)/([^/]+)$` and point at a stage that runs earlier.
- `agent.posture`, when set, matches `^[a-z][a-z0-9_.-]*$`. Existence is verified at runtime against the posture registry, not at load.
- `agent.budget.*` must all be non-negative integers.

If the user's design violates any of these, fix the design (not the loader). Anti-patterns and how to refactor around them: [`references/anti-patterns.md`](references/anti-patterns.md).

### Phase 5: Scaffold on disk

Goal: write the workspace folder.

Before you start writing, run pre-flight checks:

1. Confirm the workspace path. Expand `~` yourself; do not write to the literal `~/...`.
2. If the directory exists and is non-empty, ask: append into it, or refuse and ask for a clean path? Do not silently overwrite.
3. Confirm the user has (or plans to register) the postures named in `agent.posture` and `default_judge_posture`. The plugin loads but stages will fail at dispatch if a referenced posture is missing.
4. If any `type: llm` predicates exist, confirm `default_judge_posture` is set in `icm.yaml` defaults (or every predicate names its own `model:`).

Write in this order, so partial scaffolds are still meaningful:

1. `workspace.md` — high-level workflow description. Required, non-empty.
2. `icm.yaml` — only when defaults need overriding. Otherwise omit.
3. For each stage `stages/NN_slug/`:
   - `contract.md` with YAML frontmatter + body.
   - `grounding/` with stubbed reference files (TODO at top).
4. `shared/grounding/` — workspace-wide grounding (only if any contract referenced it).
5. `shared/skills/<name>/SKILL.md` (+ optional `references/`) — for any workspace-shared skill.
6. `stages/NN_slug/skills/<name>/SKILL.md` — for any stage-local skill.
7. `schemas/<name>.json` — every JSON-output + every `type: schema` predicate. Use real constraints, not `{"type": "object"}`.
8. `validators/<name>.md` — every `type: llm` predicate's rubric. Enumerate concrete pass/fail criteria.
9. `scripts/<name>.sh` — every `type: command` predicate. Mark them executable (`chmod +x`). The loader rejects non-executable scripts.
10. `verifiers/<id>/contract.md` — every verifier referenced by `verifiers[]` (or a flat `<id>.md` file if you prefer the single-file form).
11. `operator.md` or `operator.overlay.md` — only when the plugin's embedded default operator (`plugins/workflows/icm/defaults/operator.md`) does not fit.

After the workspace is on disk, tell the user how to test it (see the final section below).

## icm.yaml format

```yaml
# Optional — overrides the four layer filenames.
layer_names:
  operator: operator.md     # default
  workspace: workspace.md   # default
  contract: contract.md     # default
  grounding: grounding      # default folder name under each stage

# Optional — workspace-wide stage defaults.
defaults:
  turn_policy: fixed        # fixed | until_valid | until_human_approves
  human_gate: none          # none | start | end | both
  on_error: halt            # halt | retry | human_gate
  judge_posture: small_judge # posture name used by type:llm predicates that omit model:
  agent:                    # default agent spec; stage-level fields override per-field
    posture: workflow_default
    model_role: planner
    budget:
      timeout_seconds: 120
      max_tokens: 8000
      max_tool_calls: 20

# Optional — overlay appended to operator.md (whether workspace's or embedded default).
operator:
  overlay: operator.overlay.md
```

Note: the field is `judge_posture`, NOT `judge_model`. The plugin treats it as a posture name (matching `^[a-z][a-z0-9_.-]*$`). See `plugins/workflows/icm/workspace/types.go:78`.

The overlay filename derives from `layer_names.operator` automatically: `operator.md` → `operator.overlay.md`. If `layer_names.operator` is overridden to `ops.md`, the overlay is `ops.overlay.md`. Do not declare the overlay path manually — `operator.overlay` in `icm.yaml` only toggles whether the overlay block is loaded.

## Naming and format rules (quote these verbatim)

From `plugins/workflows/icm/workspace/loader.go:16-20`:

```go
stageFolderRE = regexp.MustCompile(`^\d+_[a-z0-9_]+$`)
skillNameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
artifactRefRE = regexp.MustCompile(`^([0-9]+_[a-z0-9_]+|00_input)/([^/]+)$`)
postureNameRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
```

- Stage folders: digits + underscore + lowercase alphanumeric + underscores. `01_research`, `12_final_review` are valid; `01-research`, `01_Research`, `research` are not.
- Skill folder names (and `inputs.skills` entries): start with a lowercase letter, then lowercase alphanumeric + hyphens. `content-style` is valid; `content_style` is NOT.
- Artifact references: `<stage_id>/<filename>`. The stage_id is either a real stage folder name or the reserved `00_input`. No `/artifacts/` segment.
- Posture names (`agent.posture`, `judge_posture`, `type: llm` `model:`): lowercase letter start, then lowercase alphanumeric, underscores, dots, hyphens. `claude_sonnet_4`, `small.judge`, `workflow-default` are valid.

## Defaults to assume when the user does not specify

- `turns.policy: fixed`, `turns.max: 1`. Switch to `until_valid` only when at least one validator exists.
- `human_gate: end` for content/synthesis stages; `none` for transformation, extraction, and verifier stages.
- `output.format: text` unless a downstream consumer needs structured data.
- `output.persist: file_ref` (default; the orchestrator writes to disk and downstream stages get a `<file_ref/>` in their payload).
- `output.validators: []` — none by default. Add deliberately.
- `on_error: halt`.
- `agent.model_role`, `agent.posture`: leave unset unless the workspace needs a specific posture. The plugin's `default_workflow_posture` config or workspace `defaults.agent` provides the base.

State the defaults you applied at the end of Phase 5 so the user can audit them.

## After scaffolding

Tell the user three things, in order:

1. **Validate the workspace.** The plugin registers an LLM-facing tool named `icm_validate` (or `icm_validate_<suffix>` if the plugin is instance-suffixed, e.g. `nexus.workflows.icm/script` → `icm_validate_script`). Invoke it with `{"workspace": "<absolute path>"}` to get the aggregated load errors or `ok`. The same code path runs `workspace.Validate(path)` from `plugins/workflows/icm/workspace/loader.go:58`.

2. **Confirm postures.** Any posture name referenced in `default_judge_posture`, `default_workflow_posture`, `defaults.agent.posture`, a stage's `agent.posture`, or a `type: llm` predicate's `model:` must exist in the registry that the `nexus.agent.postures` plugin (or another posture provider) loads. Postures register from YAML files in directories declared in `nexus.agent.postures` `scan_dirs`. Suggest the user add the necessary YAML before booting Nexus with the workspace.

3. **Test small first.** Recommend running a single-stage version with `human_gate: end` and no validators, then layering on validators, loops, fan-out, and verifiers in subsequent edits. ICM workspaces are easy to load and hard to debug all at once.

## Reference files

Load these only when their concern is in scope. For a plain three-stage sequential workspace you may only need `contract-format.md` and `predicates.md`.

- [`references/contract-format.md`](references/contract-format.md) — every field of `contract.md` frontmatter, with valid values and defaults pulled from `types.go` and `validate.go`.
- [`references/predicates.md`](references/predicates.md) — six predicate types, the four shipped native handlers with their args, the cost ladder.
- [`references/skills.md`](references/skills.md) — workspace skill SKILL.md format and the two-source resolution chain.
- [`references/loops.md`](references/loops.md) — `loop.until` semantics, exhausted action, the LoopMaxRestarts plugin knob.
- [`references/fan-out.md`](references/fan-out.md) — `fan_out` semantics, `item_id` gojq, JSON array source requirement, aggregation.
- [`references/architecture.md`](references/architecture.md) — file-driven principle, the posture-per-stage design, session dir layout, plugin config (icm.yaml + plugin schema.json) split.
- [`references/validation-checks.md`](references/validation-checks.md) — full Phase 4 checklist, sourced item-by-item from the loader's error paths.
- [`references/anti-patterns.md`](references/anti-patterns.md) — things to push back on during the interview.

## What "done" looks like

- `icm_validate` returns `ok` against the workspace path.
- Every required posture exists in the user's posture registry directory.
- The user has read `workspace.md` and each `contract.md` body and confirms the workflow.
- Every JSON schema has at least one required field with a real constraint (no rubber-stamp `{"type": "object"}`).
- Every LLM rubric enumerates specific, falsifiable criteria.
- The user knows how to launch a run (`io.input` carrying a path or content).

Then tell the user the path is ready to use in the plugin's `workspace:` config key.
