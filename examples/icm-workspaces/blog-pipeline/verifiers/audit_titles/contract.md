---
id: audit_titles
display: Verify every outline title survived assembly
turns:
  policy: fixed
  max: 1
human_gate: none
on_error: human_gate
output:
  format: text
  persist: file_ref
  filename: audit.md
  validators:
    - type: regex
      name: explicit_verdict
      pattern: '^(PASS|FAIL):'
      anchor: first_line
inputs:
  artifacts:
    - 02_outline/outline.json
    - 04_assemble/draft.md
agent:
  posture: blog_editor
  model_role: editor
  prompt_overlay: |
    You are a deterministic auditor. Be terse. No commentary.
  budget:
    max_tokens: 2000
    max_tool_calls: 0
---

# Process

You have the outline JSON and the assembled draft. Confirm that every section title from `02_outline/outline.json` appears verbatim as an `## H2` line in `04_assemble/draft.md`.

**Output format**

First line, exact shape: `PASS:` or `FAIL: <reason>`. After the first line, list any missing titles, one per line, prefixed with `- missing: `.

Examples:

```
PASS:
```

```
FAIL: 2 titles missing
- missing: The Cost Model
- missing: Where It Breaks
```

No other output.
