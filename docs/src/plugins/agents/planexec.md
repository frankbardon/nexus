# Plan & Execute Agent

The Plan & Execute agent separates planning from execution. It delegates plan
*generation* to whichever planner plugin is active on the bus (e.g.
`nexus.planner.dynamic` or `nexus.planner.static`) while retaining full
control of the surrounding flow: phase transitions, approval, step execution,
re-planning on failure, and final synthesis.

This means you can change how plans are produced — LLM-driven, static,
role-specialized, or a custom planner you build — without modifying the
agent.

## Details

| | |
|---|---|
| **ID** | `nexus.agent.planexec` |
| **Dependencies** | A planner plugin (e.g. `nexus.planner.dynamic`) must be active |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_iterations` | int | `15` | Maximum LLM iterations per step during execution |
| `execution_model_role` | string | `balanced` | Model role used for step execution and synthesis |
| `replan_on_failure` | bool | `true` | Request a fresh plan if a step fails (up to 2 replans per turn) |
| `approval` | string | `never` | When planexec requires approval after the planner returns a plan: `always`, `never` |
| `system_prompt` | string | *(none)* | Inline system prompt for execution/synthesis |
| `system_prompt_file` | string | *(none)* | Path to system prompt file |

Plan-generation options (model, prompt, max steps, planner-side approval) now
live on the planner plugin itself. See the planner docs.

### Approval layers

Two independent approval gates may apply:

1. **Planner-side** — e.g. `nexus.planner.dynamic` supports `always` / `auto`
   / `never`. When the planner denies or the user rejects, the planner emits
   `plan.result` with `Approved: false` and planexec will end the turn.
2. **planexec-side** — even when the planner returns an approved plan,
   setting planexec's own `approval: always` will emit a second
   `plan.approval.request` before execution begins.

To avoid double-prompting, pick one side to own approval and set the other
to `never`.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.input` | 50 | Receives user messages |
| `tool.result` | 50 | Tool execution results |
| `llm.response` | 50 | Step execution and synthesis responses |
| `llm.stream.chunk` / `llm.stream.end` | 50 | Streaming synthesis output |
| `skill.loaded` | 50 | Skill content |
| `tool.register` | — | Tool discovery |
| `plan.result` | 50 | Receives generated plans from the active planner |
| `plan.approval.response` | 50 | User response to planexec-side approval |
| `memory.compacted` | 50 | History compaction |

### Emits

| Event | When |
|-------|------|
| `plan.request` | Start of a turn, or when re-planning after a step failure |
| `plan.approval.request` | When planexec's own `approval: always` is set |
| `llm.request` | Step execution and final synthesis |
| `before:tool.invoke` / `tool.invoke` | Tool invocation |
| `agent.plan` | After each step status change |
| `io.status` | Phase transitions |
| `thinking.step` | Reasoning steps |
| `agent.turn.start` / `agent.turn.end` | Turn boundaries |

## Phases

The agent transitions through these phases:

```
idle → planning → awaiting_approval → executing → synthesizing → idle
```

1. **Planning** — Emits `plan.request`; waits for `plan.result` from the
   active planner.
2. **Awaiting Approval** — Only entered if planexec's own `approval: always`.
3. **Executing** — Runs each step sequentially, with its own message history
   and iteration budget.
4. **Synthesizing** — After all steps complete, generates a summary of
   results.

## Replanning

When `replan_on_failure: true` and a step fails, the agent:

1. Collects the status and results of completed/failed/pending steps.
2. Emits a fresh `plan.request` whose `Input` contains the original request
   plus a structured summary of what happened.
3. Waits for the new `plan.result` and resumes execution with the revised
   plan.

Because re-planning is just another `plan.request`, any planner
implementation automatically participates.

## Example Configuration

```yaml
plugins:
  active:
    - nexus.agent.planexec
    - nexus.planner.dynamic    # any planner plugin works
    # ...

  nexus.agent.planexec:
    max_iterations: 15
    execution_model_role: balanced
    replan_on_failure: true
    approval: never
    system_prompt_file: ./prompts/coding-assistant.md

  nexus.planner.dynamic:
    model_role: reasoning
    max_steps: 8
    approval: auto
```

## When to Use

Choose Plan & Execute over ReAct when:

- Tasks are complex and benefit from upfront decomposition.
- You want explicit step tracking and progress visibility.
- You want to swap planning strategies (LLM, static, domain-specific)
  without touching the agent.
- You need a different model tier for planning vs. execution.
