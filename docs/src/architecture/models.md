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

## API

```go
type ModelConfig struct {
    Provider  string  // Plugin ID of the LLM provider
    Model     string  // Model identifier string
    MaxTokens int     // Maximum tokens for this model
}

type ModelRegistry struct {
    Resolve(role string) (ModelConfig, bool)  // Look up a role
    Default() ModelConfig                      // Get the default model
    Roles() []string                           // List all registered role names
}
```

## Default Role

The `default` key in the models config is a string alias pointing to another role:

```yaml
models:
  default: balanced   # When no role specified, use "balanced"
```

This means `models.Resolve("")` and `models.Default()` both return the `balanced` config.
