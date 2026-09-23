# Anthropic API — surface tracking

- **Last verified:** 2026-09-22
- **Plugin:** `nexus.llm.anthropic` (`plugins/providers/anthropic/`)
- **Batch surface:** `plugins/llm/batch/anthropic.go`

## How this API is versioned

A required `anthropic-version` request header. Anthropic publishes only two
versions ever — `2023-01-01` and `2023-06-01` — and within a version guarantees
existing input and output parameters keep working. It reserves the right to add
optional inputs, add output values, add enum variants (including new streaming
event types), and change error conditions.

**The practical consequence for Nexus:** the version header is not where breakage
comes from. Breakage comes from (a) **beta headers**, each a dated contract that
can graduate or be withdrawn, and (b) **model families** rejecting parameters an
older family accepted — which is not a versioned change at all. The
`provider-thinking-api` effort existed because of exactly (b): `thinking` with
`budget_tokens` became an HTTP 400 on current models with no version bump.

So: watch the release notes and the model cards, not the version string.

## What Nexus pins

| Thing | Value | Set in |
|---|---|---|
| API version | `2023-06-01` | `auth.go:330`, `files.go:155`, `files.go:201`, `plugins/llm/batch/anthropic.go:419` |
| Bedrock body version | `bedrock-2023-05-31` | `auth.go:51` |
| Vertex body version | `vertex-2023-10-16` | `auth.go:52` |
| Files beta | `files-api-2025-04-14` | `files.go:28` |
| Batches beta | `message-batches-2024-09-24` | `plugins/llm/batch/anthropic.go:23` |
| Extended cache TTL beta | `extended-cache-ttl-2025-04-11` | `beta.go:113` |
| PDF beta | `pdfs-2024-09-25` | `beta.go:119` |

The version string is written as a literal in four places rather than a shared
constant. Changing it is a four-site edit — worth knowing before a version bump
is ever needed.

## Feature matrix

| Feature | API | Nexus | Notes |
|---|---|---|---|
| Messages API | GA | ✅ | `plugin.go`, raw `net/http` |
| Streaming SSE | GA | ✅ | via `engine.StreamPublisher` |
| Tool use | GA | ✅ | `tool_choice.go` |
| Extended thinking — `adaptive` | GA | ✅ | `thinking.go`, `mode: adaptive` |
| Extended thinking — `budget_tokens` | GA (older families) | ✅ | `mode: budget`, operator-declared |
| `output_config.effort` | GA | ✅ | plugin-level and per `core.models` role |
| `thinking.display: summarized \| omitted` | GA | ✅ | `thinking.go` |
| `thinking.display: "updates"` | Beta `thinking-display-updates-2026-08-18` | ❌ | Progress updates between tool calls. Not implemented; our enum rejects it. |
| Per-message effort | Beta `mid-conversation-output-config-2026-07-01` | ❌ | Effort is per-request in Nexus, not per-message |
| Thinking-block replay | GA | ✅ | `pkg/roundtrip` `thinking_blocks` |
| Prompt caching | GA | ✅ | `cache.go`, per-role since v0.29.0 |
| Extended cache TTL (1h) | Beta | ✅ | `extended-cache-ttl-2025-04-11` |
| Cache diagnostics | Beta `cache-diagnosis-2026-04-07` | ❌ | Would answer "why did my cache miss" |
| Citations | GA | ✅ | `citations.go` |
| Structured outputs | GA/beta per config | ⚠️ | `structured.go` writes top-level `response_format`; current API is `output_config.format`. **Known stale, deferred twice.** |
| Files API | Beta | ✅ | `files.go` |
| PDF documents | Beta | ✅ | `multimodal.go` |
| Message Batches | Beta | ✅ | `plugins/llm/batch/anthropic.go` |
| Bedrock / Vertex | GA | ✅ | `auth.go` |
| Compaction on demand | Beta `compact-2026-09-04` | ❌ | Nexus compacts in its own memory plugins |
| Inline tools in system messages | Beta `inline-tools-2026-09-15` | ❌ | |
| Turn-scoped system messages | Beta `mid-conversation-system-clear-at-2026-08-21` | ❌ | |
| Server-side fallback | Beta `server-side-fallback-2026-07-01` | ❌ | Nexus fails over itself via `nexus.provider.fallback` |
| Task budgets | Beta `task-budgets-2026-03-13` | ❌ | Nexus has its own gates |
| Fast mode | Beta `fast-mode-2026-02-01` | ❌ | |
| Computer use | Beta | ❌ | `beta.go:131` notes the header shape for it |
| Code execution | Beta | ❌ | ditto |

## Changes needing accommodation

**Open, ranked.**

1. **`response_format` → `output_config.format`** (structured outputs). The
   current API moved it; `structured.go` still writes the old top-level key. Both
   the `provider-thinking-api` and `per-role-reasoning` interviews deferred this
   explicitly. It is the oldest known-stale thing on this provider.
2. **`thinking.display: "updates"`** — a third value our enum rejects at `Init`.
   Cheap to add; the question is whether `thinking.step` should carry the
   distinction between reasoning text and progress updates.
3. **Sampling parameters on current families.** `temperature`/`top_p`/`top_k` are
   an unconditional 400 on current Anthropic model families regardless of
   thinking, and Nexus strips them only when thinking is on. Deferred through two
   efforts, and made easier to hit by per-role `temperature` (v0.29.0).
4. **`thinking.step` gating.** Emission reads the plugin-level `include_thoughts`,
   so a role turning it on gets API text and no events. Documented asymmetry, not
   a provider issue — ours.

**Shapes of change to watch for.**

- A new model family rejecting a parameter an older one accepted, with no version
  bump — the failure mode that caused `provider-thinking-api`. Read model cards,
  not just changelogs.
- A beta header being withdrawn or graduating: withdrawal breaks requests,
  graduation makes our header noise.
- New streaming event types. The version contract explicitly permits adding
  these, so our SSE reader must keep ignoring unknown events rather than erroring.

## Verification basis

Versioning policy and API release notes both fetched directly on 2026-09-22 —
the beta-header list and the recent-changes column come from the release-notes
page itself. Feature rows not named in those two pages (citations, files, PDF,
batches) are read from our own code and cite files; they describe what Nexus
sends, not independent confirmation that the API still accepts it.

## References

- Versioning policy — https://platform.claude.com/docs/en/api/versioning
- API release notes (**the page to diff against `Last verified`**) — https://platform.claude.com/docs/en/release-notes/api
- Messages API reference — https://platform.claude.com/docs/en/api/messages
- Extended thinking — https://platform.claude.com/docs/en/build-with-claude/extended-thinking
- Prompt caching — https://platform.claude.com/docs/en/build-with-claude/prompt-caching
