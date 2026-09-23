# Gemini API — surface tracking

- **Last verified:** 2026-09-22
- **Plugin:** `nexus.llm.gemini` (`plugins/providers/gemini/`)
- **Batch surface:** none — Gemini is not wired into `nexus.llm.batch`

## How this API is versioned

The version is **in the URL path**, and it differs by deployment:

- **AI Studio** (`generativelanguage.googleapis.com`): `v1beta` or `v1`. `v1beta`
  is the preview channel — early features, subject to breaking change. `v1` is
  stable. All models are served by both.
- **Vertex AI** (`<region>-aiplatform.googleapis.com`): `v1`, a different API
  surface with its own shapes, not merely a different version of the same one.

Nexus sends `v1beta` on AI Studio and `v1` on Vertex.

**The practical consequence for Nexus:** the AI Studio path is on the preview
channel by construction, which is what makes explicit context caching reachable
there. It also means a `v1beta` breaking change is in-contract for Google and
will arrive without a version bump.

## What Nexus pins

| Thing | Value | Set in |
|---|---|---|
| AI Studio generateContent | `https://generativelanguage.googleapis.com/v1beta/models/<model>:<op>` | `auth.go:358` |
| AI Studio cachedContents | `https://generativelanguage.googleapis.com/v1beta/cachedContents` | `auth.go:405` |
| AI Studio file upload | `/upload/v1beta/files` | `files.go:80` |
| Vertex | `https://<region>-aiplatform.googleapis.com/v1/projects/<p>/locations/<l>/publishers/google/models/<model>:<op>` | `auth.go:351` |
| Vertex cachedContents | `.../v1/.../cachedContents` | `auth.go:402` |

## Feature matrix

| Feature | API | Nexus | Notes |
|---|---|---|---|
| `generateContent` / streaming | GA both versions | ✅ | |
| Tool calling | GA | ✅ | `tool_choice.go` |
| `thinkingConfig.thinkingLevel` | 3.x | ✅ | `mode: level`, `minimal\|low\|medium\|high` |
| `thinkingConfig.thinkingBudget` | 2.5 | ✅ | `mode: budget`, incl. `-1` dynamic / `0` off |
| Mutual exclusion of the two | enforced by API (400) | ✅ | Guaranteed by `thinking.go` merge |
| `includeThoughts` | GA | ✅ | Emission ungated — a role's value works end to end |
| Thought signatures | GA | ✅ | `pkg/roundtrip` `gemini_thought_signatures` — load-bearing, a 400 without it |
| Per-role thinking block | n/a (Nexus concept) | ✅ | Since v0.29.0 |
| Explicit context caching | **v1beta only** on AI Studio | ⚠️ | Nexus is on `v1beta` there, so reachable. **Vertex path uses `/v1/cachedContents` — support unverified.** |
| Code execution | GA | ✅ | `code_execution` config key |
| Multimodal | GA | ✅ | `multimodal.go`, `modality.go` |
| Files API | GA | ✅ | `files.go` |
| Vertex AI auth | GA | ✅ | `auth.go`, incl. Workload Identity |
| Native web search grounding | GA | ✅ | `plugins/search/gemini_native/` |
| Batch API | GA | ❌ | Gemini is absent from `nexus.llm.batch` entirely |
| Live API | v1beta only | ❌ | Bidirectional streaming; no Nexus surface |
| Agents / Webhooks API | v1beta only | ❌ | Out of scope — Nexus is the agent harness |
| Computer Use tool | v1beta only | ❌ | |
| MCP server tools (native) | v1beta only | ❌ | Nexus has its own MCP client |
| Priority / Flex service tiers | v1beta only | ❌ | Cost-relevant; no config surface |
| Audio output config | v1beta only | ❌ | |
| Interactions API | separate surface | ❌ | Explicitly out of scope — different field names |

## Changes needing accommodation

**Open, ranked.**

1. **Vertex explicit caching is unverified.** The v1beta-only rule is documented
   for AI Studio; Nexus calls `/v1/cachedContents` on Vertex and nothing in this
   repo confirms that route behaves the same. Worth a live check before anyone
   relies on per-role caching against Vertex.
2. **`cache.min_tokens` is inert.** The token-count probe was never implemented
   and `lookup` is read-only, so the key parses and does nothing. Documented in
   the reference as of v0.29.0.
3. **No batch support.** Gemini has a Batch API; `nexus.llm.batch` covers
   Anthropic and OpenAI only. A deployment batching a Gemini role has no path.
4. **`v1beta` is a preview channel.** Breaking changes there are in-contract.
   Moving AI Studio to `v1` would cost explicit context caching — a real
   trade-off, not a free hardening.

**Shapes of change to watch for.**

- A v1beta feature graduating to v1, which may change its shape on the way.
- A new `thinkingLevel` value. Nexus clamps Anthropic's `xhigh`/`max` down to
  `high`; a genuinely new Gemini level would need adding to the vocabulary.
- Thought-signature handling changes — this path is load-bearing and its absence
  is an immediate `400 INVALID_ARGUMENT`.

## References

- API versions explained (**v1 vs v1beta**) — https://ai.google.dev/gemini-api/docs/api-versions
- Release notes (**the page to diff against `Last verified`**) — https://ai.google.dev/gemini-api/docs/changelog
- Thinking — https://ai.google.dev/gemini-api/docs/thinking
- Context caching — https://ai.google.dev/gemini-api/docs/caching
- Vertex AI Gemini API — https://cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference
