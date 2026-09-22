# OpenAI Provider

The OpenAI provider calls the Chat Completions API via direct HTTP requests — no SDK dependency. It supports streaming, tool use, request cancellation, and automatic retries. Compatible with any OpenAI-compatible API endpoint (Azure OpenAI, local proxies, etc.) via `base_url`.

## Details

| | |
|---|---|
| **ID** | `nexus.llm.openai` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_key_env` | string | `OPENAI_API_KEY` | Name of the environment variable containing the API key |
| `base_url` | string | `https://api.openai.com/v1/chat/completions` | API endpoint URL (override for Azure, local proxies, etc.). Setting it narrows the default `api` to `chat_completions` |
| `api` | string | `chat_completions` | Which OpenAI API to speak: `chat_completions` or `responses`. `responses` is not implemented yet and fails `Init`. Also settable per `core.models` entry, which wins |
| `debug` | bool | `false` | Log raw request/response bodies to the session plugin directory |
| `pricing` | map | (embedded defaults) | Per-model pricing overrides. Keys are model IDs, values have `input_per_million` and `output_per_million` (USD) |

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

A role's `retry:` block is read too, merging over the plugin-level one, as are its `effort:` and its `reasoning:` block — the two halves of reasoning depth; see [Per-role reasoning](#per-role-reasoning) below. The remaining per-entry axes have no consumer here: the `thinking`, `cache` and `api` entries are read by nobody on this provider.

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
> [configuration reference](../../configuration/reference.md#declaring-a-reasoning-model-openai).

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
  api: chat_completions   # chat_completions | responses
```

It matters because from GPT-5.4 onward Chat Completions refuses tool calling
with any `reasoning_effort` other than `none`, and Nexus puts tools on every
turn — so on a current reasoning model the chat surface cannot reason at all.
It is still the right surface for older models and for the OpenAI-compatible
endpoints `base_url` exists for, which implement `/chat/completions` and mostly
not `/responses`.

The default is `chat_completions`, narrowed further in the sense that a
`base_url` or an Azure `auth_mode` keeps it there even after the plain
`api.openai.com` default moves. A `core.models` entry may carry its own `api:`,
which wins over the plugin key; being a provider-native axis it does not fall
through from the `default` role to a named one. Exactly one endpoint is chosen
per request, from those two and nothing else.

`api: responses` is **not usable yet**: the selector, the request serializer,
the non-streaming reply parser and the SSE reader ship ahead of the multimodal
Item shapes and the endpoint builder, so declaring it — on the plugin or on a
role — still fails `Init` naming the release it lands in. A surface with no
endpoint builder has no URL to post to.

The two surfaces carry genuinely different requests, which is why this is a
declared axis rather than a URL suffix: `messages` becomes a flat `input` list
of Items, tool calls and their results become sibling `function_call` /
`function_call_output` Items paired by `call_id`, tool definitions and
`text.format` are flattened, `max_tokens` becomes `max_output_tokens`, and the
`reasoning_effort` scalar becomes a `reasoning` object that can also carry
`summary`. Nexus sends `store: false` on every Responses request — it keeps its
own history — and writes `strict: false` explicitly on every tool definition,
because on that surface an *absent* `strict` attempts strict mode.

The reply differs just as much — an `output` array of typed Items rather than a
`choices[0].message` — but what lands on `llm.response` does not: text is the
`output_text` parts concatenated, each `function_call` Item becomes a tool call
identified by its `call_id`, usage maps across the renamed counters with
reasoning tokens included, and the run `status` is translated back into the
chat surface's finish-reason words. `reasoning` Items are captured whole onto
`llm.response.Metadata["openai_reasoning_items"]` — under `store: false` they
carry `encrypted_content`, which the next request must replay verbatim or the
model loses its reasoning across a tool round.

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

Two warnings fire at `Init`, each once, and neither on `responses`: one when a
deployment configures reasoning while its effective `api` is `chat_completions`
(the restriction above), and one when it sets `reasoning.summary` there, which
that surface has no field for.

See [Which OpenAI API: `api`](../../configuration/reference.md#which-openai-api-api).

### Compatible Endpoints

The `base_url` config allows pointing at any OpenAI-compatible API:

- **Azure OpenAI** — Set `base_url` to your Azure endpoint
- **Local proxies** — LM Studio, Ollama (with OpenAI-compatible mode), vLLM, etc.
- **Other providers** — Any service implementing the Chat Completions API

## HTTP Configuration

- **Timeout**: 5 minutes per request
- **API endpoint**: Configurable via `base_url`, defaults to `https://api.openai.com/v1/chat/completions`

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
