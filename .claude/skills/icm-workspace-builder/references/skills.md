# Skills

In an ICM workspace, "skill" refers to a bundle of reference content with progressive disclosure: a `SKILL.md` (frontmatter + body) that loads inline into the agent's context plus an optional `references/` subfolder of files the agent pulls on demand via the `read_skill_reference` tool.

ICM treats skills as a special kind of Layer 3 grounding — same purpose (constrain the agent's behavior), different loading discipline (lazy expansion).

Important: an ICM workspace skill is NOT the same thing as a Nexus-registered skill loaded by the `nexus.skills` plugin. ICM v1 resolves workspace skills only from inside the workspace folder — see "Two skill sources" below.

## Why a skill, not just another grounding file

Skills exist for cases where:

- The full reference content is too large to inline (a couple thousand tokens of guidelines, examples, edge-case docs).
- The agent only needs specific subsections depending on the work in front of it.
- The reference set is reused across multiple stages without copy-paste.
- The reference content has its own internal structure (a design system with separate files for tokens, components, motion).

If your grounding file is approaching a couple thousand tokens and has natural subsections, it is probably a skill in disguise.

## Workspace skill shape

```
<name>/
  SKILL.md                  required — YAML frontmatter + body
  references/               optional — files loaded on demand
    tone.md
    structure.md
    examples/
      good_intro.md
```

`SKILL.md` frontmatter (validated by the loader at `plugins/workflows/icm/workspace/validate.go:835`):

```yaml
---
name: content-style
description: Voice, tone, and structural conventions for video scripts. Use when writing or editing any narrative content for the workflow.
---
```

Required fields, both non-empty: `name` and `description`. `name` MUST match the folder name exactly — mismatch is a load error. Do NOT add `class`, `output_schema`, `allowed_tools`, or any other field — ICM skills v1 deliberately keep the frontmatter minimal.

The `description` is what the agent uses to decide whether to consult the skill at all. Write it pushy enough that the agent actually pulls reference files instead of guessing. Treat it like a tool description.

## Two skill sources

Skills resolve in precedence order, most-local wins. The loader walks two paths only (`plugins/workflows/icm/workspace/validate.go:802`):

1. **Stage-local** — `stages/NN_slug/skills/<name>/`. For skills used by exactly one stage.
2. **Workspace-shared** — `shared/skills/<name>/`. For skills used by multiple stages in the workflow.

A stage-local skill shadows a workspace-shared one of the same name. This lets a single stage tweak a skill's body without forking it across the whole workspace.

**There is no Nexus-registered skill source in v1.** The handoff design floated a third tier (system-wide skills configured in a Nexus `SkillsDir`), but the shipping loader does not look outside the workspace folder. If you reference a name that does not resolve as stage-local or workspace-shared, the load fails with `skill %q not found (looked in stage-local skills/, shared/skills/)`.

## Declaring skills in a contract

```yaml
inputs:
  grounding: [voice.md]
  shared_grounding: [house_style.md]
  artifacts: [01_research/research.md]
  skills:
    - content-style          # resolves via two-source chain
    - tone-guide
```

The skill names must match the regex `^[a-z][a-z0-9-]*$` (`plugins/workflows/icm/workspace/loader.go:17`). Use hyphens, not underscores. `content-style` is valid; `content_style` is not.

The loader resolves each name at load time. Missing skills fail the load with a clear error and the search path it tried.

## The `read_skill_reference` tool

When a stage declares `inputs.skills`, the plugin auto-appends the instance-scoped `read_skill_reference` tool to that stage's posture (controlled by the plugin's `auto_include_skill_reference_tool` config, default `true`). For the default plugin instance the tool is named `read_skill_reference`; for a suffixed instance (e.g. `nexus.workflows.icm/script`) the tool is named `read_skill_reference_script` so multi-instance configurations do not collide. See `plugins/workflows/icm/runtime/posture.go:261`.

Set `auto_include_skill_reference_tool: false` to require explicit `agent.tools` listing per stage.

## How the agent sees skills

The XML payload distinguishes skills from flat grounding (sketch):

```xml
<grounding>
  <skill name="content-style" source="workspace">
    <body>[full SKILL.md content after frontmatter]</body>
    <references_available>
      <ref path="tone.md"/>
      <ref path="structure.md"/>
      <ref path="examples/good_intro.md"/>
    </references_available>
  </skill>
  <file path="grounding/voice.md">...</file>
</grounding>
```

The sub-agent calls `read_skill_reference(skill_name, ref_path)` when its current work matches a reference's description.

## When to make something a skill vs. shared grounding

| Concern | Skill | Shared grounding |
|---|---|---|
| Size | >1500 tokens of reference content | <1500 tokens |
| Structure | Multiple sub-files with distinct concerns | Single file, one concern |
| Loading | Selective, only what is needed | Always in context |
| Reuse | Multiple stages | Multiple stages |
| Authoring | Description-driven (when to use) | Direct reference |

A test: if you would describe the content as "a small library of guidelines", it is a skill. If you would describe it as "the rules", it is grounding.

## Validation at load

For every skill resolved via `inputs.skills`:

- The folder name matches `^[a-z][a-z0-9-]*$`.
- The folder contains a `SKILL.md`.
- The `SKILL.md` parses: valid YAML frontmatter delimited by `---`, non-empty body.
- The frontmatter has non-empty `name` and `description`.
- The frontmatter `name` matches the folder name exactly.
- If a `references/` folder exists, the loader walks it and enumerates every regular file. Non-regular files (symlinks, devices) are an error.

The loader does NOT parse the body or descriptions beyond YAML — the body is loaded as text and references are listed by path.

## Anti-patterns

- **Skills that are really just one file.** If your skill has a SKILL.md and no `references/`, it is grounding. Move it.
- **Skills with vague descriptions.** "Helpful information" is not useful. "Voice and tone conventions for narrative scripts. Use when writing or editing the script body, opening, or closing." is.
- **Copying a skill folder into the workspace just to use it.** ICM v1 does not have a system-wide skill source, but if you want to share a skill across workspaces, the right pattern is a git submodule or symlink at `shared/skills/<name>/`, not duplicating content.
- **Skill folder named with underscores.** The loader rejects names that do not match `^[a-z][a-z0-9-]*$`. Use hyphens.
- **Adding frontmatter fields the loader does not parse.** Extra fields are silently ignored, but future versions may reject them. Stick to `name` + `description`.
- **Cross-stage shared grounding that is secretly a skill.** If `shared/grounding/style_guide.md` keeps growing and acquiring sub-sections, promote it to `shared/skills/style/SKILL.md + references/`.
