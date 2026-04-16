# Agent Plugins

Agents are the brain of a Nexus harness. They receive user input, orchestrate LLM calls and tool usage, and produce output. You must activate exactly one agent plugin (unless using the orchestrator pattern, which depends on subagent + react).

## Available Agents

| Plugin | ID | Strategy |
|--------|----|----------|
| [ReAct](./react.md) | `nexus.agent.react` | Iterative reason-and-act loop |
| [Plan & Execute](./planexec.md) | `nexus.agent.planexec` | Create a plan first, then execute step by step |
| [Subagent](./subagent.md) | `nexus.agent.subagent` | Spawns child agents as tools |
| [Orchestrator](./orchestrator.md) | `nexus.agent.orchestrator` | Decomposes tasks and dispatches to parallel workers |

## Choosing an Agent

- **ReAct** — Best for most use cases. Simple, flexible, supports planning as an optional phase. Start here.
- **Plan & Execute** — When you want a mandatory planning phase with explicit step tracking and optional replanning on failure.
- **Orchestrator** — For complex tasks that benefit from parallel decomposition across multiple subagent workers.
- **Subagent** — Not used standalone. Provides a `spawn_subagent` tool that other agents (or the orchestrator) can invoke.

## Agent + Planner Interaction

The ReAct and Plan & Execute agents can optionally integrate with planners:

- **ReAct + Dynamic Planner** — LLM generates a plan before the agent starts iterating. The plan is injected into the system prompt.
- **ReAct + Static Planner** — Fixed steps from config are injected into the system prompt.
- **Plan & Execute** — Has its own built-in planning phase (uses LLM directly, no separate planner plugin needed).
