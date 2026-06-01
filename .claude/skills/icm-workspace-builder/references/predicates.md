# Predicates

Predicates are the unified shape for both output validators and loop exit conditions. The same six types serve both roles; only the semantic differs:

- **Validators** (`output.validators`) gate the artifact write. Pass = output is acceptable, fail = retry the turn (or in the case of `until_valid`, retry up to `turns.max` times).
- **Loop exit conditions** (`loop.until`) gate the loop exit. Pass = done iterating; fail = continue with the next iteration.

Both inject feedback into the next turn or iteration via the same `<previous_attempt>` or `<previous_iteration>` block in the XML payload, so a rubric that produces good feedback is reusable in both roles.

The loader validates predicates at `plugins/workflows/icm/workspace/validate.go:458`.

## The six types

### `schema`

JSON Schema validation. Free, in-process Go via `github.com/santhosh-tekuri/jsonschema/v6`.

```yaml
- type: schema
  name: well_formed
  schema: schemas/script.json
```

The path is workspace-relative. The loader reads the file at load and verifies it parses + compiles as draft-2020. Schema predicates make the most sense when `output.format: json`; for `text` outputs, a regex anchor is usually clearer.

### `regex`

Pattern match using Go's `regexp` package.

```yaml
- type: regex
  name: starts_with_h1
  pattern: '^# .+'
  anchor: first_line          # first_line | last_line | whole; default whole
  message: Output must start with an H1 heading.
```

The loader compiles the pattern at load time (`validate.go:474`); compile failure is a load error. `anchor` controls where the pattern matches — `first_line` and `last_line` scope to single lines, `whole` runs over the full artifact.

### `native`

In-process Go handler. Cost: a function call.

```yaml
- type: native
  name: not_too_long
  handler: word_count_under
  args:
    max_words: 800
```

Use for hot-path mechanical checks where a subprocess spawn or LLM call would be wasteful. Handler existence is checked at *dispatch*, not at load — the loader accepts any non-empty handler name.

The plugin ships **four** baked-in native handlers (registered in `plugins/workflows/icm/predicates/builtins/register.go`):

| Handler | Args | Pass when |
|---|---|---|
| `word_count_under` | `max_words` (int, > 0, required) | artifact word count is STRICTLY less than `max_words` |
| `word_count_over`  | `min_words` (int, >= 0, required) | artifact word count is STRICTLY greater than `min_words` |
| `contains_required_ids` | `ids` ([]string, non-empty, required); `case_insensitive` (bool, optional, default false) | every id in `ids` appears at least once in the artifact text |
| `json_path_exists` | `path` (string, gojq syntax, required); `must_be_non_empty` (bool, optional, default true) | the gojq query yields at least one result, and (when must_be_non_empty) at least one non-null/non-empty result |

Notes:
- `word_count_under` and `word_count_over` use `strings.Fields` for tokenization — any Unicode whitespace separates words. They are strict inequalities; equality fails.
- `contains_required_ids` treats an empty `ids` array as a malformed-args error, not a vacuous-truth pass.
- `json_path_exists` parses the artifact as JSON first. If the artifact is not valid JSON, the predicate fails with `artifact is not valid JSON: ...`. Empty results, null, empty strings, and empty containers count as "empty" when `must_be_non_empty` is true; zero numbers and `false` count as non-empty real values.

If you reference any other handler name, the workspace loads fine but the dispatch will fail with `unregistered native handler`. Make sure a host plugin registers it before booting.

### `command`

Local script. Cost: subprocess spawn.

```yaml
- type: command
  name: lint_script
  run: scripts/lint_script.sh
  timeout_seconds: 60          # optional; default = plugin's predicate_command_timeout_seconds (30)
```

Exit 0 = pass; non-zero = fail. Stdout becomes feedback. The loader resolves `run` against the workspace root, verifies the file exists, and rejects non-executable scripts (loader checks `info.Mode() & 0o111 != 0` at `validate.go:502`). The default plugin-level command timeout is `predicate_command_timeout_seconds` in `schema.json` (default 30s); `timeout_seconds: 0` means "use the plugin default" — negative values are a load error.

### `llm`

