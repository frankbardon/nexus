# Model Registry

The model registry maps abstract **role names** to concrete model configurations. This lets plugins request a model by capability (e.g., "reasoning", "quick") without hardcoding specific model IDs.

## Role-Based Model Selection

Define model roles in the `core.models` config section:

```yaml
core:
  models:
    default: balanced        # Role to use when none specified
    reasoning:               # High-capability model for complex tasks
      provider: nexus.llm.anthropic
      model: claude-opus-4-20250514
      max_tokens: 16384
    balanced:                # General-purpose model
      provider: nexus.llm.anthropic
      model: claude-sonnet-4-20250514
      max_tokens: 8192
    quick:                   # Fast, cost-effective model
      provider: nexus.llm.anthropic
      model: claude-haiku-4-5-20251001
      max_tokens: 4096
```

## How Roles Are Used

Plugins reference roles by name in their configuration:

```yaml
nexus.planner.dynamic:
  model_role: reasoning     # Use the high-capability model for planning

nexus.memory.compaction:
  model_role: quick         # Use the fast model for summarization

nexus.agent.react:
  model_role: balanced      # Default agent model (optional, uses default role)
```

When a plugin emits an `llm.request`, the LLM provider resolves the role to a concrete model:

```go
// Plugin requests by role
config, found := models.Resolve("reasoning")
// Returns: ModelConfig{Provider: "nexus.llm.anthropic", Model: "claude-opus-4-20250514", MaxTokens: 16384}
```

## Resolution Rules

1. If the role name matches a defined role, return that config
2. If the role is empty, use the `default` role
3. If the role is not found but contains a hyphen (e.g., `claude-sonnet-4-20250514`), treat it as a raw model ID for backward compatibility
4. Otherwise, return not found

## Provider Fallback Chains

Role values can be ordered arrays instead of single maps. First entry = primary, subsequent entries are tried in order when the primary fails with a non-retryable error or exhausts its retry budget.

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
    quick:
      provider: nexus.llm.anthropic    # single entry = no fallback
      model: claude-haiku-4-5-20251001
```

Single-map format is backward compatible — parsed as a chain of length 1.

**Requires**: `nexus.provider.fallback` in `plugins.active` + both provider plugins active.

**Trigger conditions**: Fallback occurs when a provider error is non-retryable (4xx except 429, auth failures), or when the provider's own retry logic (429, 5xx backoff) has exhausted `max_retries`.

**Streaming partial failure**: If a provider fails mid-stream, the fallback plugin emits `io.output.clear` to wipe partial content, then `provider.fallback` notification, then re-emits `llm.request` targeting the next provider. Clean restart — no spliced output from two models.

## Provider Fanout

Roles with `fanout: true` send requests to all listed providers in parallel instead of sequential fallback. The fanout plugin collects responses and returns them as a single `LLMResponse` with `Alternatives`.

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
```

**Requires**: `nexus.provider.fanout` in `plugins.active` + all listed provider plugins active.

**Deadline**: Configurable via `nexus.provider.fanout.deadline_ms` (default 30s). If a provider doesn't respond in time, the fanout proceeds with available responses.

**No per-leg fallback**: A fanout provider that fails is simply marked as failed. Fallback chains and fanout are separate concepts — a role is either a fallback chain or a fanout group, not both.

## API

```go
type ModelConfig struct {
    Provider  string  // Plugin ID of the LLM provider
    Model     string  // Model identifier string
    MaxTokens int     // Maximum tokens for this model
}

type ModelRegistry struct {
    Resolve(role string) (ModelConfig, bool)              // Primary model for a role (index 0)
    Fallback(role string, attempt int) (ModelConfig, bool) // Model at chain index
    ChainLen(role string) int                             // Number of entries in fallback chain
    IsFanout(role string) bool                            // Whether a role uses parallel fanout
    FanoutProviders(role string) []ModelConfig            // All providers in a fanout role
    Default() ModelConfig                                 // Get the default model
    Roles() []string                                      // List all registered role names
}
```

## Default Role

The `default` key in the models config is a string alias pointing to another role:

```yaml
models:
  default: balanced   # When no role specified, use "balanced"
```

This means `models.Resolve("")` and `models.Default()` both return the `balanced` config.
