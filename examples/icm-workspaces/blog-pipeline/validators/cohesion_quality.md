# Cohesion quality rubric

You are judging whether the assembled draft reads as one cohesive blog post or as five glued-together sections.

Respond with the standard ICM judge schema: `{"verdict": "pass" | "fail", "feedback": "...", "score": 0.0..1.0}`.

**Output format — strict**

Emit ONLY the raw JSON document. No Markdown code fence (no triple backticks). No preamble ("Here is my verdict..."). No commentary after. The parser reads bytes 0..N as JSON; anything else is rejected as malformed.

## Pass when ALL of the following hold

1. **Single voice.** Tone, vocabulary, and sentence rhythm are consistent across sections. No section reads like a different author wrote it.
2. **No internal duplication.** No concept is defined more than once. No phrase is repeated near-verbatim across sections.
3. **No dangling references.** Phrases like "as we'll see", "as discussed above", "the previous section" only appear when the referent actually exists in the correct direction.
4. **Smooth seams.** Adjacent sections connect via at least one transitional sentence. A blunt section break with no bridge is a fail.
5. **Intro and conclusion frame the body.** The introduction names the central claim and previews how the body supports it. The conclusion returns to that claim with the body's evidence behind it.
6. **No structural drift.** No section has expanded into another's territory. Each H2 still answers exactly the question its title implies.

## Auto-fail conditions

- Two or more sections define the same key concept independently.
- An introduction or conclusion is missing or is a copy-paste of the outline summary.
- A section ends with a question and the next section ignores it.
- A claim contradicts itself across sections.
- Markdown is malformed (broken headings, unclosed code fences, list items without parent).

## Feedback discipline

In the `feedback` field, name the specific issue and the specific location. Quote ~10 words of the offending text so the writer can find it. Do not summarize ("improve cohesion"); enumerate ("Section 2 redefines 'prompt caching' which Section 1 already defined as ...").

## Score (optional)

0.0 = unusable, 0.5 = recoverable in one more pass, 0.8 = ship-with-polish, 1.0 = ship as is.
