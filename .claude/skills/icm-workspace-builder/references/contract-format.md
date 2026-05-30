# contract.md format

Every stage folder under `stages/NN_slug/` contains a `contract.md`. The file has two parts: YAML frontmatter delimited by `---` lines, and a markdown body. The frontmatter declares structured configuration that the loader parses into the `Stage` struct (`plugins/workflows/icm/workspace/types.go:86`); the body is free-form prose rendered into the operator system prompt as `{{ .Stage.Role }}`.

Verifier files under `verifiers/` use the same frontmatter shape. A verifier may live as a folder (`verifiers/<id>/contract.md`) or as a flat file (`verifiers/<id>.md`).

## Full example

```yaml
---
id: 02_script                   # optional; defaults to folder name; must match if present
display: "Draft the 60s script" # optional; falls back to first non-empty body line, then ID
turns:
  policy: until_valid           # fixed | until_valid | until_human_approves
  max: 3                        # positive int; default 1 for fixed, 3 for until_valid
human_gate: end                 # none | start | end | both
on_error: halt                  # halt | retry | human_gate
loop:                           # optional; omit for non-looping stages
  max_iterations: 5
  until:
    - type: llm
      rubric: validators/draft_approved.md
  on_exhausted: human_gate      # human_gate | error
fan_out:                        # optional; omit for non-fan-out stages
  source: 01_research/topics.json
  jsonpath: .topics             # gojq expression; default "." (whole document)
  item_var: topic
  item_id: .slug                # gojq expression on each item for folder naming
  max_parallel: 1
  on_item_failure: continue     # continue | halt
output:
  format: json                  # text | json
  schema: schemas/script.json   # workspace-relative; required when format=json
  persist: file_ref             # context | file_ref | both
  filename: script.md           # required; no path separators
  validators:
    - type: regex
      name: starts_with_h1
      pattern: '^# .+'
      anchor: first_line        # first_line | last_line | whole; default whole
    - type: llm
      name: quality_check
      rubric: validators/script_quality.md
      model: small_judge        # POSTURE NAME, not a raw model identifier
inputs:
  grounding: [voice.md, structure.md]            # relative to this stage's grounding/
  shared_grounding: [house_style.md]             # relative to shared/grounding/
  artifacts: [01_research/research.md]           # <stage_id>/<filename>; must run earlier
  skills: [content-style]                        # stage-local or workspace-shared only
agent:
  posture: drafter              # registered posture name; existence checked at runtime
  model_role: writer            # NOT model: — this is a registry role name
  tools: [read_file]            # nil inherits; explicit [] means "no tools beyond posture"
  prompt_overlay: |
    Pay special attention to pacing constraints.
  budget:
    timeout_seconds: 180
    max_tokens: 12000
    max_tool_calls: 30
  max_recursion_depth: 2        # 0 inherits posture default
verifiers: [audit_script]       # IDs registered under verifiers/
---

# Process

Free-form prose. This is the stage's role/instructions, rendered into the
operator system prompt as `{{ .Stage.Role }}`. Be specific about what the
stage does, what good output looks like, and any non-obvious constraints
that cannot be expressed declaratively in the validators.
```

## Field reference

### `id` (optional)
String. Stage identifier. Defaults to the folder name. If set, must match the folder name. Mismatch is a load error.

### `display` (optional)
String. Human-readable label for UIs and the derived posture description. Trimmed to 80 chars at load time. Defaults to the first non-empty line of the body (with leading `#` stripped), then to the stage ID. (`plugins/workflows/icm/workspace/parse_contract.go:96`)

### `turns` (optional)
Object. Inner-turn behavior within a single stage invocation. The same sub-agent retries within one invocation; do not confuse with `loop`, which re-dispatches the entire stage.

- `policy`: one of `fixed`, `until_valid`, `until_human_approves`. Default `fixed`.
- `max`: positive integer. Default 1 for `fixed`, 3 for `until_valid`.

`until_valid` REQUIRES at least one entry in `output.validators` (loader-enforced at `validate.go:338`).

### `human_gate` (optional)
One of `none`, `start`, `end`, `both`. Default `none`. Fires at the bounds of the entire stage, not per turn, iteration, or fan-out item. For per-iteration human review use a `type: human` predicate inside `loop.until`.

