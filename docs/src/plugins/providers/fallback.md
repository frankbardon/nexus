# Provider Fallback

**Plugin ID**: `nexus.provider.fallback`

Automatic provider failover when the primary LLM provider returns a non-retryable error or exhausts its retry budget. Agents remain unaware — fallback is transparent at the provider layer.

## Event Subscriptions

| Event | Priority | Purpose |
|-------|----------|---------|
| `before:llm.request` | 3 | Inject fallback tracking metadata |
| `before:core.error` | 5 | Intercept provider errors for fallback |

## Event Emissions

| Event | Payload | Purpose |
|-------|---------|---------|
| `io.output.clear` | *(none)* | Wipe partial streamed content from UI |
| `provider.fallback` | `ProviderFallback` | Notify UI of provider switch |
| `llm.request` | `LLMRequest` | Re-emit request targeting fallback provider |

## Configuration

No plugin-specific config. Fallback chains are defined in `core.models`:

```yaml
core:
  models:
    balanced:
      - provider: nexus.llm.anthropic
        model: claude-sonnet-4-20250514
        max_tokens: 8192
      - provider: nexus.llm.openai
        model: gpt-4o
        max_tokens: 8192

plugins:
  active:
    - nexus.llm.anthropic
    - nexus.llm.openai
    - nexus.provider.fallback    # required for fallback to work
```

## Trigger Conditions

Fallback occurs when:

- **Non-retryable errors**: 4xx status codes (except 429), malformed responses, auth failures
- **Retries exhausted**: Provider's own retry logic (429, 5xx backoff) has hit `max_retries` and given up

Fallback does **not** occur for: cancellation, context deadline, or errors that the provider's built-in retry is still handling.

## Streaming Partial Failure

If a provider fails mid-stream:

1. Emits `io.output.clear` to wipe partial streamed content from UI
2. Emits `provider.fallback` notification so user sees "Switching to [provider]..."
3. Re-emits `llm.request` targeting next provider in chain

Clean restart — splicing output from two different models produces incoherent text.

## What Stays Unchanged

- **Agent plugins** — unaware of fallback. Emit `llm.request`, receive `llm.response`.
- **Provider routing** — providers still check `cfg.Provider != pluginID` and skip non-matching requests.
- **Single-provider configs** — backward compatible, parsed as chain of length 1.
- **Gate plugins** — operate on `before:llm.request` as before. Fallback re-emits go through same gate checks.

## Request Metadata

The fallback plugin injects tracking metadata into requests for roles with fallback chains:

| Key | Type | Purpose |
|-----|------|---------|
| `_fallback_id` | string | Unique ID for this fallback sequence |
| `_fallback_attempt` | int | Current index in the chain (0 = primary) |
| `_fallback_role` | string | Role name being resolved |

This metadata flows through the provider and back via `ErrorInfo.RequestMeta` on failure, enabling the plugin to correlate errors with their original requests.
