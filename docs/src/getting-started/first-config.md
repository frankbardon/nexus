# Your First Configuration

Nexus configuration is a single YAML file with two top-level sections: `core` (engine settings) and `plugins` (what to activate and how to configure it).

## Minimal Configuration

Here's the simplest useful configuration — a conversational agent with no tools:

```yaml
core:
  log_level: warn
  models:
    default: balanced
    balanced:
      provider: nexus.llm.anthropic
      model: claude-sonnet-4-20250514
      max_tokens: 8192

plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.memory.capped

  nexus.agent.react:
    max_iterations: 10
    system_prompt: "You are a helpful assistant."

  nexus.memory.capped:
    max_messages: 100
    persist: true
```

Save this as `my-agent.yaml` and run it:

```bash
bin/nexus -config my-agent.yaml
```

## Understanding the Structure

### Core Section

The `core` section configures the engine itself:

```yaml
core:
  log_level: warn          # debug | info | warn | error
  tick_interval: 5s        # heartbeat interval
  max_concurrent_events: 100

  models:
    default: balanced      # which role to use when none specified
    reasoning:             # high-capability model for planning
      provider: nexus.llm.anthropic
      model: claude-opus-4-20250514
      max_tokens: 16384
    balanced:              # general-purpose model
      provider: nexus.llm.anthropic
      model: claude-sonnet-4-20250514
      max_tokens: 8192
    quick:                 # fast model for simple tasks
      provider: nexus.llm.anthropic
      model: claude-haiku-4-5-20251001
      max_tokens: 4096

  sessions:
    root: ~/.nexus/sessions
    retention: 30d
    id_format: datetime_short
```

### Plugins Section

The `plugins` section has two parts:

1. **`active`** — a list of plugin IDs to load (order doesn't matter; dependencies are resolved automatically)
2. **Per-plugin config** — each key matching a plugin ID provides that plugin's settings

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.tool.shell
    - nexus.tool.file

  nexus.tool.shell:
    allowed_commands: ["ls", "cat", "grep", "find"]
    timeout: 30s
    sandbox: true
```

## Adding Tools

To give your agent capabilities, add tool plugins to the active list and configure them:

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.tool.shell          # Shell command execution
    - nexus.tool.file           # File read/write/list
    - nexus.tool.ask            # Ask user questions
    - nexus.memory.capped

  nexus.tool.shell:
    allowed_commands: ["go", "git", "ls", "cat", "grep", "make"]
    timeout: 30s
    sandbox: true
```

Tools register themselves automatically when the agent starts. The agent discovers available tools through the event bus — no explicit wiring needed.

## Adding Planning

To enable a planning phase before the agent acts, add a planner plugin and set `planning: true` on the agent:

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.planner.dynamic
    - nexus.observe.thinking
    # ... other plugins

  nexus.agent.react:
    max_iterations: 10
    planning: true
    system_prompt: |
      You are a coding assistant powered by Nexus. You help users write, debug, refactor, and understand code.

      ## Guidelines

      1. Always explain your reasoning before making changes
      2. Run tests after modifications to verify correctness
      3. Prefer minimal, targeted changes over broad refactors
      4. Ask for clarification when requirements are ambiguous
      5. Read files in chunks 16kb or less
      6. Follow the existing code style and conventions of the project

  nexus.planner.dynamic:
    approval: auto          # always | never | auto
    max_steps: 10
    model_role: reasoning   # use the high-capability model for planning
```

See [Dynamic Planner](../plugins/planners/dynamic.md) and [Static Planner](../plugins/planners/static.md) for details.

## Using System Prompts

System prompts can be defined inline or loaded from a file:

```yaml
# Inline
nexus.agent.react:
  system_prompt: "You are a coding assistant. Be concise and precise."

# From file
nexus.agent.react:
  system_prompt: |
      You are a coding assistant powered by Nexus. You help users write, debug, refactor, and understand code.

      ## Guidelines

      1. Always explain your reasoning before making changes
      2. Run tests after modifications to verify correctness
      3. Prefer minimal, targeted changes over broad refactors
      4. Ask for clarification when requirements are ambiguous
      5. Read files in chunks 16kb or less
      6. Follow the existing code style and conventions of the project
```

## Next Steps

- Learn about the [architecture](../architecture/overview.md) to understand how plugins communicate
- Browse the [plugin reference](../plugins/overview.md) to see what's available
