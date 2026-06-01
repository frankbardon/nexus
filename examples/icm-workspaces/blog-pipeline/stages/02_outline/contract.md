---
id: 02_outline
display: Outline the sections
turns:
  policy: until_valid
  max: 2
human_gate: start
on_error: halt
output:
  format: json
  schema: schemas/outline.json
  persist: file_ref
  filename: outline.json
  validators:
    - type: schema
      name: outline_well_formed
      schema: schemas/outline.json
inputs:
  artifacts:
    - 00_input/brief.md
    - 01_research/research.json
  grounding:
    - outline_principles.md
  shared_grounding:
    - house_rules.md
agent:
  posture: blog_planner
  model_role: planner
  prompt_overlay: |
    The number of sections you choose IS a design decision. 3 sections
    for a tight argument, 5 for a survey, 6 for a deep technical post.
    Default to 4 unless the brief obviously calls for something else.
  budget:
    max_tokens: 6000
---

# Process

You have the research note and the original brief. Produce an outline that downstream fan-out drafting will iterate over — one item per section.

**Steps**

1. Choose a working `title`. Use the `<title>: <subtitle>` pattern when it sharpens the framing; do not use clickbait constructions.
2. Write a one-paragraph `summary` (40+ chars). This becomes the TL;DR.
3. Decide on 3–6 sections. Each section MUST have:
   - `slug` — filesystem-safe (lowercase, hyphens). This is the per-item folder name during fan-out. Examples: `intro`, `the-cost-model`, `where-it-breaks`.
   - `title` — the verbatim H2 the assembled draft will use. A later stage validates that this exact string appears in the final post.
   - `focus` — what the section must explain. Be specific.
   - `target_words` — soft target between 80 and 380. The drafter will be hard-capped at 400.

Output ONLY the JSON document — no preamble, no Markdown fence.

A human reviews this outline before drafting begins; do not assume a second chance to reshape after fan-out has started.