### `on_error` (optional)
One of `halt`, `retry`, `human_gate`. Default `halt`. Governs non-validator failures: LLM API errors, malformed output, tool call failures, fan-out source not resolving to an array.

### `loop` (optional)
Object. Declares the stage iterates as a whole. See [`loops.md`](loops.md).

### `fan_out` (optional)
Object. Declares the stage runs once per item in a JSON array. See [`fan-out.md`](fan-out.md).

### `output` (required)
Object. What the stage writes and how it is validated.

- `format`: `text` or `json`. Default `text`.
- `schema`: workspace-relative path to a JSON Schema file. REQUIRED when `format: json`. The loader reads + compiles the schema at load via `github.com/santhosh-tekuri/jsonschema/v6`.
- `persist`: `context`, `file_ref`, or `both`. Default `file_ref`.
- `filename`: string. REQUIRED. Must not contain `/` or `\`.
- `validators`: array of predicates. See [`predicates.md`](predicates.md).

### `inputs` (optional)
Object. What the stage reads.

- `grounding`: array of paths relative to this stage's `grounding/` folder. Existence verified at load.
- `shared_grounding`: array of paths relative to `shared/grounding/`. Existence verified at load.
- `artifacts`: array of logical refs `<stage_id>/<filename>`. The stage_id must run earlier in execution order (or be the reserved `00_input`), and the `<filename>` must match the source stage's declared `output.filename` exactly. The token `artifacts/` must NOT appear in the ref (the loader rejects refs containing `/artifacts/`).
- `skills`: array of skill names. Each name resolves through the two-source chain: stage-local `stages/NN_slug/skills/<name>/` → workspace-shared `shared/skills/<name>/`. There is no Nexus-registered third source in v1 — see [`skills.md`](skills.md).

### `agent` (optional)
Object. Sub-agent configuration. All fields optional.

- `posture`: string. Name of a registered posture. The loader validates shape only (regex `^[a-z][a-z0-9_.-]*$`); existence is verified at runtime by the posture registry. When set, replaces the workspace-level `default_workflow_posture` as the base posture before per-field overrides apply.
- `model_role`: string. Engine model-role name (not a raw model identifier). Overrides the base posture's model role when non-empty.
- `tools`: array of tool names. `nil` inherits workspace defaults + base posture allowed tools. An explicit empty `[]` clears them. Set entries are resolved against the catalog at dispatch.
- `prompt_overlay`: string. Appended verbatim to the rendered operator prompt for this stage only.
- `budget`: object — `{timeout_seconds, max_tokens, max_tool_calls}`. All non-negative integers. Zero inherits the posture default.
- `max_recursion_depth`: non-negative integer. 0 inherits the posture default.

The field is `agent.model_role`, NOT `agent.model`. Nexus is a role-based model registry; raw model identifiers belong in posture YAML, not workspace contracts. (`plugins/workflows/icm/workspace/types.go:394`)

### `verifiers` (optional)
Array of strings. Verifier IDs declared under `verifiers/`. The loader validates that every referenced verifier exists (`validate.go:673`). Verifiers run after this stage's output is written; verifier failure escalates per the verifier's own `on_error`.

## Body rendering

The body (everything after the second `---`) is treated as literal text and injected into the operator system prompt as `{{ .Stage.Role }}`. The body is NOT itself a template — Go's `text/template` is applied to the operator.md content, with the stage Role substituted into it. If you need dynamic content in the body, that is a tool call or a prior stage's artifact, not a template variable.

## Resolution and defaults

Field resolution cascade, most specific wins:

1. Stage `contract.md` frontmatter.
2. Workspace `icm.yaml` `defaults` block.
3. Loader library defaults (turn_policy=fixed, human_gate=none, on_error=halt, output.format=text, output.persist=file_ref).

Absent fields at every level fall through to library defaults.

## Body checking

The loader rejects:

- Empty body after the second `---` (`validate.go:275`).
- Missing or unreadable `contract.md`.
- Missing closing `---` for the frontmatter.

The body should describe the stage's job in operational terms — what to produce, what good looks like, what to avoid. Treat it as the prompt's job description, not metadata.
