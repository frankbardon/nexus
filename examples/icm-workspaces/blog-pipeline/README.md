# Blog Pipeline — example ICM workspace

This workspace is the reference demo for `nexus.workflows.icm`. It exists to be **run**, not just read — every major capability the plugin ships is wired into at least one stage so you can watch them fire end-to-end on a single launch.

It turns a one-paragraph topic brief into a publication-ready Markdown blog post via five sequential stages plus a verifier.

## What it demonstrates

| Capability | Stage(s) | How to spot it |
|---|---|---|
| `output.format: json` + JSON Schema | `01_research`, `02_outline` | Each stage emits a structured JSON artifact validated against `schemas/*.json` |
| `schema` predicate | `01_research`, `02_outline` | `output.validators[type: schema]` rejects malformed JSON |
| `native:json_path_exists` | `01_research` | Requires `.key_concepts` to be non-empty |
| `native:word_count_under` | `03_draft_sections` | Hard-caps each fan-out section at 400 words |
| `native:word_count_over` | `04_assemble` | Total draft must exceed 800 words |
| `native:contains_required_ids` | `04_assemble` | `introduction` + `conclusion` headings must be present |
| `regex` predicate | `03_draft_sections`, `04_assemble`, `05_publish`, verifier | First-line anchors enforce H1 / H2 / verdict prefixes |
| `command` predicate (inside `loop.until`) | `04_assemble` | `scripts/link_audit.sh` rejects placeholder / malformed Markdown links each iteration |
| `llm` predicate (judge posture, as a turn validator) | `04_assemble` | `validators/cohesion_quality.md` rubric against `blog_small_judge`; feedback re-enters the same sub-agent via `<previous_attempt>` |
| Stage loop (`loop.until` + mechanical predicates) | `04_assemble` | Iterates up to 3 times; `on_exhausted: human_gate` |
| Fan-out over JSON array | `03_draft_sections` | One sub-agent per `02_outline/outline.json` `.sections[]` entry; per-item folders use `.slug` |
| `turns.policy` — all three values | `01`/`02`/`03`/`04` use `until_valid`, `05` uses `until_human_approves` (`fixed` is the workspace default for any unset stage) | |
| `human_gate: start` | `02_outline` | HITL prompt before drafting begins |
| `human_gate: end` | `04_assemble`, `05_publish` | HITL prompt after the assembly loop converges, and again before final post.md is finalized |
| `output.persist: both` | `05_publish` | Final post.md both inlined for context and written as a file_ref |
| Stage-local skill (`SKILL.md` + `references/`) | `stages/03_draft_sections/skills/section-writer` | Drafter pulls examples via `read_skill_reference` |
| Workspace-shared skill | `shared/skills/brand-voice` (used by `05_publish`) | Polisher consults brand voice + vocabulary references |
| Shared grounding | `shared/grounding/house_rules.md` | Every stage gets it |
| Per-stage grounding | `stages/02_outline/grounding/outline_principles.md` | Only the outline stage sees it |
| Verifier stage | `verifiers/audit_titles/` | Runs after `04_assemble`; checks every outline title survived |
| `agent.prompt_overlay` | every stage | Per-stage operator-prompt refinement |
| `agent.budget` overrides | most stages | Per-stage token / tool-call ceilings on top of posture defaults |
| `00_input/` reserved stage | `01_research`, `02_outline` | Both read `00_input/brief.md` |

## Workflow shape

```
00_input/brief.md            (user supplies a paragraph)
        │
        ▼
01_research → research.json     # JSON + schema + json_path_exists
        │
        ▼
02_outline → outline.json       # JSON + schema, gated by human_gate: start
        │   sections[] becomes fan-out source ──┐
        ▼                                       ▼
03_draft_sections → section.md   # FAN-OUT one item per section
        │   aggregated for downstream ──┐
        ▼                               ▼
04_assemble → draft.md           # LOOPS until 5 exit predicates pass
        │   (word_count_over + contains_required_ids + command +
        │    llm rubric + per-iteration human)
        │
        ├──► verifier: audit_titles (every outline title present?)
        │
        ▼
05_publish → post.md             # HITL approve before final output
```

