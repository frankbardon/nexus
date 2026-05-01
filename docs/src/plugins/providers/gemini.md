# Gemini Provider

The Gemini provider calls Google's Gemini API via direct HTTP — no SDK dependency. It supports both the public Generative Language API (api-key auth) and Vertex AI (service-account JWT auth) and ships feature parity with the OpenAI and Anthropic providers (sync + streaming, tool use, structured output, retry, cancellation, debug logs, fallback hooks). On top of that it adds Gemini-only features: thinking ("reasoning") parts, multimodal inputs, the built-in code execution tool, and prompt caching via the `cachedContents` API.

## Details

|  |  |
|---|---|
| **ID** | `nexus.llm.gemini` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `auth` | string | `api_key` | `api_key` for the public endpoint, `vertex` for Vertex AI |
| `api_key` | string | — | Direct API key (overrides `api_key_env`) |
| `api_key_env` | string | `GEMINI_API_KEY`, then `GOOGLE_API_KEY` | Env var holding the API key |
| `project_id` | string | `$GOOGLE_CLOUD_PROJECT` | (Vertex) GCP project id |
| `location` | string | `us-central1` | (Vertex) GCP region for the AI Platform endpoint |
| `service_account_json` | string | — | (Vertex) Path to a service-account JSON key |
| `service_account_json_env` | string | `GOOGLE_APPLICATION_CREDENTIALS` | (Vertex) Env var holding the service-account path |
| `debug` | bool | `false` | Log raw request/response bodies into the session plugin directory |
| `pricing` | map | embedded defaults | Per-model pricing overrides; see **Cost Tracking** below |
| `retry` | map | disabled | Retry/backoff config; see **Retry Logic** |
| `thinking` | map | disabled | Reasoning config for Gemini 2.5; see **Thinking** |
| `code_execution` | bool | `false` | Enable Gemini's built-in code execution tool |
| `cache` | map | disabled | Prompt cache config; see **Prompt Caching** |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `llm.request` | 10 | LLM requests from agents |
| `cancel.active` | 5 | Cancels in-flight API requests |

### Emits

| Event | When |
|-------|------|
| `llm.response` | Non-streaming response received (also after a stream completes) |
| `llm.stream.chunk` | Each chunk of a streaming response |
| `llm.stream.end` | Streaming response complete |
| `thinking.step` | A `thought: true` part is observed (sync or stream) |
| `tool.invoke` / `tool.result` | When the built-in code execution tool is used |
| `before:core.error` / `core.error` | API errors (vetoable so fallback can intercept) |

## Features

### Auth Modes

- **`api_key`** (default) — Sends `x-goog-api-key` header. URL: `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent`.
- **`vertex`** — Mints an OAuth2 access token via signed JWT exchange against `https://oauth2.googleapis.com/token`, caches the token until expiry minus 60s, and routes requests to `https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent`. JWT signing uses RS256 from the stdlib `crypto/rsa`; no third-party dependencies are added.

### Streaming

When `llm.request.Stream` is `true`, the provider uses `:streamGenerateContent?alt=sse` and parses SSE `data:` lines. Text deltas are emitted as `llm.stream.chunk` (Content), function calls as `llm.stream.chunk` (ToolCall), and a final `llm.stream.end` carries `usageMetadata`.

### Tool Calling

Nexus tool definitions are translated into Gemini `functionDeclarations`. Because Gemini matches tool calls and tool responses by **function name** (not an opaque ID), the provider synthesises stable IDs (`call_{seq}_{name}`) on outbound responses and resolves trailing `tool` messages back to the function name when serializing `functionResponse` parts.

### Tool Choice

`ToolChoice.Mode` maps to `toolConfig.function_calling_config`:

- `auto` → `mode: AUTO`
- `required` → `mode: ANY`
- `none` → `mode: NONE`
- `tool` (with `Name`) → `mode: ANY` plus `allowed_function_names: [name]`

### Structured Output (Native)

When `ResponseFormat.Type` is `json_object` or `json_schema`, the provider sets `generationConfig.responseMimeType: application/json` and (for `json_schema`) `generationConfig.responseSchema`. JSON Schema fields Gemini doesn't accept (`$schema`, `$id`, `additionalProperties`, `$ref`, `definitions`, `$defs`) are stripped recursively. `LLMResponse.Metadata["_structured_output"]` is set to `true`.

### Thinking (Gemini 2.5)

