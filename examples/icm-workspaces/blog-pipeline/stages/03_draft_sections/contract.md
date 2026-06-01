---
id: 03_draft_sections
display: Draft each section (fan-out)
turns:
  policy: until_valid
  max: 2
human_gate: none
on_error: halt
fan_out:
  source: 02_outline/outline.json
  jsonpath: .sections
  item_var: section
  item_id: .slug
  max_parallel: 1
  on_item_failure: continue
output:
  format: text
  persist: file_ref
  filename: section.md
  validators:
    - type: regex
      name: starts_with_h2
      pattern: '^## .+'
      anchor: first_line
      message: Each section must begin with the H2 heading from the outline.
    - type: native
      name: section_under_cap
      handler: word_count_under
      args:
        max_words: 400
inputs:
  artifacts:
    - 01_research/research.json
    - 02_outline/outline.json
  shared_grounding:
    - house_rules.md
  skills:
    - section-writer
agent:
  posture: blog_writer
  model_role: writer
  prompt_overlay: |
    You are writing ONE section in isolation. Do not introduce the
    post, do not conclude the post, do not refer to "the previous
    section". Other instances are drafting in parallel.
  budget:
    max_tokens: 6000
    max_tool_calls: 6
---

# Process

Each invocation receives one `<fan_out_item key="section">` containing `{slug, title, focus, target_words}`. Your job: produce a single self-contained section.

**Rules**

1. The first line MUST be `## <title>` — copy the title verbatim from the item.
2. Stay under 400 words. The orchestrator hard-caps you; aim for `target_words` ± 20%.
3. Stick to the `focus`. Do not drift into other sections' territory — they are being drafted in parallel by other instances.
4. Cite a research `claim` or `key_concept` when it grounds an assertion. Use inline phrasing like "as noted in the research...", not footnote syntax.
5. Consult the `section-writer` skill for tone and structural conventions. Use `read_skill_reference` if you need the examples.

Output ONLY the section Markdown — no preamble, no commentary.
