# Subagent

The subagent plugin provides a tool that spawns independent child agents. Other agents (or the orchestrator) can invoke it to delegate subtasks. Each instance can be configured with its own system prompt, tool name, and model role.

## Details

| | |
|---|---|
| **ID** | `nexus.agent.subagent` |
| **Dependencies** | `nexus.agent.react` |
| **Multi-instance** | Yes — supports instance suffixes (e.g., `nexus.agent.subagent/researcher`) |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_iterations` | int | `10` | Max iterations for spawned agents |
| `model_role` | string | *(default)* | Default model role for spawned agents |
| `system_prompt` | string | *(none)* | Default system prompt for spawned agents |
| `system_prompt_file` | string | *(none)* | Path to default system prompt file |
| `tool_name` | string | `spawn_subagent` | Name of the tool exposed to the parent agent |
| `tool_description` | string | *(auto)* | Description of the tool |

## Tool Definition

The subagent registers a tool (default name: `spawn_subagent`) with these parameters:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task` | string | Yes | The task description for the subagent |
| `system_prompt` | string | No | Override the default system prompt |
| `model_role` | string | No | Override the default model role |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handles spawn tool invocations |
| `tool.register` | 50 | Collects available tools |

### Emits

| Event | When |
|-------|------|
| `tool.register` | Registers the spawn tool at boot |
| `subagent.spawn` | When a new subagent is created |

## Multi-Instance Usage

Each instance creates its own spawn tool. Use instance suffixes to create specialized spawners:

```yaml
plugins:
  active:
    - nexus.agent.react
    - nexus.agent.subagent/researcher
    - nexus.agent.subagent/writer

  nexus.agent.subagent/researcher:
    max_iterations: 15
    model_role: reasoning
    system_prompt: "You are a research specialist. Gather information thoroughly."
    tool_name: spawn_researcher

  nexus.agent.subagent/writer:
    max_iterations: 10
    model_role: balanced
    system_prompt: "You are a technical writer. Produce clear, concise documentation."
    tool_name: spawn_writer
```

The parent agent will see two tools: `spawn_researcher` and `spawn_writer`.

## Subagent Events

When a subagent runs, these events are emitted:

| Event | Payload | When |
|-------|---------|------|
| `subagent.spawn` | SpawnID, Task, ParentTurnID | Subagent created |
| `subagent.started` | SpawnID, Task, ParentTurnID | Subagent begins execution |
| `subagent.iteration` | SpawnID, Iteration, Content | Each reasoning iteration |
| `subagent.complete` | SpawnID, Result, TokensUsed, CostUSD | Subagent finished |

## Example

A ReAct agent with a research subagent:

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.agent.subagent
    - nexus.memory.conversation

  nexus.agent.react:
    max_iterations: 10
    system_prompt: "You can delegate research tasks using the spawn_subagent tool."

  nexus.agent.subagent:
    max_iterations: 10
    system_prompt: "Take in the user input and summarize the problem."
```
