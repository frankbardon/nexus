# Architecture

Cross-cutting design principles for `nexus.workflows.icm`: file-driven configuration, posture-as-stage, predicate dispatch, session dir layout, plugin lifecycle, and the boundary between workspace-owned config and plugin-owned config.

## The file-driven principle

ICM is file-first. Everything *about the workflow* lives in the workspace; the plugin owns only platform concerns. Lock this principle in when authoring.

**In workspace files:**

- Stage definitions, ordering, dependencies (`contract.md` frontmatter and body).
- Validator criteria — JSON schemas, LLM rubrics (`validators/*.md`), regex patterns, command scripts (`scripts/*.sh`), native handler references.
- Loop exit conditions and iteration caps.
- Fan-out source and item-handling rules.
- Human gate placement and per-iteration human checks.
- Per-stage posture, model_role, tool, and budget overrides.
- Operator identity overrides (`operator.md`, `operator.overlay.md`).
- Workspace-level defaults (`icm.yaml`).
- Error policy (`on_error`).
- Workspace-bundled skills (`shared/skills/<name>/`, `stages/NN_slug/skills/<name>/`).

**In the plugin's YAML config (`schema.json`):**

- `workspace`: absolute or `~`-relative path to the workspace folder. Required.
- `default_judge_posture`: posture name for `type: llm` predicates that omit `model:`.
- `default_workflow_posture`: base posture name layered under every stage.
- `cache_size`: per-run delegate cache capacity. Default 0 (disabled — ICM stages typically have tool side effects and HITL gates that make cross-run caching hostile).
- `inline_artifact_limit_bytes`: maximum size for inlining an artifact body into the XML payload. Default 32768.
- `loop_max_restarts`: per-stage cap on loop-exhaustion restarts. Default 3; 0 = unlimited.
- `input_filename`: name for the file written into `<runID>/00_input/` when io.input carries direct content. Default `input.txt`.
- `treat_input_as_path_if_exists`: when true, io.input.Content is interpreted as a file path if `os.Stat` succeeds. Default true.
- `workspace_inputs_dir`: optional directory whose regular files are copied into `<runID>/00_input/` at run start.
- `auto_include_skill_reference_tool`: when true (default), the `read_skill_reference` tool is auto-appended to each derived posture whose stage declares `inputs.skills`.
- `predicate_command_timeout_seconds`: default timeout for `type: command` predicates. Default 30.
- `emit_progress_thinking_steps`: when true (default), the plugin emits `thinking.step` events with `Phase="icm.<stage_id>"` so UIs render stage transitions inline.

**Not workspace-configurable:**

- LLM provider, API keys, network transport.
- Tool name → implementation mapping (handled by the catalog plugin).
- Native predicate handler registry (the four shipped handlers + any host-registered ones).
- Session storage.
- The XML payload structure delivered to sub-agents.
- The fixed LLM judge response schema (`{verdict, feedback, score?}`).

If a workspace author wants to change workflow behavior, they edit files. If you find yourself wanting them to touch plugin YAML for workflow logic, the workspace spec needs a new file-driven knob.

## In-process Go vs LLM tool calls

Prefer in-process Go over LLM invocation whenever the work can be expressed cleanly without semantic judgment.

The design pushes this hard:
- Loader, orchestrator state machine, path resolution, payload assembly, aggregation, sidecar writes — all in-process Go.
- Schema, regex, native, and command predicates — in-process Go or local subprocess.
- LLM calls happen only for: stage sub-agent work, `type: llm` predicates, and verifier sub-agents.

When authoring, walk the predicate ladder cheapest-first (see [`predicates.md`](predicates.md)). For mechanical cross-stage checks ("every ID in stage 1 appears in stage 2"), use a `native:contains_required_ids` predicate, not a verifier stage.

## Posture as the stage's runtime contract

At `Ready()` time, the plugin derives a `posture.AgentPosture` for each stage and verifier and registers it with the posture registry (capability `posture.registry`, provided by `nexus.agent.postures`).

Posture name format (from `plugins/workflows/icm/runtime/posture.go:122`):
- Default instance: `icm.<stageID>`, e.g. `icm.02_script`.
- Suffixed instance: `icm.<suffix>.<stageID>`, e.g. `icm.script.02_script` for instance `nexus.workflows.icm/script`.

The derivation chain, for each stage:

