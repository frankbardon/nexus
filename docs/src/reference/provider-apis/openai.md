# OpenAI API — surface tracking

- **Last verified:** 2026-09-22
- **Plugin:** `nexus.llm.openai` (`plugins/providers/openai/`)
- **Batch surface:** `plugins/llm/batch/openai.go`, `openai_responses.go`, `openai_surface.go`

## How this API is versioned

There is **no version header** on the direct API. The version axis is the
**surface**: `/v1/chat/completions` and `/v1/responses` are two different APIs
with different request shapes, different streaming event models, and different
feature sets — and OpenAI ships new capability to Responses first.

Azure is versioned separately: a required `api-version` query on the
deployment-scoped chat route, and an implicitly-versioned `/openai/v1/` route
for Responses (preview features still need `api-version=preview`).

**The practical consequence for Nexus:** "which OpenAI API version do we
support" is answered by `api:`, an operator-declared key
(`plugins/providers/openai/api.go`), not by a header. Divergence between the two
surfaces is the thing to track, because it only widens.

As of the Sept 2026 changelog, GPT-6 Astra **requires Responses for tool
calling**, and async tool calling, mid-turn steering and mid-conversation
reasoning-effort changes all launched on Responses only. Chat Completions has
not been deprecated, but it is no longer where the API grows.

## What Nexus pins

| Thing | Value | Set in |
|---|---|---|
| Surface (unnarrowed default) | `responses` | `api.go` (`unnarrowedDefaultAPI`) |
| Surface when `base_url` or Azure auth is set | `chat_completions` | `api.go` (`narrowsToChatCompletions`) |
| Chat endpoint | `https://api.openai.com/v1/chat/completions` | `plugin.go` |
| Responses endpoint | `https://api.openai.com/v1/responses` | `plugin.go` (`responsesURL`) |
| Azure chat | `/openai/deployments/<dep>/chat/completions?api-version=<v>` | `auth.go:222` |
| Azure Responses | `/openai/v1/responses`, deployment in body `model` | `auth.go` |
| Azure `api-version` | operator-supplied, required | `auth.go:189` |
| Batch line endpoint | follows the role's `api:`, default `responses` | `plugins/llm/batch/openai_surface.go` |

## Feature matrix

| Feature | Chat Completions | Responses | Nexus | Notes |
|---|---|---|---|---|
| Text generation | ✅ | ✅ | ✅ both | |
| Streaming | ✅ | ✅ typed SSE | ✅ both | Separate readers; Responses via `responses_stream.go` |
| Tool calling | ⚠️ **not with `reasoning_effort` other than `none` from GPT-5.4** | ✅ | ✅ both | This restriction is why `responses` is the default |
| `reasoning_effort` / `reasoning.effort` | ✅ | ✅ | ✅ both | Vocabulary `none\|minimal\|low\|medium\|high\|xhigh\|max`, unclamped |
| Reasoning summaries | ❌ none returned | ✅ `reasoning.summary` | ✅ Responses only | → `thinking.step` since v0.29.0 |
| Encrypted reasoning replay | n/a | ✅ `encrypted_content` under `store: false` | ✅ Responses only | `pkg/roundtrip` `openai_reasoning_items` |
| Structured output | `response_format` | `text.format` | ✅ both | |
| Multimodal image/file | `image_url` | `input_image` / `input_file` | ✅ both | Files-by-id only on Responses |
| Files API | ✅ | ✅ | ✅ | Always `api.openai.com`, even in Azure modes |
| Predicted Outputs (`prediction`) | ✅ | ❌ **no equivalent** | ⚠️ chat only | Dropped with one warning per role on Responses |
| Prompt caching | automatic | automatic | n/a | No config surface; not per-role |
| Prompt cache diagnostics | ❌ | ✅ GA Sep 2026 | ❌ | Would explain cache misses |
| Batch API | ✅ `/v1/chat/completions` lines | ✅ `/v1/responses` lines | ✅ both | Follows the role's `api:` since v0.29.0 |
| Azure | ✅ | ✅ | ✅ both | Responses batch on Azure **unverified**, not claimed |
| `reasoning.context` (`auto\|current_turn\|all_turns`) | n/a | ✅ GPT-5.6+ | ❌ | Not exposed |
| Async tool calling | ❌ | ✅ Sep 2026 | ❌ | |
| Mid-turn steering | ❌ | ✅ Sep 2026 | ❌ | |
| Mid-conversation effort changes | ❌ | ✅ Sep 2026 | ❌ | Nexus sets effort per request |
| Agents API | — | beta Sep 2026 | ❌ | Out of scope — Nexus is the agent harness |

## Changes needing accommodation

**Open, ranked.**

1. **Two body builders for one wire format.** `plugins/providers/openai/responses.go`
   and `plugins/llm/batch/openai_responses.go` are independent implementations.
   They agree today; a conformance suite pinning them was added in v0.29.0
   (E6-S2). Any Responses shape change must be applied to both.
2. **Responses has only run against mocks.** Every wire assumption is a
   hypothesis until real traffic. Two shapes were asserted from knowledge rather
   than a fetched spec: nested `input_audio`, and a content-parts array on
   `function_call_output.output` for a tool returning media.
3. **Replay-rejection rate is unmeasured.** Field reports describe
   `encrypted_content` failing verification after 3-4 tool rounds under
   `store: false`. Nexus fails the request loudly when that happens. If the rate
   is material, long agent loops on OpenAI become unusable and the policy needs
   revisiting.
4. **Cross-model-family replay** is undetected by design — detection would
   require a model table. Surfaces as a request failure naming it as one of two
   causes.

**Shapes of change to watch for.**

- Any Responses request-shape change — it now lands in two places.
- Chat Completions losing a capability Nexus still relies on for
  `base_url`-narrowed deployments. The compat-endpoint ecosystem (vLLM, Ollama,
  OpenRouter, LM Studio) tracks Chat Completions, so this is the surface that
  strands them.
- Azure `/openai/v1/` moving from implicit versioning, or Responses batch support
  landing (currently unverified, so unclaimed).
- A new `reasoning.effort` value. Nexus does not clamp, so a new word passes
  through — good — but the `Init` allowlist would reject it.

## References

- Changelog (**the page to diff against `Last verified`**) — https://developers.openai.com/api/docs/changelog
- Migrate to Responses — https://developers.openai.com/api/docs/guides/migrate-to-responses
- Reasoning models — https://developers.openai.com/api/docs/guides/reasoning
- Responses streaming events — https://developers.openai.com/api/reference/resources/responses/streaming-events
- Batch API — https://developers.openai.com/api/docs/guides/batch
- Azure Responses — https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/responses
- Azure API version lifecycle — https://learn.microsoft.com/en-us/azure/foundry/openai/api-version-lifecycle
