---
name: section-writer
description: Conventions for drafting ONE section of a blog post in isolation, without knowing what the adjacent sections will say. Consult when drafting any fan-out section.
---

# Section writer

You are drafting exactly one section. Other instances are drafting the others in parallel. Hold this constraint tightly.

## Structural rules

1. **First line is the H2.** Copy `section.title` verbatim. The orchestrator validates this; do not creatively reformat it.
2. **No cross-section navigation.** Do not write "in the previous section" or "as we'll see later". You do not know what those sections will say. The assembly stage adds the bridges.
3. **No intro to the post and no conclusion.** This section is body text. The introduction and conclusion are separate work.
4. **Word budget is real.** The orchestrator hard-caps at 400 words. Sentences past that point disappear; write the most important content first.

## Substance rules

1. **The `focus` field is the contract.** Answer the question the focus implies and nothing else. If the focus is "explain the cost model of prompt caching", do NOT also cover invalidation, race conditions, or rollout strategy — those are other sections' jobs.
2. **Use the research note.** Cite a `key_concept` definition or a `claim` when it grounds an assertion. Inline phrasing only ("the research notes that...") — no footnotes.
3. **Concrete > abstract.** Replace any sentence describing "what could happen" with one describing "what does happen". Use specific tools, specific numbers, specific cases.

## Opening pattern

The first sentence after the H2 should pose the section's question in concrete terms. Examples:

- "Caching saves money only when the cached prefix is reused; here is when it is and is not."
- "The 5-minute TTL is a hard physical fact; the cost it imposes depends on traffic pattern."

Do not open with "Let's discuss" or any meta-narration.

## Closing pattern

End on the strongest single sentence you can write. Do not summarize within the section — the assembly stage handles summarization. A blunt last sentence with momentum is preferable to a tidy one with none.

## References

For more detail, the agent should call `read_skill_reference` for:

- `section_examples.md` — three full example sections at the target length.
