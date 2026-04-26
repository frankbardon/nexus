# Vortex AI

## Overview
Vortex AI is the developer-first challenger in the agent platform space. YC W23
cohort, Series A of $24M led by Benchmark in 2025. ~45 employees. SF + remote.

## Positioning
Open-core, developer-led, strong on local-first deployment. Their wedge is
"your laptop, then production, no rewrite" — the same agent definitions run
unchanged whether you're prototyping locally or on their managed cloud. Target
buyer is a senior IC or tech lead, not a CIO.

## Product Surface
- Vortex Core — open-source TypeScript SDK (MIT licensed).
- Vortex Cloud — managed hosting with usage-based pricing.
- Vortex Trace — observability/tracing built on OpenTelemetry.
- Vortex Eval — eval harness for agent regression testing.

## Pricing
- Core: free (MIT).
- Cloud: $0.10 per 1M tokens for hosted inference + $0.000002 per agent step.
- No annual minimums; credit card signup.
- Enterprise SKU exists ("call for pricing") but they de-emphasize it.

## Strengths
- True open-source core with active community (12k GitHub stars, 3.4k Discord members).
- Excellent docs — widely cited as the gold standard in the space.
- Local-first dev loop is genuinely first-class, not retrofitted.
- Strong eval tooling — competitors don't really have an answer here.

## Weaknesses
- Thin on enterprise table-stakes (no SOC2 yet, scheduled for Q3 2026).
- TypeScript-only SDK; Python users have to use a community-maintained wrapper.
- No visual builder; everything is code.
- Single-region cloud (us-east-1 only); EU customers blocked on data residency.

## Recent Activity
- 2026-01: Released Vortex Eval 1.0 with regression harness.
- 2026-02: Hired former Datadog VP Engineering as their new CTO.
- 2026-03: Announced "Vortex for Education" free tier for academic use.

## Sources
- Vortex public docs (docs.vortex.ai)
- Vortex GitHub (github.com/vortex/core, contributor stats)
- Founder interview on the Latent Space podcast, episode 142