When `thinking.enabled: true`, the provider sends `generationConfig.thinkingConfig`:

```yaml
nexus.llm.gemini:
  thinking:
    enabled: true
    budget_tokens: 8192      # 0 = disabled, -1 = dynamic budget
    include_thoughts: true   # surface thought parts to the bus
```

Response parts with `thought: true` are emitted as `thinking.step` events (`Source: nexus.llm.gemini`, `Phase: reasoning`) and are excluded from `LLMResponse.Content`. Every `thinking.step` event lands in the per-session journal automatically — read it via `journal.Writer.SubscribeProjection` (live) or `journal.ProjectFile` (post-mortem). `usageMetadata.thoughtsTokenCount` is mirrored into `events.Usage.ReasoningTokens`.

### Multimodal

`events.Message.Parts` (text, image, audio, video, file) is serialized into Gemini parts. Inline payloads up to 18 MB use `inlineData` with base64 bytes; larger payloads must be uploaded via the Files API and referenced by URI (`fileData.fileUri`). Provider falls back to `Content` only when `Parts` is empty, so existing text-only callers are unaffected.

### Code Execution

Set `code_execution: true` to advertise Gemini's built-in code execution tool. Response parts of type `executableCode` and `codeExecutionResult` are dual-emitted: appended to `Content` as fenced markdown blocks for any UI, and emitted as `tool.invoke` / `tool.result` events under the synthetic name `_gemini_code_execution` so observers see them as ordinary tool activity.

### Prompt Caching

```yaml
nexus.llm.gemini:
  cache:
    enabled: true
    min_tokens: 32768
    ttl: "1h"
    max_entries: 64
```

The provider computes a deterministic hash of the cache-eligible prefix (model + system instruction + tool declarations + the leading run of contents up to the first tool exchange). When a hit is present in the in-memory LRU, `cachedContent` is set on the request and only the trailing delta is sent. Cache entries are populated explicitly via `Plugin.createCachedContent`; the auto-populate path is intentionally read-only in this initial release. `usageMetadata.cachedContentTokenCount` flows into `events.Usage.CachedTokens` and the cost calculation applies the cached-input discount (default 25%, override via `pricing.<model>.cached_ratio`).

### Cost Tracking

Embedded defaults cover the 1.5, 2.0, and 2.5 model lines (single tier — the 2.5-pro >200k tier is **not** modeled; override via config when high-context billing matters):

```yaml
nexus.llm.gemini:
  pricing:
    gemini-2.5-pro:
      input_per_million: 2.50      # >200k tier
      output_per_million: 15.0
      cached_ratio: 0.25
```

`ReasoningTokens` are billed at the output rate. `CachedTokens` are billed at `input_per_million * cached_ratio`; remaining prompt tokens at the standard input rate.

### Retry Logic

Same retry surface as the OpenAI / Anthropic providers (`constant`, `linear`, `exponential`, `exponential_jitter`). Defaults retry 429 / 500 / 502 / 503 / 504. Honors `Retry-After` on 429 responses.

### Request Cancellation

Subscribes to `cancel.active` at priority 5. When a cancellation arrives, the in-flight HTTP request context is cancelled.

### Fallback Hook

Errors are emitted on the vetoable `before:core.error` event before the terminal `core.error`, letting `nexus.provider.fallback` swap to another provider in the chain.

## Example: api-key

```yaml
core:
  models:
    default: balanced
    balanced:
      provider: nexus.llm.gemini
      model: gemini-2.5-flash
      max_tokens: 8192

plugins:
  active:
    - nexus.io.tui
    - nexus.llm.gemini
    - nexus.agent.react

  nexus.llm.gemini:
    api_key_env: GEMINI_API_KEY
```

## Example: Vertex AI

```yaml
plugins:
  nexus.llm.gemini:
    auth: vertex
    location: us-central1
    project_id: my-gcp-project
    service_account_json: ~/.config/gcloud/keys/nexus-sa.json
```

## HTTP Configuration

- **Timeout**: 5 minutes per request
- **Public endpoint**: `https://generativelanguage.googleapis.com/v1beta`
- **Vertex endpoint**: `https://{location}-aiplatform.googleapis.com/v1`

## Search Grounding

A separate plugin, `nexus.search.gemini_native`, advertises the `search.provider` capability and answers `search.request` events using Gemini's `google_search` tool. Use it independently of the LLM provider — for example, run Anthropic for chat and Gemini for grounded search lookups.
