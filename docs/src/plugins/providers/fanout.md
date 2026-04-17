# Provider Fanout

**Plugin ID**: `nexus.provider.fanout`

Sends a single LLM request to multiple providers in parallel and collects their responses. Supports configurable selection strategies to determine the final response. Agents remain unaware — fanout is transparent at the provider layer.

## Event Subscriptions

| Event | Priority | Purpose |
|-------|----------|---------|
| `before:llm.request` | 2 | Detect fanout roles, veto original, dispatch parallel requests |
| `llm.response` | 1 | Collect individual provider responses |
| `before:core.error` | 4 | Absorb provider errors within fanout sequences |

## Event Emissions

| Event | Payload | Purpose |
|-------|---------|---------|
| `provider.fanout.start` | `ProviderFanoutStart` | Fanout initiated |
| `provider.fanout.response` | `ProviderFanoutResponse` | Individual provider responded (success or failure) |
| `provider.fanout.complete` | `ProviderFanoutComplete` | All responses collected or deadline reached |
| `llm.request` | `LLMRequest` | Per-provider targeted requests (via EmitAsync) |
| `llm.response` | `LLMResponse` | Combined final response with Alternatives |

## Configuration

```yaml
core:
  models:
    compare:
      fanout: true
      providers:
        - provider: nexus.llm.anthropic
          model: claude-sonnet-4-20250514
          max_tokens: 4096
        - provider: nexus.llm.openai
          model: gpt-4o
          max_tokens: 4096

plugins:
  active:
    - nexus.llm.anthropic
    - nexus.llm.openai
    - nexus.provider.fanout    # required for fanout to work

  nexus.provider.fanout:
    strategy: all              # selection strategy (default: "all")
    deadline_ms: 30000         # max wait time in milliseconds (default: 30000)
```

### Plugin Config

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `strategy` | string | `"all"` | Selection strategy: `all`, `llm_judge`, `heuristic`, `user` |
| `deadline_ms` | int | `30000` | Maximum time to wait for all providers (milliseconds) |

### Fanout Role Config

Fanout roles are defined in `core.models` as maps with `fanout: true` and a `providers` list:

```yaml
core:
  models:
    compare:
      fanout: true
      providers:
        - provider: nexus.llm.anthropic
          model: claude-sonnet-4-20250514
        - provider: nexus.llm.openai
          model: gpt-4o
```

This is distinct from fallback chains (which are YAML arrays). Fanout roles dispatch to all providers simultaneously; fallback chains try providers sequentially on error.

## Selection Strategies

| Strategy | Description | Status |
|----------|-------------|--------|
| `all` | Return all responses. First response is primary, rest in `Alternatives`. | Implemented |
| `llm_judge` | Separate LLM call picks best response. | Planned |
| `heuristic` | Rule-based selection (response length, latency, confidence). | Planned |
| `user` | Surface all to user, let them pick. | Planned |

## Response Shape

The combined response uses the `Alternatives` field on `LLMResponse`:

- Primary fields (`Content`, `Model`, `ToolCalls`, etc.) contain the first successful response
- `Alternatives` contains remaining successful responses as full `LLMResponse` structs
- `Usage` and `CostUSD` are aggregated across all responses
- `Metadata["_fanout"] = true` indicates this was a fanout response
- `Metadata["_fanout_id"]` contains the fanout sequence ID

## Deadline Handling

If any provider doesn't respond within `deadline_ms`, the fanout finalizes with whatever responses have arrived. Failed or timed-out providers are counted in `ProviderFanoutComplete.Failed`.

If all providers fail or time out, the plugin emits a `core.error` instead of an `llm.response`.

## Streaming

Fanout disables streaming for individual provider requests (`Stream: false`). Complete responses are required to support selection strategies and the `Alternatives` response shape.

## Request Metadata

The fanout plugin injects tracking metadata into per-provider requests:

| Key | Type | Purpose |
|-----|------|---------|
| `_fanout_id` | string | Unique ID for this fanout sequence |
| `_target_provider` | string | Plugin ID of the target provider |
| `_fanout_provider` | string | Plugin ID that this leg targets |
| `_source` | string | Set to `nexus.provider.fanout` so agents skip individual responses |

## Interaction with Other Plugins

- **Fallback**: Fanout and fallback serve different purposes. Fallback is sequential error recovery; fanout is parallel dispatch. A fanout leg that fails is simply marked as failed — no per-leg fallback.
- **Gates**: The initial `before:llm.request` passes through gates normally. Per-provider requests emitted by fanout are direct `llm.request` events (not vetoable).
- **Mock mode**: Test IO mock responses work with fanout — each fanout leg gets the next mock response in sequence.
