---
name: brand-voice
description: Authoritative voice, vocabulary, and grammar conventions for the publication. Consult before any final polish; the post WILL get rejected if it ships in a generic LLM tone.
---

# Brand voice

We publish technical posts for senior practitioners. The voice is direct, declarative, and uses concrete examples instead of generalizations.

## Always

- Active voice. "The cache invalidates after 5 minutes" — not "the cache is invalidated".
- Specific quantities. "Reduces input tokens by ~75%" — not "significantly fewer tokens".
- One idea per sentence. Two sentences with one idea each beats one sentence with two.
- Code, identifiers, and shell commands in backticks. CamelCase API names in backticks even mid-sentence.
- En-dashes for ranges, em-dashes for parenthetical interruptions.

## Never

- "In this post we'll explore..." — and any other meta-narration.
- "Easily", "simply", "just" before any verb. They are lies.
- "Best practices", "leverage", "robust solution", "go-to choice".
- "It's worth noting that..." — if it's worth noting, just note it.
- Rhetorical questions in the body. Allowed once in the introduction.

## Headings

- H1 in title case.
- H2 in title case for major sections, sentence case for "Introduction" / "Conclusion".
- No emoji in headings.

## References

For more detail, the agent should call `read_skill_reference` for these reference files:

- `tone_examples.md` — paired bad/good rewrites.
- `vocabulary.md` — preferred terms and their disallowed alternatives.
