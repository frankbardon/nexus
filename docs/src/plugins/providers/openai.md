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
