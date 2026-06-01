---
id: 05_publish
display: Final polish + human approval
turns:
  policy: until_human_approves
  max: 3
human_gate: end
on_error: human_gate
output:
  format: text
  persist: both
  filename: post.md
  validators:
    - type: regex
      name: has_h1
      pattern: '^# .+'
      anchor: first_line
inputs:
  artifacts:
    - 04_assemble/draft.md
  shared_grounding:
    - house_rules.md
  skills:
    - brand-voice
agent:
  posture: blog_editor
  model_role: editor
  prompt_overlay: |
    You are NOT rewriting. You are tuning. Voice + grammar + the small
    things that mark this post as "ours". A human will approve before
    publish — do not surprise them.
  budget:
    max_tokens: 16000
    max_tool_calls: 4
---

# Process

The draft from `04_assemble/draft.md` is structurally complete. Your job: tune voice to match the `brand-voice` skill, fix mechanical issues, polish without rewriting.

**Allowed**

- Sentence-level edits for clarity and pacing.
- Vocabulary swaps to match brand voice (consult the skill; use `read_skill_reference` for the tone examples).
- Punctuation, grammar, and formatting fixes.
- Adding or tightening section transitions (one or two sentences max).
- Removing redundancy.

**Not allowed**

- Re-ordering sections.
- Adding or removing sections.
- Changing the H1 title without an explicit human request.
- Rewriting claims or evidence.

A human reviews the result before publish. Treat their feedback in the `<previous_iteration>` block as authoritative; address each note before producing the next version.

Output ONLY the final post Markdown.
