# Section examples

Three example sections at the target length. Use as shape reference, not content reference.

---

## The Cost Model

Prompt caching charges 25% of the input-token rate on cache reads and 1.25x on cache writes. The break-even hits at the second read of the same prefix; everything past that compounds. For an agent with a 4000-token system prompt invoked 100 times in a 5-minute window, the cache write costs 5000 tokens-equivalent, and the reads save 396,000. That ratio holds across model sizes — the multipliers are fixed.

The corollary: do not cache one-shot prompts. The 1.25x write penalty is unrecoverable when the prefix is never reused. Reserve the cache for the largest stable chunk that every request actually shares — usually the system prompt, sometimes a fixed tool list.

---

## Where It Breaks

Cache invalidation is silent. The 5-minute TTL resets on every read, so a sustained traffic pattern keeps the cache warm indefinitely; an irregular pattern hits the boundary repeatedly. Production logs from December show a 12% cache-miss rate concentrated in the first request of each new conversation — the moments when latency matters most.

The second failure mode is prefix mutation. The cache key is computed over the literal prefix bytes; a single whitespace change invalidates. Teams that template-render their system prompts must pin the renderer's behavior, or the cache hit rate collapses without warning.

---

## What Survives the Audit

Two patterns hold up under the constraints above. First, cache the system prompt and the tool list together as one block — they change in lockstep at deploy time, so a single cache write per deploy covers every subsequent request. Second, treat the cache as ephemeral infrastructure: assume a miss, measure the hit rate as an observable, and budget the cost at the no-cache rate.

The pattern that does NOT survive: caching individual user turns. The hit rate is structurally bounded by user repetition, which empirically tops out near zero.
