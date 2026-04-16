# Plugin Overview

Nexus ships with 21 built-in plugins organized into categories. Activate only the plugins you need — the engine handles dependency resolution and boot ordering automatically.

## Plugin Categories

| Category | Count | Purpose |
|----------|-------|---------|
| [Agents](./agents/index.md) | 4 | Core reasoning loops — ReAct, Plan & Execute, Subagent, Orchestrator |
| [LLM Providers](./providers/index.md) | 2 | LLM API integration (Anthropic, OpenAI) |
| [Tools](./tools/index.md) | 5 | Capabilities the agent can invoke — shell, files, PDF, opener, ask user |
| [Memory](./memory/index.md) | 2 | Conversation persistence and context window compaction |
| [I/O Interfaces](./io/index.md) | 2 | User interaction — terminal UI and browser-based UI |
| [Observers](./observers/index.md) | 2 | Event logging and thinking step persistence |
| [Planners](./planners/index.md) | 2 | Execution planning — LLM-generated or pre-configured |
| [Skills](./skills.md) | 1 | Skill discovery and management |
| [System](./system.md) | 1 | Dynamic system prompt variables |
| [Control](./control.md) | 1 | Cancellation coordination |

## Choosing Plugins

A minimal useful agent needs at least:

- **One I/O plugin** — How the user interacts (`nexus.io.tui` or `nexus.io.browser`)
- **One LLM provider** — Which AI model to use (`nexus.llm.anthropic`, `nexus.llm.openai`)
- **One agent** — The reasoning strategy (`nexus.agent.react` is the most common)

Everything else is optional. Add tools to give the agent capabilities, memory to persist conversations, planners for complex task decomposition, and observers for debugging.

## Common Combinations

### Conversational Agent (no tools)
```yaml
active:
  - nexus.io.tui
  - nexus.llm.anthropic
  - nexus.agent.react
  - nexus.memory.conversation
```

### Coding Assistant
```yaml
active:
  - nexus.io.tui
  - nexus.llm.anthropic
  - nexus.agent.react
  - nexus.tool.shell
  - nexus.tool.file
  - nexus.tool.ask
  - nexus.skills
  - nexus.memory.conversation
```

### Planned Coding Workflow
```yaml
active:
  - nexus.io.tui
  - nexus.llm.anthropic
  - nexus.agent.react
  - nexus.planner.dynamic
  - nexus.observe.thinking
  - nexus.tool.shell
  - nexus.tool.file
  - nexus.tool.ask
  - nexus.skills
  - nexus.memory.conversation
  - nexus.memory.compaction
```

### Multi-Agent Orchestration
```yaml
active:
  - nexus.io.tui
  - nexus.llm.anthropic
  - nexus.agent.orchestrator
  - nexus.agent.subagent
  - nexus.agent.react
  - nexus.tool.shell
  - nexus.tool.file
  - nexus.memory.conversation
  - nexus.control.cancel
```

## Plugin Documentation Format

Each plugin page in this reference covers:

- **ID** — The plugin identifier used in config
- **Purpose** — What it does and when to use it
- **Configuration** — All config options with types and defaults
- **Events** — What it subscribes to and emits
- **Dependencies** — Other plugins it requires
- **Usage examples** — Config snippets and common patterns