LLM judge with a markdown rubric. Cost: an LLM call to a posture.

```yaml
- type: llm
  name: quality_check
  rubric: validators/script_quality.md
  model: small_judge           # POSTURE NAME; optional; defaults to icm.yaml defaults.judge_posture
```

The `model:` field is a posture name (regex `^[a-z][a-z0-9_.-]*$`), NOT a raw model identifier. The plugin resolves it against the posture registry at dispatch. If `model:` is omitted, the plugin falls back to `default_judge_posture` from the plugin config (the schema.json `default_judge_posture` key, or `icm.yaml` `defaults.judge_posture`).

The judge response schema is fixed by the plugin: `{verdict: "pass" | "fail", feedback: string, score?: number}`. Workspace authors do NOT define the schema; they define the rubric.

Default the judge to a small, fast posture (Haiku-class). A pass/fail verdict against a rubric does not need a frontier model.

### `human`

Pause for human input via the HITL bus. Cost: human time.

```yaml
- type: human
  name: editor_approval
  prompt: "Does this draft meet the brief?"
  require_feedback_on_continue: true       # optional; pointer, default true
```

The human sees the latest artifact and answers approve / continue / reject. With `require_feedback_on_continue: true`, continuing without approval forces a feedback string that injects into the next iteration's payload identically to LLM-judge feedback. With `false`, continue can come back with an empty feedback string.

`prompt` is required (loader rejects empty).

## The predicate selection ladder

Walk this ladder cheapest-first. Only reach `llm` when the check genuinely needs semantic judgment.

| Check kind | Predicate | Cost |
|---|---|---|
| "Output is well-formed JSON matching this shape" | `schema` | Free |
| "Output starts with `# ` / matches this pattern" | `regex` | Free |
| "Output has count / contains required IDs / hits a JSON path" | `native` | Function call |
| "Output passes this script's check" | `command` | Subprocess spawn |
| "Output is semantically aligned with X" | `llm` | LLM call |
| "A human approves" | `human` | Human time |

A useful self-test: if you can describe the check without the words "good", "appropriate", or "well-written", it is probably mechanical and belongs above the `llm` line.

## Naming predicates for failure attribution

Every predicate accepts an optional `name`. When multiple predicates fail on the same turn, names disambiguate the feedback the agent receives. If unset, names default to `<type>_<index>` (e.g. `llm_0`, `regex_1`). For predicates that can co-fail in the same turn, set the name so the agent has a labelled list of issues.

## Writing an LLM rubric

A rubric is a markdown file the judge LLM reads alongside the candidate output. It describes what "good" means for this stage's output.

A good rubric:

- Enumerates concrete, checkable criteria. "Opening uses a question, image, or specific number — not a generic preamble like 'In this video...'"
- States failure modes plainly. "Fail if the script exceeds 90 seconds at 150 wpm."
- Avoids subjective filler. "Make it engaging" is not a criterion. "Each beat resolves within 12 seconds" is.

A bad rubric is worse than no validator — the judge will pass everything. Validate a rubric by hand-feeding it a known-bad output and confirming the judge catches it before declaring the stage ready.

## Choosing predicates by stage type

- **Synthesis stages** (writing, summarization, content generation): `regex` for structural anchors + `native:word_count_*` for length + `llm` for quality. This is where iterative refinement pays off most.
- **Transformation stages** (extraction, format conversion): `schema` is usually sufficient.
- **Decision stages** (classification, routing): `schema` + a small `llm` validator confirming the decision is justified by the input.
- **Pure mechanical stages** (file moves, deterministic transforms): no validators; let the schema do its job.

## Loop conditions vs. output validators

Same machinery, different semantic role. Common pattern:

```yaml
output:
  validators:
    - type: schema             # output must be valid JSON
    - type: regex              # output must include required headers

loop:
  until:
    - type: llm                # output is semantically converged
      rubric: validators/converged.md
```

Validators ensure the artifact is well-formed at each turn; the loop condition ensures the stage is *done*. Different layers, different concerns. Mixing roles (e.g. a regex check in `loop.until`) is fine if the check is the right shape — `loop.until` evaluates after each iteration's final accepted artifact.
