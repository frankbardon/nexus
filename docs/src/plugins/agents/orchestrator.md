# Orchestrator

The orchestrator implements a manager-worker pattern. It uses an LLM to decompose complex tasks into subtasks, then dispatches them to parallel subagent workers. Results are synthesized into a final response.

## Details

| | |
|---|---|
| **ID** | `nexus.agent.orchestrator` |
| **Dependencies** | `nexus.agent.subagent` |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_workers` | int | `5` | Maximum concurrent subagent workers |
| `max_subtasks` | int | `8` | Maximum number of subtasks to decompose into |
| `worker_max_iterations` | int | `10` | Max iterations per worker subagent |
| `orchestrator_model_role` | string | `reasoning` | Model for task decomposition |
| `worker_model_role` | string | `balanced` | Model for worker execution |
| `synthesis_model_role` | string | `balanced` | Model for result synthesis |
| `fail_fast` | bool | `false` | Stop all workers if one fails |
| `system_prompt` | string | *(none)* | System prompt for the orchestrator |
| `system_prompt_file` | string | *(none)* | Path to system prompt file |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.input` | 50 | Receives user tasks |
| `tool.result` | 50 | Tool results during decomposition |
| `llm.response` / `llm.stream.*` | 50 | LLM responses |
| `skill.loaded` | 50 | Skill content |
| `tool.register` | 50 | Tool discovery |
| `subagent.started` | 50 | Worker started notification |
| `subagent.iteration` | 50 | Worker progress |
| `subagent.complete` | 50 | Worker finished |
| `cancel.active` / `cancel.resume` | 5 | Cancellation |
| `memory.compacted` | 50 | History compaction |

### Emits

| Event | When |
|-------|------|
| `llm.request` | Decomposition and synthesis LLM calls |
| `before:tool.invoke` / `tool.invoke` | Tool invocation |
| `thinking.step` | Decomposition and synthesis reasoning |
| `io.status` | Phase transitions |
| `agent.turn.start` / `agent.turn.end` | Turn boundaries |

## Phases

```
idle → decomposing → dispatching → executing → synthesizing → idle
```

1. **Decomposing** — The orchestrator LLM breaks the task into subtasks with descriptions and optional dependencies
2. **Dispatching** — Subtasks are queued and sent to subagent workers (respecting `max_workers` concurrency)
3. **Executing** — Workers run in parallel; the orchestrator tracks progress and collects results
4. **Synthesizing** — All results are gathered and the synthesis LLM produces a unified response

## Subtask Dependencies

The orchestrator can recognize dependencies between subtasks. Dependent subtasks wait until their prerequisites complete before dispatching.

## Failure Handling

- **`fail_fast: false`** (default) — Other workers continue even if one fails. Failed results are included in synthesis.
- **`fail_fast: true`** — All remaining workers are cancelled when any worker fails.

## Example Configuration

```yaml
plugins:
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

  nexus.agent.orchestrator:
    max_workers: 3
    max_subtasks: 6
    orchestrator_model_role: reasoning
    worker_model_role: balanced
    synthesis_model_role: balanced
    fail_fast: false

  nexus.agent.subagent:
    max_iterations: 10
```

## When to Use

The orchestrator is ideal for:

- **Complex tasks** that naturally decompose into independent subtasks
- **Research** across multiple topics that can be explored in parallel
- **Code analysis** across multiple files or modules simultaneously
- **Any task** where parallel execution significantly reduces total time
