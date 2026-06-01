---
id: 04_assemble
display: Assemble and iterate the full draft
turns:
  policy: until_valid
  max: 2
human_gate: end
on_error: halt
loop:
  # Loop exits when every predicate below passes. Keep these strictly
  # mechanical so convergence is decidable; quality checks live in
  # output.validators below, where the agent gets feedback per turn
  # within an iteration.
  max_iterations: 3
  until:
    - type: native
      name: above_total_floor
      handler: word_count_over
      args:
        min_words: 800
    - type: native
      name: all_section_titles_present
      handler: contains_required_ids
      args:
        ids: [introduction, conclusion]
        case_insensitive: true
    - type: command
      name: link_audit
      run: scripts/link_audit.sh
      timeout_seconds: 20
  on_exhausted: human_gate
output:
  format: text
  persist: file_ref
  filename: draft.md
  validators:
    - type: regex
      name: starts_with_h1
      pattern: '^# .+'
      anchor: first_line
      message: Draft must open with a single H1.
    - type: llm
      name: cohesion_quality
      rubric: validators/cohesion_quality.md
inputs:
  artifacts:
    - 02_outline/outline.json
    - 03_draft_sections/section.md
  shared_grounding:
    - house_rules.md
agent:
  posture: blog_editor
  model_role: editor
  prompt_overlay: |
    You are weaving N independently-drafted sections into one piece.
    Add an `# H1` title and a 2-3 sentence "Introduction" before the
    first section. Add a "Conclusion" before signing off. Smooth the
    seams between sections — repeated definitions, jarring tonal
    shifts, dangling references. Do not rewrite section bodies whole.
  budget:
    max_tokens: 16000
    max_tool_calls: 8
verifiers:
  - audit_titles
---

# Process

You have the outline (`02_outline/outline.json`) and the fan-out aggregate (`03_draft_sections/section.md`). The aggregate is a mechanical concatenation; your job is to make it read as one cohesive post.

**Steps**

1. Open with `# <outline.title>`.
2. Write an `## Introduction` section (2-3 sentences) that sets up what follows. Use the outline `summary` as raw material; do not paste it.
3. Insert each drafted section in outline order. Keep the H2 headings verbatim — a verifier checks every outline `title` survives the assembly.
4. Smooth transitions. When two adjacent sections both define the same concept, keep the stronger definition and excise the duplicate.
5. Close with `## Conclusion` (2-3 sentences). Restate the central claim from the research note.

**Iteration**

If any loop predicate fails, you will be re-dispatched with a `<previous_iteration>` block describing what failed. Address each named failure explicitly; do not silently rewrite unrelated parts.

Output ONLY the assembled Markdown.