1. Seed from the plugin's `default_workflow_posture` (registry lookup).
2. If `stage.agent.posture` is set, look that posture up — it REPLACES the entire base for Model / AllowedTools / Budget / MaxRecursionDepth (the base posture's SystemPrompt is always discarded).
3. Apply per-field overrides: `model_role`, `tools`, `budget`, `max_recursion_depth`.
4. Render the SystemPrompt freshly from the workspace operator template (workspace `operator.md` + overlay, or the embedded default + overlay) plus the stage's `prompt_overlay`.
5. Set `OutputSchema` to the derived schema name when the stage emits JSON.
6. If `auto_include_skill_reference_tool` is true and the stage declares skills, append the instance-scoped `read_skill_reference` tool to AllowedTools.

This is why `agent.model_role` and `agent.posture` are different fields. The posture is the inheritance point — a swap of the entire base configuration. `model_role` is one field of that configuration; the workspace can override it without forking a whole posture.

## Schema registry naming

When a stage declares JSON output or a `type: schema` predicate, the plugin registers the schema with the engine's `SchemaRegistry`. Names (from `posture.go:130`):

- Stage output schema: `icm.[<suffix>.]<stageID>.output`.
- Predicate schema: `icm.[<suffix>.]<stageID>.<predicateName>`.

The predicate name defaults to `<type>_<index>` when unset (e.g. `schema_0`, `validator_0`). Set explicit names for predicates whose schemas you want to address from outside the workspace.

## Session dir layout

Workspace = spec (git-tracked, stable). Session dir = run output (per-run, in Nexus session storage under `~/.nexus/sessions/<sessionID>/files/`).

```
<session>/<runID>/
  00_input/                       initial input copied at run start
    brief.md
  01_research/
    research.md                   the artifact
    research.md.icm.json          sidecar metadata (provenance, validators)
  02_draft/                       looping stage
    iter_01/draft.md
    iter_01/draft.md.icm.json
    iter_02/draft.md
    ...
    draft.md                      written by orchestrator from latest accepted iter
  03_review/                      fan-out stage
    items/
      topic_a/review.md
      topic_a/review.md.icm.json
      topic_b/review.md
      ...
    reviews.json                  aggregate, built mechanically after all items
```

Path conventions:

| Stage type | Artifact path |
|---|---|
| Plain | `<session>/<runID>/<stageID>/<filename>` |
| Looping | `<session>/<runID>/<stageID>/iter_NN/<filename>` (per iter) + finalized `<session>/<runID>/<stageID>/<filename>` |
| Fan-out | `<session>/<runID>/<stageID>/items/<id>/<filename>` (per item) + aggregate `<session>/<runID>/<stageID>/<filename>` |
| Fan-out + loop | `<session>/<runID>/<stageID>/items/<id>/iter_NN/<filename>` (per item/iter) + aggregate |

`<id>` for fan-out items comes from `fan_out.item_id` gojq, with `item_NN` fallback (1-indexed).

## Initial run input

At run start, the orchestrator:

1. Creates `<session>/<runID>/00_input/`.
2. If `workspace_inputs_dir` is set, copies its regular files into `00_input/`.
3. Receives the `io.input` event. If `treat_input_as_path_if_exists` is true and the input content is a file path that exists, copies the file into `00_input/`. Otherwise writes the content as `00_input/<input_filename>`.

Stages reference initial input via `inputs.artifacts: [00_input/<filename>]`. `00_input` is a reserved stage ID — the loader rejects `stages/00_input/`.

## Operator system prompt

The operator system prompt is a Go `text/template` rendered at posture build time (not per turn).

Resolution order (from `plugins/workflows/icm/workspace/validate.go:111`):

1. Workspace `operator.md` if present.
2. Embedded plugin default (`plugins/workflows/icm/defaults/operator.md`) otherwise.
3. If `operator.overlay.md` exists alongside, its body is appended to the result.

The overlay filename is derived from the operator filename: `operator.md` → `operator.overlay.md`. If `layer_names.operator` is overridden to `ops.md`, the overlay is `ops.overlay.md`.

The template receives:

```go
type OperatorTemplateCtx struct {
    Workspace struct {
        Name        string // basename of the workspace root folder
        Description string // first non-empty line of workspace.md
    }
    Stage struct {
        ID        string
        Display   string
        Role      string // contract.md body, post-frontmatter
        Output    struct {
            Format   string // "text" or "json"
            Filename string
            Schema   string // workspace-relative; empty for text
        }
        HumanGate string
    }
}
```

Per-invocation context (turn, iteration, fan-out item, prior outputs, validator feedback) does NOT appear in the system prompt. It goes in the user-turn XML payload. This keeps the system prompt cacheable per stage and gives the operator a single, consistent identity within a stage.

## Plugin lifecycle

1. **Init.** Parse YAML config. Resolve `posture.registry` capability. Build the private `delegate.Runtime`. Build the `predicates.Evaluator` and register the four shipped native handlers. Load the workspace (`workspace.LoadWorkspace`) — load failure aborts boot. Subscribe to `io.input`, `tool.invoke`, `hitl.responded`.
2. **Ready.** Register the judge response schema. Register every per-stage and per-predicate JSON schema. Derive + register a posture per stage and verifier. Register the `icm_validate` tool (`icm_validate_<suffix>` for instance-suffixed plugins).
3. **Runtime.** Each `io.input` begins a new run with a fresh `runID` and orchestrator. Stages dispatch sequentially via the private delegate runtime. Tool invokes for `icm_validate` are handled inline.
4. **Shutdown.** Unsubscribe from the bus. Deregister postures in reverse registration order.

The workspace is loaded once at `Init` and pinned for the lifetime of the plugin. Workspace edits require a plugin restart.

## Multi-instance plugins

The plugin uses the engine's instance-suffix mechanism. Configure multiple instances when you want several workflows active concurrently:

```yaml
plugins:
  - id: nexus.workflows.icm/script
    config:
      workspace: ~/workflows/script
      default_judge_posture: small_judge
  - id: nexus.workflows.icm/research
    config:
      workspace: ~/workflows/research
      default_judge_posture: small_judge
```

Each instance gets:

- An independent workspace and posture set.
- An instance-scoped `read_skill_reference_<suffix>` tool.
- An instance-scoped `icm_validate_<suffix>` tool.
- Instance-suffixed posture names (`icm.<suffix>.<stageID>`).

## Capabilities

The plugin advertises `workflow.runner` and requires `posture.registry`. Multiple ICM instances coexist without conflict; both advertise `workflow.runner` (capability resolution returns the first provider, but capabilities are descriptive rather than exclusive here).
