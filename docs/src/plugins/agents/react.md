# ReAct Agent

The ReAct (Reason + Act) agent is the default and most commonly used agent strategy. It runs an iterative loop: send messages to the LLM, parse the response, execute any tool calls, feed results back, and repeat until the LLM produces a final answer.

## Details

| | |
|---|---|
| **ID** | `nexus.agent.react` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `planning` | bool | `false` | Enable planning phase before iteration starts |
| `model_role` | string | *(default)* | Model role to use (e.g., `reasoning`, `balanced`, `quick`) |
| `system_prompt` | string | *(none)* | Inline system prompt text |
| `system_prompt_file` | string | *(none)* | Path to a system prompt markdown file |
| `parallel_tools` | bool | `false` | Run multiple tool calls from a single LLM response in parallel |
| `max_concurrent` | int | `4` | Concurrency cap when `parallel_tools: true` |
| `tool_choice` | string/object | *(none)* | Tool choice mode — shorthand string or object with `mode`/`name`/`sequence` |

> Iteration limits are not an agent setting — enforce them with
> `nexus.gate.endless_loop` (default cap: 25 LLM calls per turn).

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.input` | 50 | Receives user messages to start processing |
| `tool.result` | 50 | Receives results from tool execution |
| `llm.response` | 50 | Receives non-streaming LLM responses |
| `llm.stream.chunk` | 50 | Receives streaming response chunks |
| `llm.stream.end` | 50 | Streaming complete signal |
| `skill.loaded` | 50 | Receives loaded skill content |
| `tool.register` | 50 | Dynamically registers available tools |
| `plan.result` | 50 | Receives completed plans from planners |
| `cancel.active` | 5 | Handles cancellation |
| `cancel.resume` | 5 | Handles resumption after cancel |
| `memory.compacted` | 50 | Updates conversation history after compaction |
| `gate.llm.retry` | 50 | Retries previously vetoed LLM request |
| `agent.tool_choice` | 50 | Dynamic tool choice override from other plugins |

### Emits

| Event | When |
|-------|------|
| `llm.request` | Sending a message to the LLM |
| `before:tool.invoke` | Before executing a tool (vetoable — enables approval) |
| `tool.invoke` | Invoking a tool |
| `before:tool.result` | Before tool result propagation (vetoable) |
| `tool.result` | Synthetic tool results (e.g., for vetoed tools) |
| `before:io.output` | Before sending output (vetoable) |
| `io.output` | Final agent response to the user |
| `io.status` | Status updates (thinking, tool_running, etc.) |
| `thinking.step` | Reasoning/thinking steps for persistence |
| `plan.request` | Requesting a plan from a planner plugin |
| `agent.turn.start` | Beginning of a conversation turn |
| `agent.turn.end` | End of a conversation turn |

## How It Works

1. User sends a message → `io.input` arrives
2. Agent builds the message history and sends `llm.request`
3. LLM responds with text and/or tool calls
4. If tool calls exist:
   - Agent emits `before:tool.invoke` (can be vetoed for approval)
   - Agent emits `tool.invoke` for each tool call
   - Tool plugin emits `before:tool.result` (vetoable — gates can inspect/block)
   - Waits for `tool.result` events
   - Loops back to step 2 with tool results appended
5. If no tool calls, the LLM's response is the final answer → `io.output`
6. Stops when `nexus.gate.endless_loop` vetoes the next `llm.request` (default: 25 calls per turn)

## Planning Integration

When `planning: true`, the agent requests a plan before starting iteration:

1. Agent emits `plan.request` with the user's input
2. A planner plugin (dynamic or static) generates a plan
3. Agent receives `plan.result`
4. The plan steps are injected into the system prompt as context
5. Normal ReAct iteration begins with the plan as guidance

## Tool Choice

Controls whether the LLM must use tools. Supports static defaults, per-iteration sequences, and dynamic overrides.

### Static default (shorthand or object)

```yaml
nexus.agent.react:
  tool_choice: required              # shorthand: force tool use every iteration
  # or
  tool_choice:
    mode: auto                       # "auto" | "required" | "none" | "tool"
    name: shell                      # only when mode == "tool"
```

### Per-iteration sequence

```yaml
nexus.agent.react:
  tool_choice:
    sequence:
      - mode: required               # iteration 1: force tool use
      - mode: tool                   # iteration 2: force specific tool
        name: shell
      - mode: auto                   # iteration 3+: last entry repeats
```

### Dynamic override

Any plugin can emit `agent.tool_choice` with `AgentToolChoice{Mode, ToolName, Duration}`:
- `Duration: "once"` — applies to next LLM request only, then reverts to config default.
- `Duration: "sticky"` — persists until replaced by another override. Reset on new turn.

## Example Configuration

```yaml
nexus.agent.react:
  planning: true
  model_role: balanced
  system_prompt: |
    You are a coding assistant powered by Nexus. You help users write, debug, refactor, and understand code.

    ## Guidelines

    1. Always explain your reasoning before making changes
    2. Run tests after modifications to verify correctness
    3. Prefer minimal, targeted changes over broad refactors
    4. Ask for clarification when requirements are ambiguous
    5. Read files in chunks 16kb or less
    6. Follow the existing code style and conventions of the project
  tool_choice:
    sequence:
      - mode: required
      - mode: auto
```

## Tool Discovery

The agent discovers tools dynamically through `tool.register` events. When tool plugins initialize, they emit their tool definitions. The agent collects these and includes them in every `llm.request`.

This means adding a tool to your agent is as simple as adding it to the `active` list — no explicit wiring needed.
