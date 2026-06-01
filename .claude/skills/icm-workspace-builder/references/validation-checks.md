# Phase 4 validation checklist

Run every check below before writing files in Phase 5. Each item is taken from the loader at `plugins/workflows/icm/workspace/loader.go` and `validate.go`. When a check fails, fix the design before scaffolding; the loader aggregates errors so you will see all problems on the first `icm_validate` invocation, but fewer errors mean less back-and-forth.

Report each check as PASS / FAIL with the file path on failure.

## Structural

- [ ] Workspace root exists and is a directory (`validate.go:73`).
- [ ] `workspace.md` (or the file named by `layer_names.workspace` in icm.yaml) exists and has non-empty content after trimming.
- [ ] If `icm.yaml` exists, it parses as valid YAML.
- [ ] If `operator.md` exists, it is readable. (Template syntax is not validated until posture build.)
- [ ] `stages/` directory exists and is non-empty.
- [ ] Each stage folder name matches `^\d+_[a-z0-9_]+$`.
- [ ] No stage folder is named `00_input` (reserved by `validate.go:192`).
- [ ] No two stage folders share the same numeric prefix.
- [ ] No stage folder contains an `artifacts/` directory (rejected at `validate.go:226`).

## icm.yaml validity

- [ ] `defaults.turn_policy`, if set, is one of `fixed`, `until_valid`, `until_human_approves`.
- [ ] `defaults.human_gate`, if set, is one of `none`, `start`, `end`, `both`.
- [ ] `defaults.on_error`, if set, is one of `halt`, `retry`, `human_gate`.
- [ ] `defaults.agent.posture`, if set, matches `^[a-z][a-z0-9_.-]*$` (shape check; existence is runtime).
- [ ] `defaults.judge_posture` is set if any predicate uses `type: llm` without an explicit `model:`.

## Per-contract checks

For every `stages/NN_slug/contract.md` and every `verifiers/<id>/contract.md` (or `verifiers/<id>.md`):

