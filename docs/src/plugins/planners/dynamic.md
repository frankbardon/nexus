# Dynamic Planner

Uses an LLM to generate an execution plan from the user's input. The plan is a sequence of steps with descriptions and optional detailed instructions.

## Details

| | |
|---|---|
| **ID** | `nexus.planner.dynamic` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `approval` | string | `always` | Approval mode: `always` (user must approve), `never` (skip), `auto` (LLM decides) |
| `model_role` | string | *(default)* | Model role for plan generation |
| `max_steps` | int | `10` | Maximum number of plan steps |
| `plan_prompt` | string | *(built-in)* | Custom inline planning prompt |
| `plan_prompt_file` | string | *(none)* | Path to a custom planning prompt file |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `plan.request` | 50 | Receives plan generation requests |
| `llm.response` | 50 | Receives the LLM's generated plan |

### Emits

| Event | When |
|-------|------|
| `plan.result` | Plan generated and ready |
| `thinking.step` | Planning reasoning |
| `io.status` | Status updates during plan generation |

## How It Works

1. Receives `plan.request` with user input
2. Constructs a prompt asking the LLM to generate a JSON plan
3. Sends `llm.request` tagged with `Metadata["_source"]` so the ReAct agent ignores this response
4. Parses the LLM response as JSON steps
5. Emits `plan.result` with the steps

## Approval Modes

| Mode | Behavior |
|------|----------|
| `always` | User must explicitly approve before execution |
| `never` | Plan is executed immediately |
| `auto` | The LLM includes a risk assessment; low-risk plans skip approval |

## Example Configuration

```yaml
nexus.planner.dynamic:
  approval: auto
  max_steps: 10
  model_role: reasoning
```
