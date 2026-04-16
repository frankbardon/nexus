# Anthropic (Claude) Provider

The Anthropic provider calls the Claude API via direct HTTP requests — no SDK dependency. It supports streaming, tool use, request cancellation, and automatic retries.

## Details

| | |
|---|---|
| **ID** | `nexus.llm.anthropic` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_key_env` | string | `ANTHROPIC_API_KEY` | Name of the environment variable containing the API key |
| `debug` | bool | `false` | Log raw request/response bodies to the session plugin directory |

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

When `llm.request.Stream` is `true`, the provider uses Server-Sent Events (SSE) to stream the response. Each content chunk and tool use block generates a `llm.stream.chunk` event. When streaming completes, `llm.stream.end` carries the full usage statistics.

### Tool Calling

The provider translates Nexus tool definitions into the Anthropic `tool_use` format. Tool call responses from the API are parsed and included in the `llm.response` or final `llm.stream.end` event.

### Prompt Assembly

Before sending a request, the provider calls `PromptRegistry.Apply()` to append dynamic sections (skills catalog, system variables, etc.) to the system prompt.

### Request Cancellation

Subscribes to `cancel.active` at priority 5. When a cancellation arrives, the in-flight HTTP request context is cancelled, aborting the API call.

### Retry Logic

Transient errors (rate limits, server errors) are retried with exponential backoff.

### Debug Mode

When `debug: true`, raw request and response JSON bodies are written to the session's plugin directory for inspection.

## HTTP Configuration

- **Timeout**: 5 minutes per request
- **API endpoint**: `https://api.anthropic.com/v1/messages`

## Example Configuration

```yaml
nexus.llm.anthropic:
  api_key_env: ANTHROPIC_API_KEY
  debug: false
```

To use a different environment variable for the API key:

```yaml
nexus.llm.anthropic:
  api_key_env: MY_CLAUDE_KEY
```
