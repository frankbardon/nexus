---
id: 01_research
display: Research the topic
turns:
  policy: until_valid
  max: 3
human_gate: none
on_error: halt
output:
  format: json
  schema: schemas/research.json
  persist: file_ref
  filename: research.json
  validators:
    - type: schema
      name: research_well_formed
      schema: schemas/research.json
    - type: native
      name: has_key_concepts
      handler: json_path_exists
      args:
        path: .key_concepts
        must_be_non_empty: true
inputs:
  artifacts:
    - 00_input/brief.md
  shared_grounding:
    - house_rules.md
agent:
  posture: blog_researcher
  model_role: researcher
  prompt_overlay: |
    Bias toward primary evidence. Where you make a claim, attach a
    source. Where you cannot, mark the claim "unsourced" in the
    support field so a later stage can flag it.
  budget:
    max_tokens: 8000
---

# Process

Read the brief at `00_input/brief.md`. Produce a structured JSON research note that downstream stages will consume.

**Steps**

1. Restate the topic in your own words (`topic`). If the brief is ambiguous, pick the most useful framing.
2. Name the audience concretely (`audience`). Roles + seniority + what they will do with this knowledge.
3. List 3–8 `key_concepts`. Each gets a name and a 1-sentence definition. If a concept is jargon, define it from first principles, not by reference.
4. Capture 2+ `claims` the post will make. Each claim has a `statement` (what you assert) and `support` (the evidence). Mark unsupported claims with `support: "unsourced"`.
5. Cite sources where available. Empty list is acceptable; fabricated sources are not.

The schema enforces shape; you enforce substance. Output ONLY the JSON document — no preamble, no Markdown fence.
