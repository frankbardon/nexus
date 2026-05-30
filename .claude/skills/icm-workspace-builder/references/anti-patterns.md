# Anti-patterns

Things to push back on when authoring an ICM workspace. Some of these the loader catches; many it does not, but they produce workflows that load fine and run badly.

## Stage design

- **Stages that do more than one job.** "Research and draft" → split into research + draft. One stage, one job. The whole point of stage boundaries is human-reviewable seams and clean artifact handoffs.
- **Stages combined to save folders.** Each stage costs almost nothing on disk. Combining costs clarity and erases the review surface.
- **Renaming folders without renumbering prefixes.** The loader sorts by numeric prefix. `script` and `02_script` are different things; missing prefix is a load error.
- **Folder names with hyphens, uppercase, or dots.** The regex `^\d+_[a-z0-9_]+$` rejects them. Use underscores: `02_final_review`, not `02-final-review` or `02_Final_Review`.
- **Defining a `stages/00_input/` folder.** `00_input` is reserved for the run's initial input. The loader rejects this folder by name.

## Field names

- **Using `agent.model` instead of `agent.model_role`.** Nexus is a role-based model registry; raw model identifiers belong in posture YAML, not workspace contracts. `agent.model` is silently ignored — the field is `agent.model_role`.
- **Using `judge_model` instead of `judge_posture`.** The icm.yaml defaults field is `judge_posture`. The plugin config key is `default_judge_posture`. The `type: llm` predicate field is `model:` but is interpreted as a posture name.
- **Putting a raw model identifier in a `type: llm` predicate's `model:` field.** The loader validates `^[a-z][a-z0-9_.-]*$` — `claude-haiku-4-5` will fail because of the dashes. Use a posture name (`small_judge`, `judge.cheap`).

## Grounding

- **Grounding files larger than ~2000 tokens that are not behind a `file_ref`.** Either trim, or set `output.persist: file_ref` upstream and let the operator pull on demand.
- **Per-stage grounding folders duplicating shared content.** If a voice guide applies to three stages, it goes in `shared/grounding/`, not three copies.
- **Grounding files that drift toward "everything we know".** Grounding is constraints, not knowledge. If you find yourself adding paragraphs of context, that is research output (a stage's artifact), not Layer 3 reference.

## Validators and predicates

- **JSON schemas with no real constraints.** Every field `string`, every field optional — the schema is not doing any work. The schema IS the contract; weak schemas mean weak contracts.
- **LLM validators with vague rubrics.** "Make it good" is not a rubric. A vague rubric produces a judge that passes everything, which is worse than no validator.
- **`turns.policy: until_valid` with no validators.** The policy has no meaning without something to validate against. The loader rejects this at `validate.go:338`.
- **Reaching for `llm` predicates when `schema`/`regex`/`native` would do.** Walk the predicate selection ladder. LLM predicates are the most expensive option; use them last.
- **Verifier stages for mechanical cross-stage checks.** Spinning up a sub-agent to count items or check field existence is wasteful. Use a `native:contains_required_ids` or `native:json_path_exists` predicate.
- **Using `native` with a handler name that no plugin registers.** The loader accepts any non-empty string for `handler`, but dispatch will fail with `unregistered native handler`. Stick to the four shipped names (`word_count_under`, `word_count_over`, `contains_required_ids`, `json_path_exists`) unless you control the host plugin.

## Loops and fan-out

- **Stage loops with vague exit conditions.** The loop runs to `max_iterations` every time and the work is wasted. Every `loop.until` predicate must be falsifiable.
- **`max_iterations` past 10 without thinking.** If you genuinely need 20 iterations, the stage is doing too much; split it.
- **Stage loops where a fan-out pattern fits better.** If you are iterating because you have a list of items to process, that is `fan_out`, not `loop`. Loop = convergence on one thing; fan-out = once per item.
- **Fan-out where multiple stages would be clearer.** 2-3 known items up front → 2-3 stages. Fan-out is for dynamic or larger lists.
- **Aggregate-level validators in fan-out.** Validators run per-item. If aggregate quality matters, write a downstream stage that reads the aggregate.
- **`fan_out.source` pointing at a `text` artifact.** The source must be JSON. Change the upstream stage's `output.format` to `json` with a schema that defines an array of items.
- **Empty-array fan-out treated as failure inside the stage.** Empty source means zero invocations and a success with an empty aggregate. If empty input should fail, declare a `native:json_path_exists` predicate on the upstream stage that rejects empty arrays.

## Human gates

- **Human gates on every stage.** The middle of a pipeline should not need approval; the start (direction-setting) and end (alignment check) usually do. This matches the U-shaped intervention pattern documented in the ICM paper.
- **Human gates inside a loop expecting per-iteration review.** `human_gate: end` fires at the entire stage's end, not per iteration. For per-iteration human review, use a `type: human` predicate in `loop.until`.

## Configuration

- **An `artifacts/` folder in the workspace.** Artifacts live in the session dir. A workspace that ships with `artifacts/` means someone confused the spec with a run. The loader rejects it.
- **Workspace-level config that is really a plugin concern.** API keys, model identities, transport — that is all plugin (or Nexus) config. Workspace config is workflow logic only.
- **Plugin-side config that is really workflow logic.** If you are tempted to set "stage 2's exit condition" in YAML, the spec needs a new file-driven knob — open it as a feature request, not a workaround.
- **Hard-coding a workspace path in `icm.yaml`.** `icm.yaml` is workspace-internal config; the workspace path lives in the plugin YAML.
- **Setting `cache_size` > 0 without thinking about tool side effects.** The plugin defaults `cache_size` to 0 deliberately. ICM stages typically have tool side effects, HITL gates, and predicate retries that make cross-run cached results hostile. Enable caching only when you understand which stages are deterministic.

## Operator prompt

- **Custom `operator.md` for generic workflows.** Use the plugin's embedded default. Customize only for regulated domains or unusual operating constraints.
- **Stuffing per-stage instructions into `operator.md`.** Stage-specific behavior goes in `contract.md` body (the `Role` field), not the operator system prompt. Use `agent.prompt_overlay` for per-stage operator refinements.
- **Templating without escaping in `operator.md`.** The body is a Go `text/template`. Literal `{{` or `}}` not intended as actions will break parsing. Escape with `{{"{{"}}`.

## Skills

- **Skill folder names with underscores.** Loader rejects them. Use hyphens.
- **`SKILL.md` frontmatter with extra fields.** The loader only reads `name` and `description`. Extra fields are silently ignored today; future versions may reject them.
- **Skills that are really just one file.** If your skill has a `SKILL.md` and no `references/`, it is grounding. Move it.
- **Referencing a Nexus-registered skill by name in `inputs.skills`.** There is no Nexus-registered third source for ICM skills in v1. Only stage-local and workspace-shared resolve.

## Anti-patterns about anti-patterns

- **Treating this list as exhaustive.** The patterns above cover what tends to go wrong repeatedly. Novel workflows produce novel mistakes. Always reason from the design principles: one stage one job, file-driven config, in-process Go where possible, the predicate cost ladder, human review where direction is set or alignment is checked.
