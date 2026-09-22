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
| `base_url` | string | `https://api.openai.com/v1/chat/completions` | API endpoint URL (override for Azure, local proxies, etc.) |
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

`reasoning.summary` (`auto` / `concise` / `detailed`) is accepted and validated
but does not reach the wire: `/v1/chat/completions` returns no reasoning
summaries. `reasoning.enabled` and `reasoning.budget_tokens` are deprecated —
`enabled` maps onto `mode`, `budget_tokens` is ignored — and neither fails boot.

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
