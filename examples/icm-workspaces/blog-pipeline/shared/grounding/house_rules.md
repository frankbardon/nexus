# House rules

Workspace-wide constraints. Every stage gets these. Do not override locally.

## Output discipline

- Stages that declare `output.format: json` must emit ONLY a JSON document — no preamble, no commentary, no Markdown fence.
- Stages that emit Markdown must emit clean Markdown — no backtick fence around the whole document, no `> NOTE:` admonitions.
- One artifact per stage. Never bundle multiple files into one output.

## Citation discipline

- Every factual claim that didn't originate in the input brief must be either grounded in `01_research/research.json` claims or marked as the writer's inference.
- Inline phrasing only ("the research notes that..."). No footnote syntax, no `[1]` markers.

## Length discipline

- Per-section hard cap: 400 words (enforced by `word_count_under` in `03_draft_sections`).
- Total floor: 800 words (enforced by `word_count_over` in `04_assemble`).
- Reach the floor by writing more sections, not by padding existing ones.

## Identifier hygiene

- The H1 title is set in `04_assemble` and SHOULD NOT change in `05_publish` without explicit human direction.
- Section H2 headings come from `02_outline.sections[].title` and MUST appear verbatim in the final post.

## What stages must NOT do

- No stage may invent new sections after `02_outline`. The outline is the structural contract.
- No stage may rewrite a research `claim`. Claims are append-only after `01_research`.
- No stage may invoke external HTTP except via tools explicitly granted in its `agent.tools` list.
