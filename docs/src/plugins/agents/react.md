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
| `max_iterations` | int | `25` | Maximum number of reason-act cycles before stopping |
| `planning` | bool | `false` | Enable planning phase before iteration starts |
| `model_role` | string | *(default)* | Model role to use (e.g., `reasoning`, `balanced`, `quick`) |
| `system_prompt` | string | *(none)* | Inline system prompt text |
| `system_prompt_file` | string | *(none)* | Path to a system prompt markdown file |

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

### Emits

| Event | When |
|-------|------|
| `llm.request` | Sending a message to the LLM |
| `before:tool.invoke` | Before executing a tool (vetoable — enables approval) |
| `tool.invoke` | Invoking a tool |
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
   - Waits for `tool.result` events
   - Loops back to step 2 with tool results appended
5. If no tool calls, the LLM's response is the final answer → `io.output`
6. Stops if `max_iterations` is reached

## Planning Integration

When `planning: true`, the agent requests a plan before starting iteration:

1. Agent emits `plan.request` with the user's input
2. A planner plugin (dynamic or static) generates a plan
3. Agent receives `plan.result`
4. The plan steps are injected into the system prompt as context
5. Normal ReAct iteration begins with the plan as guidance

## Example Configuration

```yaml
nexus.agent.react:
  max_iterations: 15
  planning: true
  model_role: balanced
  system_prompt_file: ./prompts/coding-assistant.md
```

## Tool Discovery

The agent discovers tools dynamically through `tool.register` events. When tool plugins initialize, they emit their tool definitions. The agent collects these and includes them in every `llm.request`.

This means adding a tool to your agent is as simple as adding it to the `active` list — no explicit wiring needed.