- [ ] File begins with `---` and has a closing `---` on its own line.
- [ ] Frontmatter parses as valid YAML.
- [ ] Body (after the second `---`) is non-empty.
- [ ] `id` field, if set, exactly matches the folder name (or filename for flat verifiers).
- [ ] `turns.policy` ∈ `{fixed, until_valid, until_human_approves}`.
- [ ] `turns.max` is a positive integer.
- [ ] `human_gate` ∈ `{none, start, end, both}`.
- [ ] `on_error` ∈ `{halt, retry, human_gate}`.
- [ ] `output.format` ∈ `{text, json}`.
- [ ] `output.persist` ∈ `{context, file_ref, both}`.
- [ ] `output.filename` is set and contains no `/` or `\`.
- [ ] If `output.format: json`, `output.schema` is set.
- [ ] If `turns.policy: until_valid`, `output.validators` is non-empty.

## Output schema

For every contract with `output.format: json`:

- [ ] `output.schema` path resolves under the workspace root.
- [ ] The file parses as JSON.
- [ ] The schema compiles as draft-2020 via `github.com/santhosh-tekuri/jsonschema/v6`.

## Reference resolution

For every contract's `inputs`:

- [ ] Every file in `inputs.grounding` exists under that stage's `grounding/` folder.
- [ ] Every file in `inputs.shared_grounding` exists under `shared/grounding/`.
- [ ] Every path in `inputs.artifacts` matches `^([0-9]+_[a-z0-9_]+|00_input)/([^/]+)$`. The path must NOT contain an `artifacts/` segment.
- [ ] For each `inputs.artifacts` entry: the referenced stage executes earlier than this stage, OR the stage_id is `00_input`.
- [ ] For each non-`00_input` `inputs.artifacts` entry: the filename matches the source stage's declared `output.filename` exactly.
- [ ] Every name in `inputs.skills` matches `^[a-z][a-z0-9-]*$`.
- [ ] Every name in `inputs.skills` resolves to either `stages/NN_slug/skills/<name>/` or `shared/skills/<name>/`. (No third source exists in v1.)

## Predicate validity

For every predicate in `output.validators` and `loop.until`:

- [ ] `type` ∈ `{schema, regex, native, command, llm, human}`.

For `type: schema`:
- [ ] `schema` path resolves under the workspace.
- [ ] File parses as JSON and compiles as draft-2020.

For `type: regex`:
- [ ] `pattern` is non-empty and compiles as Go `regexp` syntax.
- [ ] `anchor`, if set, ∈ `{first_line, last_line, whole}`.

For `type: native`:
- [ ] `handler` is non-empty.
- [ ] (Recommended) `handler` is one of the four shipped names: `word_count_under`, `word_count_over`, `contains_required_ids`, `json_path_exists`, OR you have a host plugin that registers the named handler. Loader accepts any string.
- [ ] For `word_count_under`: `args.max_words` is a positive integer.
- [ ] For `word_count_over`: `args.min_words` is a non-negative integer.
- [ ] For `contains_required_ids`: `args.ids` is a non-empty array of strings.
- [ ] For `json_path_exists`: `args.path` is a non-empty string.

For `type: command`:
- [ ] `run` path resolves under the workspace (the loader resolves with `resolveWorkspacePath`).
- [ ] The script file exists.
- [ ] The script is executable (`info.Mode() & 0o111 != 0`).
- [ ] `timeout_seconds` is >= 0.

For `type: llm`:
- [ ] `rubric` path resolves to an existing file under the workspace.
- [ ] `model`, if set, matches `^[a-z][a-z0-9_.-]*$` (posture-name shape, not a raw model identifier).

For `type: human`:
- [ ] `prompt` is non-empty.

## Loop validity

If `loop` block is present:

- [ ] `max_iterations` is a positive integer.
- [ ] `until` is non-empty.
- [ ] `on_exhausted`, if set, ∈ `{human_gate, error}` (defaults to `human_gate`).
- [ ] Every entry in `until` passes the predicate checks above.

## Fan-out validity

If `fan_out` block is present:

- [ ] `source` is non-empty and matches the artifact ref regex `^([0-9]+_[a-z0-9_]+|00_input)/([^/]+)$`.
- [ ] The source stage runs earlier than this stage, OR the source is `00_input/<filename>`.
- [ ] The filename in `source` matches the source stage's declared `output.filename`.
- [ ] `item_var` is non-empty.
- [ ] `jsonpath`, if set, compiles as a gojq expression.
- [ ] `item_id`, if set, compiles as a gojq expression.
- [ ] `max_parallel`, if set, is positive (loader silently treats <=0 as 1).
- [ ] `on_item_failure`, if set, ∈ `{continue, halt}` (defaults to `continue`).

## Agent validity

For every contract's `agent`:

- [ ] `posture`, if set, matches `^[a-z][a-z0-9_.-]*$`.
- [ ] `model_role` is a string (no regex check).
- [ ] `tools`, if set, is an array of strings.
- [ ] `budget.timeout_seconds` is >= 0.
- [ ] `budget.max_tokens` is >= 0.
- [ ] `budget.max_tool_calls` is >= 0.
- [ ] `max_recursion_depth` is >= 0.

## Verifier validity

- [ ] Every `verifiers[]` entry on every contract resolves to a file or folder under `verifiers/` (`validate.go:677`).
- [ ] Every verifier file/folder passes the contract checks above.

## Skill validity

For every skill resolved through `inputs.skills`:

- [ ] Folder name matches `^[a-z][a-z0-9-]*$`.
- [ ] Folder contains a `SKILL.md`.
- [ ] `SKILL.md` parses: valid YAML frontmatter delimited by `---`.
- [ ] Frontmatter has non-empty `name` and `description`.
- [ ] Frontmatter `name` exactly matches the folder name.
- [ ] If `references/` exists, every file within is a regular file (not a symlink or device).

## What is NOT validated at load time

These are dispatch-time concerns:

- Whether `agent.posture` and `judge_posture` names actually exist in the posture registry (validated at runtime when the posture builder tries to look them up).
- Whether `native` predicate handlers other than the four shipped names are registered.
- Whether `fan_out.source` resolves to a JSON array at run time (the source artifact is generated dynamically).
- Whether `fan_out.item_id` gojq actually yields a string for individual items (compilation is validated, evaluation is not).
- Whether agent tools exist in the Nexus tool registry.
- Whether the agent's model_role resolves to a reachable model.

Dispatch-time failures here apply `on_error` policy per the failing stage.

## How to run the check

Once the workspace is on disk:

1. From the LLM tool surface, invoke `icm_validate` (or `icm_validate_<suffix>` for an instance-suffixed plugin) with `{"workspace": "<absolute path>"}`. The plugin runs `workspace.Validate(path)` and returns `ok` or a multi-line error report.
2. Programmatically: call `workspace.LoadWorkspace(path, workspace.WithDefaultOperatorBytes(defaults))` (`plugins/workflows/icm/workspace/loader.go:52`). Returns `*Workflow` on success or `*LoadErrors` aggregating every issue found in one pass.

The aggregated error list IS the validation result. Re-running after each fix is cheap (sub-second) since no LLM calls happen at load.