## Files

```
blog-pipeline/
├── README.md                          this file
├── workspace.md                       Layer 1 workflow description
├── icm.yaml                           workspace defaults (postures, agent baseline)
├── inputs/example_brief.md            sample brief you can paste
├── schemas/                           JSON Schemas for json artifacts
│   ├── research.json
│   └── outline.json
├── scripts/link_audit.sh              command-predicate script (executable)
├── validators/cohesion_quality.md     LLM rubric for the type:llm predicate
├── shared/
│   ├── grounding/house_rules.md       workspace-wide grounding
│   └── skills/brand-voice/            workspace-shared skill + references
├── stages/
│   ├── 01_research/contract.md
│   ├── 02_outline/
│   │   ├── contract.md
│   │   └── grounding/outline_principles.md
│   ├── 03_draft_sections/
│   │   ├── contract.md
│   │   └── skills/section-writer/     stage-local skill + references
│   ├── 04_assemble/contract.md
│   └── 05_publish/contract.md
└── verifiers/audit_titles/contract.md
```

## How to run it

From the repo root:

```sh
make build
ANTHROPIC_API_KEY=... bin/nexus -config examples/icm-workspaces/blog-pipeline.yaml
```

That config:
1. Loads the six `blog_*` postures from `examples/postures/` (no manual posture authoring required).
2. Points `nexus.workflows.icm` at this workspace.
3. Wires the Anthropic provider for every model role used by the postures (`writer`, `editor`, `researcher`, `planner`, `judge`).
4. Activates `nexus.control.hitl` so `human_gate` and `type: human` predicates can pause for input.

When the TUI prompts, paste a paragraph like the one in `inputs/example_brief.md`. Each stage's artifacts land under `~/.nexus/sessions/<id>/<runID>/<stage_id>/`.

### Scripted variant

Swap `nexus.io.tui` for `nexus.io.oneshot` in the config to launch with a pre-filled brief:

```yaml
plugins:
  active:
    - nexus.io.oneshot      # replaces nexus.io.tui
    # ... rest unchanged
  nexus.io.oneshot:
    input_file: ./examples/icm-workspaces/blog-pipeline/inputs/example_brief.md
```

You will still need to answer the HITL prompts at `02_outline`, the per-iteration writer signoff in `04_assemble`, and the `05_publish` approval — they go through `nexus.control.hitl`, not `io.input`.

## Validating after edits

After any change to `contract.md`, `icm.yaml`, schemas, scripts, validators, or skills, ask the running agent to call the `icm_validate` tool with `{"workspace": "<abs path>"}`. The loader aggregates every error in one pass so iteration is fast. Alternatively, write a tiny Go program that calls `workspace.LoadWorkspace` directly — that is what the plugin does at boot.

## Editing tips

- **Adding a stage**: drop a new `stages/NN_<slug>/` folder. The numeric prefix must be unique and sequential. The folder name regex is `^\d+_[a-z0-9_]+$` — no dashes, no uppercase.
- **Changing word caps**: edit the `args.max_words` / `args.min_words` in the relevant `native` predicate. Update the matching language in `shared/grounding/house_rules.md` so the agent sees the new contract.
- **Adding required headings**: add the new ID to `04_assemble`'s `contains_required_ids` predicate, and add a sentence to `04_assemble`'s body telling the editor to insert it.
- **Switching providers**: edit only `core.models` in `blog-pipeline.yaml`. The postures and contracts reference model **roles**, not models, so a one-line provider swap is enough.
- **Tightening the judge rubric**: edit `validators/cohesion_quality.md`. Hand-feed it a known-bad draft to confirm it catches the failure before re-running the full pipeline.
