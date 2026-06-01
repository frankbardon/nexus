# Outline principles

Stage-local grounding for `02_outline`. Conventions for building an outline that the fan-out drafter can use without further clarification.

## Section count

| Brief shape | Sections |
|---|---|
| Single sharp argument | 3 |
| Argument + counter-argument + synthesis | 4 (default) |
| Survey of N related approaches | N + 1 (intro/synthesis) |
| Deep technical post with prereqs | 5 |
| Multi-system comparison or retrospective | 6 (max) |

If you find yourself wanting 7+ sections, the post is two posts.

## Section title rules

- Title case. No questions. No clickbait constructions ("The One Weird Trick...").
- Each title describes the *answer*, not the question. "The Cost Model" beats "What Does It Cost?"
- The first section is typically scene-setting; the last is typically synthesis. The body sections are the actual argument.

## Section focus rules

The `focus` field is the contract with the drafter. Good focuses are specific enough that two different writers would produce structurally similar drafts. Bad focuses leave the drafter to guess.

| Bad focus | Good focus |
|---|---|
| Talk about caching. | Explain the 25% / 1.25x cost multipliers, give the break-even at reads = 2, and name the system-prompt-only caching pattern. |
| Discuss when it fails. | Identify the two failure modes — silent TTL expiry and prefix mutation — and give the prod-log evidence for each. |
| Conclude the post. | Restate the central claim that the cache is best treated as ephemeral infrastructure, not a deterministic optimization. |

## Target words

`target_words` is a hint, not a contract. The drafter is hard-capped at 400 by `word_count_under` in `03_draft_sections`. Aim for sections sized to fit comfortably inside that cap.
