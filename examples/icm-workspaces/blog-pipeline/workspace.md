# Blog Post Pipeline

A five-stage ICM workspace that turns a one-line topic brief into a publication-ready Markdown blog post. Built as the reference demo for `nexus.workflows.icm`, it deliberately exercises every major feature of the plugin:

| Feature | Where |
|---|---|
| JSON output + JSON Schema validation | `01_research`, `02_outline` |
| `native:json_path_exists` predicate | `01_research` |
| `native:word_count_under` predicate | `03_draft_sections` |
| `native:word_count_over` predicate | `04_assemble` |
| `native:contains_required_ids` predicate | `04_assemble` |
| `schema` predicate | `01_research`, `02_outline` |
| `regex` predicate | `04_assemble` |
| `command` predicate (shell script, in loop) | `04_assemble` (link audit each iteration) |
| `llm` predicate with judge posture | `04_assemble` (per-turn cohesion rubric) |
| Stage loop (`loop.until` over mechanical predicates) | `04_assemble` |
| Per-turn validator retry (`turns.policy: until_valid`) | `04_assemble` (cohesion rubric retries within an iteration) |
| Fan-out over a JSON array artifact | `03_draft_sections` |
| Stage-local skill | `stages/03_draft_sections/skills/section-writer` |
| Workspace-shared skill | `shared/skills/brand-voice` |
| HITL human gate | `04_assemble` and `05_publish` (`human_gate: end`) |
| Verifier stage | `verifiers/audit_titles` (runs after `04_assemble`) |
| Shared grounding | `shared/grounding/house_rules.md` |
| Per-stage grounding | `stages/02_outline/grounding/outline_principles.md` |

## Input

The run starts with a single file dropped into the session input slot under the logical name `00_input/brief.md` — a short freeform topic brief (e.g. "How prompt caching changes the unit economics of agents"). The orchestrator binds it as the `00_input` artifact and Stage 01 reads it as its first input.

## Output

The final artifact is `05_publish/post.md` — a complete Markdown blog post ready to paste into a static site generator.

## Cadence

One-shot. Each run produces one post. There is no recurring scheduler here; the human decides when to launch.

## Conventions

- All postures referenced in this workspace use the **role/posture** form, never raw model identifiers. Six postures ship with this example under `examples/postures/`: `blog_workflow_default`, `blog_small_judge`, `blog_writer`, `blog_editor`, `blog_researcher`, `blog_planner`. Point `nexus.agent.postures.scan_dirs` at that directory before booting.
- Word counts: per-section ceiling 400, total floor 800.
- Required structural anchors: every section title from `02_outline/outline.json` must appear verbatim as an `## H2` in the final assembled draft.

## Editing this workspace

After any change, run the plugin's `icm_validate` tool with `{"workspace": "<abs path>"}`. The loader will accumulate every error in one pass; fix them, repeat.
