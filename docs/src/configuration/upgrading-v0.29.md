# Upgrading to v0.29.0

v0.29.0 made `core.models` roles carry the provider's own native blocks —
`thinking:`, `reasoning:`, `cache:`, `retry:`, `api:`, `temperature:` — and in
the same pass repaired `nexus.llm.openai`, whose reasoning surface had never
reached the wire, and gave it the `/v1/responses` endpoint.

Most of that is additive: a config that sets none of the new keys keeps working.
Seven things are **not** additive, and this page is the complete list. Each one
says what moved, whether it fails loudly or quietly, and the exact edit that
restores the old behaviour.

The [configuration reference](./reference.md) remains canonical for what every
key does today; this page is only about the difference from v0.28.x.

## At a glance

| # | What changed | Who is affected | How it shows up |
|---|---|---|---|
| 1 | [`api:` defaults to `responses`](#1-the-openai-api-default-moved-to-responses) | `nexus.llm.openai` on plain `api.openai.com` with no `api:` set | Silently — a different endpoint, same events |
| 2 | [`reasoningModelPattern` / `force_reasoning` removed](#2-openai-no-longer-detects-reasoning-models) | OpenAI deployments pointed at `o1`/`o3`/`o4`/`gpt-5*` with no `reasoning:` block | Half loudly: a leftover `force_reasoning` key **fails boot**; bare auto-detection fails at request time with an HTTP 400 |
| 3 | [`reasoning.enabled` / `reasoning.budget_tokens` deprecated](#3-openai-reasoningenabled-and-reasoningbudget_tokens-are-deprecated) | OpenAI deployments carrying either key | A warning at `Init`. **Neither fails boot** |
| 4 | [Gemini precedence inverted back](#4-gemini-a-roles-setting-now-beats-the-plugin-block) | Gemini deployments with a plugin-level `thinking.level` **and** a role `effort:` | Silently for the value; loudly for a typo or a clamp that the old order had hidden |
| 5 | [A role `effort:` beats a plugin `reasoning.effort` on OpenAI](#5-openai-a-roles-effort-now-beats-the-plugins) | OpenAI deployments setting both | Silently — a different depth on the wire |
| 6 | [`retry:` and `cache:` blocks are parsed strictly](#6-retry-and-cache-blocks-are-parsed-strictly) | Any provider with `backoff: jitter`, or an unknown/mistyped key in either block | **Fails boot**, naming the key |
| 7 | [Batched OpenAI requests follow `api:`](#7-batched-openai-requests-follow-api) | `nexus.llm.batch` submitting to OpenAI | Silently — a different endpoint per batch, and reasoning now reaching the body |

Nothing on this list can be detected for you from inside Nexus. Where a change
is silent, it is silent because detecting it would need the model-capability
table this effort deliberately deletes.

---

## 1. The OpenAI `api:` default moved to `responses`

**Before.** `nexus.llm.openai` always spoke `/v1/chat/completions`.

**Now.** [`api:`](./reference.md#which-openai-api-api) selects the surface, and
its default is `responses` — **narrowed** to `chat_completions` whenever
`base_url` or an Azure `auth_mode` is set:

| Deployment | Effective `api` with no `api:` anywhere | Moved? |
|---|---|---|
| plain `api.openai.com` | `responses` | **yes** |
| `base_url` set (vLLM, Ollama, OpenRouter, LM Studio, any proxy) | `chat_completions` | no |
| `auth_mode: azure_key` | `chat_completions` | no |
| `auth_mode: azure_aad` | `chat_completions` | no |

**Why.** From GPT-5.4 onward Chat Completions refuses tool calling with any
`reasoning_effort` other than `none`, and Nexus puts tools on every turn — so on
a current reasoning model the chat surface cannot reason at all. A default that
cannot reason is not the conservative choice.

**To stay where you were**, declare it. On the plugin, for every role:

```yaml
plugins:
  nexus.llm.openai:
    api: chat_completions
```

…or per `core.models` entry, which wins over the plugin key:

```yaml
core:
  models:
    legacy:
      provider: nexus.llm.openai
      model: gpt-4o
      api: chat_completions
```

Being a provider-native axis, a role's `api:` does **not** fall through from the
`default` role to a named one — a named role that sets none takes the plugin
key, not the default role's.

**What does not change when the endpoint does.** No configuration key changes
shape between the two surfaces, and `llm.response` carries the same fields
either way — text, tool calls, usage and a `FinishReason` translated back into
the chat surface's words. The one request field with no counterpart on
`responses` is `prediction`, which is dropped with one warning naming the role;
the turn proceeds. The [batch coordinator](./reference.md#nexusllmbatch) follows
`api:` too, and its own default moved to `responses` alongside this one — see
[Batched OpenAI requests follow `api:`](#7-batched-openai-requests-follow-api).

---

## 2. OpenAI no longer detects reasoning models

**Before.** The provider carried a `reasoningModelPattern` regex (`o1*`, `o3*`,
`o4*`, `gpt-5*`) and a `force_reasoning` boolean to override its guess. A model
the regex matched had its sampling parameters stripped for free.

**Now.** Both are gone. [`reasoning.mode`](./reference.md#declaring-a-reasoning-model-openai)
is the only thing that tells this provider the target is a reasoning model — the
provider inspects no model id anywhere.

**The loud half.** `force_reasoning` is no longer a key in `schema.json`, whose
`additionalProperties: false` makes it a hard config error:

```text
plugins.nexus.llm.openai: unknown key "force_reasoning"
```

**The quiet half.** A config that relied on bare auto-detection — a reasoning
model with no `reasoning:` block at all — now sends `temperature` and friends
and gets an HTTP 400 from OpenAI on the first turn. Nothing local can catch it,
because catching it would require the model table this change deletes.

**The edit.** Add an explicit block to every `nexus.llm.openai` instance
targeting a reasoning model:

```yaml
plugins:
  nexus.llm.openai:
    reasoning:
      mode: effort
      effort: medium   # optional — unset leaves the model's own default
```

`force_reasoning: true` becomes exactly that block. `force_reasoning: false`
should simply be deleted.

**Why no warning heuristic was added.** Any "this looks like a reasoning model"
check at `Init` is the same stale model table under another name, and wrong in
both directions: a proxied reasoning model under a custom id gets no warning,
and a `gpt-5.5-chat` gets a spurious one.

---

## 3. OpenAI `reasoning.enabled` and `reasoning.budget_tokens` are deprecated

Both still load. **Neither fails boot.**

| Key | v0.29.0 behaviour |
|---|---|
| `reasoning.enabled: true` | Maps to `mode: effort`, with a deprecation warning. Ignored (also warned) when `mode` is set explicitly |
| `reasoning.enabled: false` | Maps to `mode: off`, same handling |
| `reasoning.budget_tokens` | **Ignored** with a warning — OpenAI has no reasoning token budget; depth is `reasoning.effort` |

Move to `reasoning.mode` and `reasoning.effort`; the aliases are kept only so an
upgrade does not refuse to start.

One related key never survived the repair: `reasoning.include_summary` was read
by the old code but absent from `schema.json`, so no deployment could set it
without failing validation. It is replaced by
[`reasoning.summary`](./reference.md#nexusllmopenai) (`auto` / `concise` /
`detailed`), which reaches the wire on the `responses` surface and is dropped
with a warning on `chat_completions`.

---

## 4. Gemini: a role's setting now beats the plugin block

**Before.** On `nexus.llm.gemini` — and only there — a plugin-level
`thinking.level` beat a `core.models` role's `effort:`, on the argument that
`level` is Gemini's native vocabulary and `effort` the translated one.

**Now.** The contest is between the *layers*, not the vocabularies, and it is
settled the same way on all three providers:

```text
request-stamped  >  role's native block  >  role's effort  >  plugin block
```

**The edit.** A deployment that set both and relied on the plugin winning now
sends the role's effort. Move the value onto the role's own `thinking:` block to
keep the old result:

```yaml
core:
  models:
    quick:
      provider: nexus.llm.gemini
      model: gemini-2.5-flash
      effort: minimal
      thinking:
        mode: level
        level: high      # now the thing that wins, as the plugin key used to
```

**Two follow-on consequences**, both of which turn silence into noise:

- **A typo now fails `Init`.** A role `effort:` that a plugin-level `level` used
  to shadow now reaches the vocabulary gate, so a word in neither vocabulary —
  previously ignored outright — fails the boot naming the role and the accepted
  set.
- **A previously-shadowed clamp is now warned.** `xhigh` and `max` clamp to
  `high` on Gemini. A role carrying one that a plugin `level` used to displace
  now clamps for real, and the clamp is logged once at `Init` naming the role,
  the configured value and the effective one.

Both are the old behaviour becoming visible rather than new behaviour, but both
show up in logs — or in a refused boot — where nothing showed before.

---

## 5. OpenAI: a role's `effort:` now beats the plugin's

**Before.** `nexus.llm.openai` read `core.models` `effort` **not at all** — the
key parsed, travelled on the request, and was consumed by nobody. Only the
plugin-level `reasoning.effort` could reach the wire, and (see below) it could
not reach it either.

**Now.** A role's `effort:` is consumed as `reasoning_effort` and **wins over**
the plugin-level `reasoning.effort` — specificity wins, uniformly across all
three providers. An `effort` the role's own `reasoning:` block names outranks
that same role's `effort:`, because both land in the same wire field.

**The edit.** A deployment that sets a role `effort:` *and* a plugin-level
`reasoning: {mode: effort, effort: …}` now sends the role's word. Either move
the value onto the role's own `reasoning:` block, or drop the role's `effort:`.

A deployment with no `reasoning:` block anywhere is unaffected: with no declared
mode, nothing is sent — a bare `effort:` does **not** imply `mode: effort`.
That is deliberate. `effort` is a shared cross-provider axis, and a role
carrying `effort: max` for its Anthropic primary must not make an OpenAI
fallback entry start sending `reasoning_effort` and stripping `temperature`.

**The bug underneath this.** Before v0.29.0 no YAML could put a reasoning control
on an OpenAI request at all: the code read `reasoning.effort`, `schema.json`
declared only `reasoning.{enabled,budget_tokens}` with
`additionalProperties: false`, and unknown keys are a hard boot failure — so an
operator following the code had a config that refused to start, and one
following the docs had a config that started and did nothing.

---

## 6. `retry:` and `cache:` blocks are parsed strictly

Those blocks are now validated by the provider itself, not only by
`schema.json` — because a block written on a `core.models` role never passes
through `schema.json` at all. The same validator runs at plugin level, so two
classes of input that used to be tolerated now fail the boot naming the key:

- **An unknown key.** `retry: {max_attempts: 2}` — a key that does not exist —
  fails, naming the role when a `core.models` entry is what set it. The key is
  `max_retries`.
- **A wrongly typed key.** `max_retries` and `multiplier` must be numbers,
  `enabled` a bool, `statuses` a list of integers.

**One correction you may have to make even without touching a role.** All three
providers' `schema.json` listed `backoff: jitter` in the enum where the code has
only ever accepted `exponential_jitter`. The enum is corrected, so:

| `backoff:` | v0.28.x | v0.29.0 |
|---|---|---|
| `jitter` | passed schema, then warned and defaulted by the parser | **fails schema validation at boot** |
| `exponential_jitter` | **failed** plugin-level schema validation (while working on a role block, which bypasses the schema) | passes — it is the real default |

Two parser corrections ride along and are not breaking, only newly-working:
numbers are read as `int` *or* `float64` everywhere (YAML gives either, so
`multiplier: 2` used to be dropped in silence), and `nexus.llm.gemini`'s
`cache.max_entries` — parsed by the code but rejected by its own schema, and so
unusable — is now accepted. Gemini's documented `cache.min_tokens` and
`cache.ttl` defaults were also wrong on this page (1000 / `5m`); the code's
values are `32768` and `1h`.

Everything else in these parsers stays lenient by long-standing contract: an
unrecognised `backoff` word and an unparseable duration are warned about and
defaulted rather than refused.

---

## 7. Batched OpenAI requests follow `api:`

**Before.** `nexus.llm.batch` hardcoded `url: "/v1/chat/completions"` on every
OpenAI JSONL line and built the body with a text-only adapter that had never
heard of `reasoning`. A `core.models` role configured for depth lost that
configuration the moment it was batched, in the one direction that matters:
from GPT-5.4 onward Chat Completions refuses tool calling with any
`reasoning_effort` other than `none`.

**Now.** A line's `url` — and the batch's own `endpoint` — is what the role's
effective `api:` resolves to, and on `api: responses` the resolved reasoning
configuration rides the body as a `reasoning` object. The per-entry axes reach
the coordinator through `engine.ResolveModelConfig`, the same core mechanism the
provider uses, so a role's `api:`, `effort:` and `reasoning:` block mean the same
thing on both paths.

**The coordinator's own default moved too**, from the hardcoded chat endpoint to
`responses`. That matches `nexus.llm.openai` by construction rather than by
coincidence: the provider narrows its default to `chat_completions` for a
deployment declaring a `base_url` or an Azure auth mode, and the coordinator has
neither — it always talks to `api.openai.com`, the deployment the unnarrowed
default is chosen for.

**The edit**, if you want to stay where you were:

```yaml
plugins:
  nexus.llm.batch:
    providers:
      openai:
        api: chat_completions
```

Or set `api:` on the `core.models` entries that should stay — an entry's `api:`
outranks the coordinator's key.

**Two new keys.** `providers.openai.api` and `providers.openai.reasoning`
(`mode`, `effort`, `summary`) exist because a plugin's config map is its own:
the coordinator cannot see `nexus.llm.openai`'s plugin-level `api:` or
`reasoning:`, exactly as it cannot see its credentials. A `core.models` entry's
`reasoning:` block merges **over** the coordinator's, key by key.

**What did not change.** Chat-path batching is byte-for-byte what it was — no
`reasoning_effort` is written there, because on that surface it is refused
alongside the tools every Nexus turn carries. Reasoning Items are neither
replayed into a line nor captured off a result: a batch line is a one-shot
request. And a submit whose roles resolve to *different* surfaces is now
rejected, naming both — an OpenAI batch carries one `endpoint` that every line
must match, so that submit was never expressible.

**Azure batch on `/v1/responses` is not claimed.** It is unconfirmed, and the
coordinator has no Azure mode to reach it with in any case.

---

## What is not verified yet

These are real limits of what shipped, not caveats about the documentation.
Read them before putting a plain `api.openai.com` deployment on a long agent
loop.

- **The Responses path has only ever run against mocks.** Every test of it —
  request serializer, reply parser, SSE reader, multimodal Items, the endpoint
  builder on all six (`auth_mode`, `api`) pairs, encrypted-reasoning replay and
  its rejection — drives an `httptest` server or a substituted transport. The
  only live OpenAI test in the repo is skipped without `OPENAI_API_KEY`, is not
  in CI, and sets no `api:`. **Plain `api.openai.com` deployments are this
  path's first real traffic on upgrade.**
- **The replay-rejection rate after 3–4 tool rounds is unverified.** Nexus sends
  `store: false`, so every `reasoning` Item comes back carrying an opaque
  `encrypted_content` blob that the next request must replay verbatim. There are
  **field reports** — not OpenAI documentation — that those blobs stop verifying
  after three or four tool-calling rounds. If they are accurate, long agent
  loops on OpenAI now **fail loudly** rather than degrade: the turn ends in
  `core.error` naming the cause, `Retryable` is `false`, and there is no silent
  retry without the Items. That is the deliberate choice — losing reasoning
  continuity changes the *answer*, quietly and plausibly, and a silent second
  attempt would convert a visible failure into an invisible quality drop. It is
  also the first thing to measure against a real key.
- **Cross-model-family reasoning replay is undetected by design.** Encrypted
  reasoning is reusable only within a model family, so a
  [`fallback`](./reference.md#nexusproviderfallback) chain that swaps families
  mid-conversation replays Items the next model cannot verify. Nexus does not
  detect this — detecting it would mean shipping the model-family table this
  effort removes. It surfaces as a request failure whose message names it as
  **one of two possible causes**, alongside a blob that simply stopped
  verifying, so an operator hitting either can tell which one they have.
- **Two Responses wire shapes were asserted from API knowledge, not a fetched
  spec.** The nested `input_audio` content part, and a content-parts array on
  `function_call_output.output` for a tool that returned media. Both only
  trigger when a message actually carries those parts; both are worth a live
  smoke check.
- **`retry.enabled` is inert on all three providers, and always has been.** The
  *presence* of a `retry:` block is what turns retrying on; the parser sets its
  internal enabled flag unconditionally and never reads the key. A deployment
  carrying `enabled: false` retries anyway. Honouring the key would silently
  flip behaviour for exactly those deployments, so it stays inert — write
  `max_retries: 0` for "one attempt, no retries".

## What is not wired yet

- **`plugins/llm/batch` still builds its own OpenAI bodies.** It now addresses
  each line to the endpoint the role's `api:` resolves to and carries the role's
  reasoning configuration on the Responses one — see [Batched OpenAI requests
  follow `api:`](#7-batched-openai-requests-follow-api) — but the serializers are
  a second, minimal implementation rather than the provider's, because the
  provider's are unexported methods on another plugin's state and there is no
  cross-plugin call. In practice that means the coordinator's bodies stay
  text-only: no multimodal parts, no prompt-registry decoration, no
  `tool_choice`, and no replay of encrypted `reasoning` Items.
- **Azure batch on `/v1/responses` is not claimed.** Whether the Azure Batch
  surface accepts those lines is unconfirmed, and the coordinator has no Azure
  mode anyway.
- **A role's `cache:` block is ignored on OpenAI.** There is no `cache:` block
  on `nexus.llm.openai` at all, so a role carrying one for an OpenAI entry is
  ignored **in silence**, typos included. That is deliberate — a role shared
  across a `fanout` spanning all three providers should not have to be split
  just to configure caching on the two that support it — but it does mean a
  typo'd OpenAI cache block is never reported. OpenAI's own prompt caching is
  automatic and server-side.
- **A role's `thinking:` block is ignored on OpenAI**, which has `reasoning:`
  instead. Likewise silent.

## Asymmetries that remain

`thinking.step` is the event every IO transport renders as reasoning, and what
gates its emission differs on all three providers. This is a known
inconsistency, stated rather than smoothed over:

| Provider | What gates `thinking.step` emission | Consequence |
|---|---|---|
| `nexus.llm.anthropic` | the **plugin-level** `thinking.include_thoughts` | A role that turns `include_thoughts` on gets the thinking text from the API — the role's value does change the wire shape — but **no events**. The response-parse path never sees the resolved per-role config |
| `nexus.llm.gemini` | **nothing** locally | `include_thoughts` is a wire field (`includeThoughts`), resolved per role like any other, and every thought part that comes back is emitted. A role's value works end to end |
| `nexus.llm.openai` | the **resolved per-role** value — `mode: effort` *and* a `reasoning.summary` | New in v0.29.0. A role naming `summary: detailed` on a deployment whose plugin block is silent gets its events; a role that omits it gets none |

The OpenAI gate is deliberately the one Anthropic does not have: summaries are
opt-in there, so a provider or proxy that volunteers summary frames cannot
fabricate a reasoning trail nobody asked for. Bringing Anthropic's gate onto the
resolved value is a separate change and has not been made.
