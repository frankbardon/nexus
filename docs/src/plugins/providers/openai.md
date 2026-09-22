# OpenAI Provider

The OpenAI provider calls the Chat Completions API or the Responses API via direct HTTP requests — no SDK dependency. Which one it speaks is declared with [`api`](#which-api-api). It supports streaming, tool use, request cancellation, and automatic retries, on Azure OpenAI (`auth_mode`) and on any OpenAI-compatible endpoint (`base_url`).

## Details

| | |
|---|---|
| **ID** | `nexus.llm.openai` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_key_env` | string | `OPENAI_API_KEY` | Name of the environment variable containing the API key |
| `base_url` | string | `https://api.openai.com/v1/chat/completions` | API endpoint URL (override for local proxies and OpenAI-compatible endpoints) — the whole chat endpoint, not a prefix. Setting it narrows the default `api` to `chat_completions`; under an explicit `api: responses` the sibling `/responses` route beneath it is used |
| `api` | string | `responses` *(narrowed)* | Which OpenAI API to speak: `responses` or `chat_completions`. Both work end to end. The default narrows to `chat_completions` when `base_url` or an Azure `auth_mode` is set (see below). Also settable per `core.models` entry, which wins |
| `reasoning.mode` | string | `off` *(absent block)* / `effort` *(present block)* | `effort` or `off`. The **only** declaration that the target is a reasoning model, and the only gate on the sampling-parameter strip. No model id is ever inspected |
| `reasoning.effort` | string | *(unset)* | `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max` — OpenAI's full vocabulary, **unclamped**. An unrecognised value fails `Init`. The least specific layer: a `core.models` role's `effort:` beats it |
| `reasoning.summary` | string | *(unset)* | `auto`, `concise`, `detailed`. Reaches the wire only on `api: responses`; dropped with one `Init` warning on `chat_completions`, which has no summary field. Also the gate on `thinking.step` emission |
| `reasoning.enabled` | bool | *(unset)* | **Deprecated** alias for `mode`: `true` → `effort`, `false` → `off`. Warns; does not fail boot |
| `reasoning.budget_tokens` | int | *(unset)* | **Deprecated and ignored** — OpenAI has no reasoning token budget. Warns; does not fail boot |
| `debug` | bool | `false` | Log raw request/response bodies to the session plugin directory |
| `pricing` | map | (embedded defaults) | Per-model pricing overrides. Keys are model IDs, values have `input_per_million` and `output_per_million` (USD) |

`auth_mode`, `azure.*`, `files.*`, `multimodal.*` and `retry.*` are configured
here too; the [configuration
reference](../../configuration/reference.md#nexusllmopenai) carries the full
table and is canonical where the two disagree.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `llm.request` | 10 | Receives LLM requests from agents |
| `cancel.active` | 5 | Cancels in-flight API requests |

### Emits

| Event | When |
|-------|------|
| `llm.response` | Non-streaming response received |
| `llm.stream.chunk` | Each chunk of a streaming response |
| `llm.stream.end` | Streaming response complete |
| `core.error` | API errors |

## Features

### Model Resolution

The provider uses the Model Registry to resolve role names. When an `llm.request` specifies a `Role` (e.g., `"reasoning"`), the provider looks up the concrete model config. If no role is specified, the default model is used.

Several per-entry axes from that config are read here: `model`/`max_tokens` through the provider's own resolution pass, and `temperature` — on every path, including a `fallback` retry and a `fanout` leg. **A temperature already on the request wins over the role's**, so an agent posture and the `approval_policy` gate still outrank `core.models`; the role fills the axis only when nothing upstream set one, and `0` is a real value rather than "unset". `applyReasoning` strips `temperature` back out whatever its origin whenever the **resolved** `reasoning.mode` — the plugin block merged with the role's — is anything but `off`; a model id alone never strips it. See [Structured Output](#structured-output-native) and the [configuration reference](../../configuration/reference.md#coremodels).

A role's `retry:` block is read too, merging over the plugin-level one, as are its `effort:` and its `reasoning:` block — the two halves of reasoning depth; see [Per-role reasoning](#per-role-reasoning) below — and its `api:`, which picks the endpoint for that entry; see [Which API: `api`](#which-api-api). So most per-entry axes land here: `model`, `max_tokens`, `temperature`, `effort`, `reasoning`, `retry` and `api`. The two that do not are `thinking` (this provider has `reasoning:` instead) and `cache` (it has no cache block at all) — both are **ignored in silence**, typos included.

There is no `cache:` block on this provider at all, so a role carrying one for an OpenAI entry is **ignored in silence** — including its typos, which nothing here validates. That is deliberate: a role shared across a `fanout` spanning all three providers should not have to be split just to configure caching on the two that support it. OpenAI's own prompt caching is automatic and server-side; there is nothing to configure. See [Prompt caching: `cache`](../../configuration/reference.md#prompt-caching-cache).

### Reasoning

Reasoning depth is declared on the plugin:

```yaml
nexus.llm.openai:
  reasoning:
    mode: effort      # effort | off
    effort: medium    # none | minimal | low | medium | high | xhigh | max
```

`mode` is the operator's declaration that the target is a reasoning model, and
it is the **only** such declaration: the provider inspects no model id anywhere
and holds no model-capability table. An **absent** `reasoning:` block means
`off` and sends no reasoning configuration at all; a **present** block with no
`mode` means `effort`. `effort` is OpenAI's full vocabulary and is **not
clamped** — it is a superset of what `nexus.llm.anthropic` and
`nexus.llm.gemini` accept, so a role word written for either of those passes
through verbatim. An unrecognised `effort` fails `Init` naming the accepted set.

`mode` also gates the **sampling-parameter strip**. Reasoning models reject
`temperature`, `top_p`, `presence_penalty`, `frequency_penalty`, `logprobs`,
`top_logprobs` and `prediction` outright, so any `mode` other than `off`
removes them from the request body before it is sent. Under `mode: off` — an
absent block included — nothing is stripped, whatever the model is called.

> **Breaking change.** Earlier releases auto-detected reasoning models from the
> model id (a built-in `o1*` / `o3*` / `o4*` / `gpt-5*` regex) and shipped a
> `force_reasoning` boolean to override that guess. Both are gone. Point an
> instance at a reasoning model with no `reasoning:` block and it will now send
> `temperature` and get an HTTP 400 from OpenAI. Add `reasoning: {mode: effort}`
> to every `nexus.llm.openai` instance that targets one; `force_reasoning: true`
> becomes exactly that block, and the key itself must be removed or config
> validation fails the boot with `unknown key "force_reasoning"`. See the
> [configuration reference](../../configuration/reference.md#declaring-a-reasoning-model-openai)
> and [Upgrading to
> v0.29.0](../../configuration/upgrading-v0.29.md#2-openai-no-longer-detects-reasoning-models).

`reasoning.summary` (`auto` / `concise` / `detailed`) reaches the wire only on
the Responses API, which takes a `reasoning` object carrying both the depth and
the summary verbosity. `/v1/chat/completions` has a single `reasoning_effort`
scalar and no summary field at all, so on that surface the key is dropped and
`Init` says so once — a deployment on `api: responses` is not warned.
`reasoning.enabled` and `reasoning.budget_tokens` are deprecated — `enabled`
maps onto `mode`, `budget_tokens` is ignored — and neither fails boot.

### Per-role reasoning

A `core.models` entry may carry a `reasoning:` block of its own and an `effort:`,
so one plugin instance can put a different depth on the wire per role:

```yaml
plugins:
  nexus.llm.openai:
    reasoning:
      mode: effort
      effort: low            # every role's default

core:
  models:
    default: balanced
    balanced:
      provider: nexus.llm.openai
      model: gpt-5.1         # inherits the plugin block whole -> low
    deep:
      provider: nexus.llm.openai
      model: gpt-5.1
      effort: max            # the cross-provider word, unclamped here
    plain:
      provider: nexus.llm.openai
      model: gpt-4o
      reasoning:
        mode: off            # not a reasoning model: send nothing, strip nothing
```

The role's block **merges over** the plugin's key by key — the role wins on every
key it names, a plugin key it is silent about survives — and the merged block goes
back through the same parser, so it gets the same validation, `mode` inference and
deprecation handling. `reasoning: {}` is a statement rather than a silence: the
merged block is present, which declares `mode: effort` on a deployment whose
plugin block is absent entirely.

**Precedence**, highest first: a block already on the request (what the `fallback`
and `fanout` coordinators stamp for the chain entry actually being served) → an
`effort` the role's own `reasoning:` block names → the role's `effort:` → the
plugin-level `reasoning:` block. One rule, every provider: specificity wins.

**`mode` is the declaration, and a bare `effort:` is not.** A role's `effort:` on a
deployment where no layer declares a reasoning mode sends nothing at all. That is
deliberate: `reasoning_effort` on a model that is not a reasoning model is an HTTP
400, this provider holds no model-capability table to tell one from the other, and
`effort` is a shared axis a role may be carrying for an Anthropic or Gemini entry
in the same chain.

Nothing clamps and nothing warns — OpenAI's vocabulary is a superset of the other
two providers' — so a word in no provider's vocabulary is simply a typo, and fails
`Init` naming the role, for any role whose depth could actually reach the wire.
Every role this provider could serve is swept at boot: these blocks bypass
`schema.json` entirely, so nothing else ever checks them. See the
[configuration reference](../../configuration/reference.md#per-role-reasoning-openai).

See the [configuration reference](../../configuration/reference.md#nexusllmopenai).

### Streaming

When `llm.request.Stream` is `true`, the provider uses Server-Sent Events (SSE) to stream the response. Each content chunk and tool use block generates a `llm.stream.chunk` event. When streaming completes, `llm.stream.end` carries the full usage statistics. Usage is requested via `stream_options.include_usage`.

### Tool Calling

The provider translates Nexus tool definitions into the OpenAI function calling format (`type: "function"`). Tool call responses from the API are parsed and included in the `llm.response` or streamed via `llm.stream.chunk` events.

### Prompt Assembly

Before sending a request, the provider calls `PromptRegistry.Apply()` to append dynamic sections (skills catalog, system variables, etc.) to the system prompt.

### Request Cancellation

Subscribes to `cancel.active` at priority 5. When a cancellation arrives, the in-flight HTTP request context is cancelled, aborting the API call.

### Retry Logic

Transient errors (rate limits, server errors) are retried with exponential backoff.
#### Per-role retry

A [`core.models`](../../configuration/reference.md#coremodels) role entry may
carry its own `retry:` block, which **merges over** the plugin-level one key by
key: the role wins on every key it names and a plugin key it is silent about
survives, so a role that only wants a shorter `max_retries` keeps the plugin's
backoff shape and status list. The merged block goes through the same parser, so
it gets the same defaulting and the same soft fallbacks; a block already stamped
on the request by `nexus.provider.fallback` or `nexus.provider.fanout` — for the
chain entry actually being served — wins over the role's. Every role this
provider could serve is swept at `Init`: an unknown key or a wrongly typed one
fails the boot naming the role, because these blocks never pass through
`schema.json`.

Unlike every other per-entry axis this one reaches no part of the request body.
It is call-time behaviour, resolved per request and used to drive that request's
retry loop — which is the point: a role with a fallback chain usually wants
*fewer* attempts at the primary than a terminal role, because every retry there
is time not spent on the entry that might actually answer.

See [Retry behaviour: `retry`](../../configuration/reference.md#retry-behaviour-retry)
for the canonical account.

### Structured Output (Native)

OpenAI natively supports structured output via the `response_format` API field. When `ResponseFormat` is set on an `LLMRequest`, the provider maps it directly:

- **`json_object`** → `{"type": "json_object"}` — Forces valid JSON output.
- **`json_schema`** → `{"type": "json_schema", "json_schema": {"name": "...", "schema": {...}, "strict": true}}` — Forces output matching a specific schema. The `Strict` field controls whether OpenAI enforces exact schema adherence.
- **`text`** → No `response_format` field (OpenAI default).

`LLMResponse.Metadata["_structured_output"]` is set to `true` for `json_object` and `json_schema` types.

### Cost Tracking

The provider computes `CostUSD` on every `llm.response` using per-model pricing rates. Embedded defaults cover common OpenAI models. Override via config for enterprise pricing tiers or new models:

```yaml
nexus.llm.openai:
  pricing:
    gpt-4o:
      input_per_million: 2.50
      output_per_million: 10.0
```

Config overrides are merged with embedded defaults — only override the models you need to change. Cost is accumulated into `SessionMeta.CostUSD` by the engine.

### Debug Mode

When `debug: true`, raw request and response JSON bodies are written to the session's plugin directory for inspection.

### Which API: `api`

This is the one provider with more than one API surface, and the surface is
declared rather than detected:

```yaml
nexus.llm.openai:
  api: responses          # responses | chat_completions
```

It matters because from GPT-5.4 onward Chat Completions refuses tool calling
with any `reasoning_effort` other than `none`, and Nexus puts tools on every
turn — so on a current reasoning model the chat surface cannot reason at all.
It is still the right surface for older models and for the OpenAI-compatible
endpoints `base_url` exists for, which implement `/chat/completions` and mostly
not `/responses`.

The default is `responses` on a plain `api.openai.com` deployment, and
**narrows to `chat_completions`** whenever a `base_url` or an Azure `auth_mode`
is set — those are the endpoints that may not implement `/responses` at all. A
`core.models` entry may carry its own `api:`, which wins over the plugin key;
being a provider-native axis it does not fall through from the `default` role to
a named one. Exactly one endpoint is chosen per request, from those two and
nothing else.

`api: responses` **works end to end** — serializer, reply parser, SSE reader,
multimodal Item shapes, the endpoint builder on every `auth_mode`, encrypted
reasoning replay across a tool loop, and the failure when a replayed blob is
*rejected*. That last one is why it is now the default: the rejection path
exists, so the trade a default makes on an operator's behalf is a trade with a
written failure mode rather than a silent one.

**What each endpoint supports.** Everything an agent loop needs works on both;
the differences are at the edges, and all of them are consequences of what the
API itself offers rather than of what is implemented here:

| | `chat_completions` | `responses` |
|---|---|---|
| Streaming, tool calling, cancellation, retries | yes | yes |
| Structured output (`json_object` / `json_schema`) | `response_format` | `text.format`, flattened; the caller's `strict` forwarded verbatim |
| Images and files | yes (`image_url` / `file`) | yes (`input_image` / `input_file`) |
| An oversize inline image uploaded via the Files API and referenced by id | **no** — `image_url` carries no `file_id`, so it is an error | yes, at or above `files.upload_threshold` |
| `reasoning_effort` on the wire | yes — but **not together with tools** from GPT-5.4 onward | yes, as a `reasoning` object |
| `reasoning.summary` | **no field exists**; dropped with one `Init` warning | yes |
| Reasoning text as `thinking.step` events | **none** — the API returns no reasoning text | yes |
| Reasoning continuity across a tool round | nothing to replay | encrypted `reasoning` Items, replayed verbatim; a rejected replay fails the turn |
| `prediction` (Predicted Outputs) | yes | **no counterpart** — dropped with one warning per role, turn proceeds |
| Azure | deployment-scoped path + `api-version` | versionless `/openai/v1/responses`, deployment in the body |
| Batched via [`nexus.llm.batch`](../../configuration/reference.md#nexusllmbatch) | yes | **no** — the batch coordinator hardcodes the chat endpoint |

**Upgrading from v0.28.x moves a plain deployment's endpoint.** That default was
`chat_completions`. It moved because on OpenAI's current models the chat surface
cannot reason while tools are on the turn, and Nexus puts tools on every turn —
a default that cannot reason is not the conservative choice. To stay where you
were, set `api: chat_completions` on the plugin, or on the `core.models` entries
that should stay. No configuration key changes shape between the surfaces and
`llm.response` carries the same fields either way; the only request field with
no counterpart on `responses` is `prediction`, which is dropped with a warning
naming the role. `base_url` and Azure deployments do not move, and the batch
coordinator keeps its own chat endpoint. That is one of six breaking changes in
this release — see [Upgrading to
v0.29.0](../../configuration/upgrading-v0.29.md) for the complete list, and
[Before you put this on real traffic](#before-you-put-this-on-real-traffic)
below for what has and has not been verified.

**The endpoint.** On `auth_mode: openai` the surfaces are
`https://api.openai.com/v1/chat/completions` and
`https://api.openai.com/v1/responses`; with a `base_url` the chat one is used
verbatim and the Responses one is its sibling (`https://proxy/v1/chat/completions`
→ `https://proxy/v1/responses`, a bare root appended, an already-`/responses`
URL left alone). On Azure the two routes are genuinely different shapes rather
than a suffix swap: chat stays
`https://<resource>.openai.azure.com/openai/deployments/<deployment>/chat/completions?api-version=<v>`,
while Responses is the versionless
`https://<resource>.openai.azure.com/openai/v1/responses` — **no deployment in
the path** (it travels in the body's `model` field) and **no `api-version`
query**, because the GA `/openai/v1/` route is implicitly versioned.
`azure.api_version` is still required, for the chat surface on the same
instance. Auth is orthogonal: the `api-key` header and the `azure_aad` bearer
token apply on both routes identically.

The two surfaces carry genuinely different requests, which is why this is a
declared axis rather than a URL suffix: `messages` becomes a flat `input` list
of Items, tool calls and their results become sibling `function_call` /
`function_call_output` Items paired by `call_id`, tool definitions and
`text.format` are flattened, `max_tokens` becomes `max_output_tokens`, and the
`reasoning_effort` scalar becomes a `reasoning` object that can also carry
`summary`. Nexus sends `store: false` on every Responses request — it keeps its
own history — and writes `strict: false` explicitly on every tool definition,
because on that surface an *absent* `strict` attempts strict mode.

Multimodal content is carried on both surfaces, in each one's own spelling —
flipping `api:` costs no capability. A message's `parts` become an
`input_text` / `input_image` / `input_file` content array on an input Item and
`output_text` on an assistant one, against the chat surface's
`text` / `image_url` / `file`; a tool result that returned images puts the same
array on the Item's `output`. Items with **no** parts keep the plain string
content the API also accepts, so an ordinary text turn is not restructured into
one-element arrays.

Two things genuinely differ rather than being renamed. `input_image` carries a
`file_id`, which the chat surface's `image_url` cannot — so on `responses` an
inline image at or over [`files.upload_threshold`](../../configuration/reference.md#nexusllmopenai)
is uploaded through the Files API and referenced by id, while on
`chat_completions` an oversize image is still an error. And an assistant Item
accepts only text, so a non-text part on an assistant message degrades to that
message's plain string rather than failing the turn — the same degradation the
chat path makes for a malformed part. `multimodal.vision: false` drops image
parts on **both** surfaces, and skips uploading them.

The reply differs just as much — an `output` array of typed Items rather than a
`choices[0].message` — but what lands on `llm.response` does not: text is the
`output_text` parts concatenated, each `function_call` Item becomes a tool call
identified by its `call_id`, usage maps across the renamed counters with
reasoning tokens included, and the run `status` is translated back into the
chat surface's finish-reason words. `reasoning` Items are captured whole onto
`llm.response.Metadata["openai_reasoning_items"]` — under `store: false` they
carry `encrypted_content`, which the next request must replay verbatim or the
model loses its reasoning across a tool round. See [Reasoning across a tool
loop](#reasoning-across-a-tool-loop) for the replay half.

Streaming is a third implementation rather than a branch: Responses sends about
forty typed SSE events where Chat Completions sends one chunk shape. Text
arrives as `response.output_text.delta`, tool-call arguments accumulate across
`response.function_call_arguments.delta` / `.done` with the tool's name and
`call_id` coming from `response.output_item.added`, and the run's status and
usage arrive on `response.completed` — there is no `[DONE]` sentinel and no
`stream_options`. The publication contract is unchanged: every text delta goes
through the engine's stream publisher, so a gate that holds, redacts or blocks a
segment behaves identically whichever surface produced it. The replayable
`reasoning` Items are taken from the `response.completed` snapshot rather than
reconstructed from deltas, because `encrypted_content` exists nowhere in the
delta stream; reasoning *summary* text is accumulated separately onto
`llm.response.Metadata["openai_reasoning_summary"]` and never released as output
text. All three tables are in the [configuration
reference](../../configuration/reference.md#which-openai-api-api).

#### Reasoning across a tool loop

Nexus sends `store: false`, so OpenAI keeps no server-side state for the
conversation and every `reasoning` Item comes back carrying an opaque
`encrypted_content` blob. **Every Item of a turn must be replayed verbatim on
the next request**, or the model starts the following round with no reasoning
context — which on a tool loop is exactly the round that needed it. Nothing in
the response text can substitute: the blob is the state.

This is structurally the same problem as Anthropic's thinking blocks and
Gemini's thought signatures, and it is solved the same way. The Items ride the
assistant turn under the `openai_reasoning_items` key on the
`pkg/roundtrip` allowlist, so every history
builder carries them onto the stored assistant message without knowing what they
are — the memory plugins for persisted history, and the in-process loops in
`delegate`, `subagent`, `planexec` and `orchestrator` that keep their own. The
serializer then splices them back into `input` at the head of the assistant turn
they belong to, ahead of that turn's text Item and its `function_call` Items,
in the order they arrived.

Three things about that replay are deliberate:

- **Whole Items, never rebuilt.** The Item goes back exactly as decoded —
  `id`, `encrypted_content`, `summary`, `status` and any field added later.
  Narrowing it to the fields Nexus names is how several other SDKs came to drop
  `encrypted_content` specifically when a `summary` was present alongside it.
- **The streamed turn is the same turn.** `encrypted_content` appears only on
  the `response.completed` snapshot and nowhere in the deltas, so a streamed
  round replays identically to a non-streamed one.
- **Reasoning summary text is not replayed.** `openai_reasoning_summary` is
  display material; it is deliberately absent from the allowlist, so history
  does not carry a second copy of every summary.

Two limits worth knowing before running a long loop. Encrypted reasoning is
reusable **only within a model family**, so a
[`fallback`](../../configuration/reference.md#nexusproviderfallback) chain that
swaps families mid-conversation replays Items the next model cannot verify;
Nexus does not detect that, because detecting it would mean shipping a
model-family table. And there are field reports of blobs failing verification
after three or four tool rounds even within one family. Both surface as a
request failure rather than a silent quality drop.

#### Reasoning in the UI

Reasoning summary text is also published as `thinking.step` — the event every IO
transport already renders as reasoning, and the parity this provider has never
had, because Chat Completions returns no reasoning text at all. A streamed turn
emits one step per delta (`response.reasoning_summary_text.delta`, the
`response.reasoning_text.delta` spelling on models that stream only that, and a
`response.reasoning_summary_part.added` that carries a whole part at once), so a
TUI shows reasoning as it arrives rather than in one block at the end. A
non-streamed turn emits one step per entry of each `reasoning` Item's `summary`
array, so both surfaces put the same reasoning on the bus. `Index` carries the
part's own `summary_index`, which is what orders the parts of one reasoning Item.

**Summaries are opt-in and are never fabricated.** A step is published only when
the turn's **resolved** configuration asked OpenAI for summaries — `mode: effort`
*and* a `reasoning.summary` — which is exactly the condition the `summary` key
reaches the wire under. Resolved means the serving role's `reasoning:` block
merged over the plugin-level one, so a role that names `summary: detailed` on a
deployment whose plugin block is silent gets its events, and a role that omits
it gets none. The accumulated text still reaches
`llm.response.Metadata["openai_reasoning_summary"]` either way: that key is a
record of what the API sent, while the events are a statement about what the
operator turned on.

Two warnings fire at `Init`, each once, and neither on `responses`: one when a
deployment configures reasoning while its effective `api` is `chat_completions`
(the restriction above), and one when it sets `reasoning.summary` there, which
that surface has no field for.

See [Which OpenAI API: `api`](../../configuration/reference.md#which-openai-api-api).

#### Before you put this on real traffic

The Responses path is complete and tested, but **everything that tested it was a
mock**. Every test drives an `httptest` server or a substituted transport: the
request serializer, the reply parser, the SSE reader, the multimodal Item
shapes, the endpoint builder on all six (`auth_mode`, `api`) pairs, the
encrypted-reasoning replay and its rejection path. The one live OpenAI test in
the repository is skipped without `OPENAI_API_KEY`, is not in CI, and sets no
`api:`. **Plain `api.openai.com` deployments become this path's first real
traffic on upgrade.** Four things are worth knowing before that happens.

- **How often a replayed blob is rejected after three or four tool rounds is
  unverified.** The reports of it are from the field, not from OpenAI's
  documentation. If they are accurate, long agent loops on OpenAI now **fail
  loudly** rather than degrade quietly — which is the deliberate choice, for the
  reason under [Reasoning across a tool loop](#reasoning-across-a-tool-loop) —
  and it is the first thing to measure against a real key.
- **A chain that crosses model families is not detected.** Encrypted reasoning
  is reusable only within one family, and Nexus ships no family table to notice
  when a `fallback` chain leaves one. It surfaces as a request failure whose
  message names it as one of two possible causes, the other being a blob that
  simply stopped verifying.
- **Two wire shapes were asserted from API knowledge rather than a fetched
  spec**: the nested `input_audio` content part, and a content-parts array on
  `function_call_output.output` for a tool that returned media. Both trigger only
  when a message actually carries those parts.
- **`retry.enabled` is inert**, here and on the other two providers, and always
  has been. The *presence* of a `retry:` block is what turns retrying on; the key
  itself is never read, so `enabled: false` with a block present still retries.
  Write `max_retries: 0` for "one attempt, no retries".

### Compatible Endpoints

The `base_url` config allows pointing at any OpenAI-compatible API:

- **Azure OpenAI** — Use `auth_mode: azure_key` / `azure_aad` with the `azure:`
  block; the provider builds the route itself on both API surfaces
- **Local proxies** — LM Studio, Ollama (with OpenAI-compatible mode), vLLM, etc.
- **Other providers** — Any service implementing the Chat Completions API

## HTTP Configuration

- **Timeout**: 5 minutes per request
- **API endpoint**: chosen per request from `auth_mode` and [`api`](#which-api-api); on `auth_mode: openai` that is `https://api.openai.com/v1/responses` by default and `.../v1/chat/completions` under `api: chat_completions`, and `base_url` overrides both (setting it also narrows the default `api` to `chat_completions`)

## Example Configuration

```yaml
nexus.llm.openai:
  api_key_env: OPENAI_API_KEY
  debug: false
```

To use a different environment variable or custom endpoint:

```yaml
nexus.llm.openai:
  api_key_env: MY_OPENAI_KEY
  base_url: https://my-proxy.example.com/v1/chat/completions
```

Using OpenAI models in the model registry:

```yaml
core:
  models:
    default: balanced
    reasoning:
      provider: nexus.llm.openai
      model: o3
      max_tokens: 16384
    balanced:
      provider: nexus.llm.openai
      model: gpt-4.1
      max_tokens: 8192
    quick:
      provider: nexus.llm.openai
      model: gpt-4.1-mini
      max_tokens: 4096
```

Note that the `reasoning` **role** above names a model, not a capability: to
send `o3` reasoning controls — and to strip the sampling parameters it rejects
— the plugin still needs an explicit `reasoning:` block, as described under
[Reasoning](#reasoning). A role called `reasoning` declares nothing by itself.

## Migrating from v0.28.x

Six changes in v0.29.0 are not additive. Three are on this provider, one is
cross-provider, and two are on `nexus.llm.gemini` and the shared `retry:` /
`cache:` parsers. [Upgrading to
v0.29.0](../../configuration/upgrading-v0.29.md) is the complete account — each
one with the exact edit that restores the old behaviour. In summary:

| # | Change | Fails loudly? |
|---|---|---|
| 1 | [`api:` now defaults to `responses`](../../configuration/upgrading-v0.29.md#1-the-openai-api-default-moved-to-responses) on plain `api.openai.com`. Opt out with `api: chat_completions` on the plugin or on a `core.models` entry. `base_url` and both Azure modes do **not** move | no — a different endpoint, the same events |
| 2 | [`reasoningModelPattern` and `force_reasoning` removed](../../configuration/upgrading-v0.29.md#2-openai-no-longer-detects-reasoning-models). Auto-detection is gone; an explicit `reasoning.mode` is required | half — a leftover `force_reasoning` key fails boot; bare auto-detection fails at request time with an HTTP 400 |
| 3 | [`reasoning.enabled` maps and warns, `reasoning.budget_tokens` is ignored and warns](../../configuration/upgrading-v0.29.md#3-openai-reasoningenabled-and-reasoningbudget_tokens-are-deprecated) | no — neither fails boot |
| 4 | [Gemini's precedence inversion is fixed](../../configuration/upgrading-v0.29.md#4-gemini-a-roles-setting-now-beats-the-plugin-block): a role's setting now beats the plugin block. Two consequences — a role `effort` a plugin `thinking.level` used to shadow now reaches the vocabulary gate, and a previously-shadowed `xhigh`/`max` clamp is now warned | for a typo, yes; for the value, no |
| 5 | [A role `effort:` now beats a plugin-level `reasoning.effort` on OpenAI](../../configuration/upgrading-v0.29.md#5-openai-a-roles-effort-now-beats-the-plugins) — specificity wins, uniformly across providers | no |
| 6 | [`retry:` and `cache:` blocks are parsed strictly](../../configuration/upgrading-v0.29.md#6-retry-and-cache-blocks-are-parsed-strictly): unknown or mistyped keys now error, and `backoff: jitter` is no longer in the schema enum (`exponential_jitter` is, and always was what the code accepted) | yes |

And read [Before you put this on real
traffic](#before-you-put-this-on-real-traffic) before upgrading a plain
`api.openai.com` deployment that runs long tool loops.
