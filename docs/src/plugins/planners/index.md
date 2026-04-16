# Planner Plugins

Planners generate execution plans before the agent starts iterating. They're optional — enable them when you want structured task decomposition before action.

## Available Planners

| Plugin | ID | Strategy |
|--------|----|----------|
| [Dynamic Planner](./dynamic.md) | `nexus.planner.dynamic` | LLM generates a plan from the user's input |
| [Static Planner](./static.md) | `nexus.planner.static` | Returns a fixed set of steps from config |

## How Planning Works

1. The agent (with `planning: true`) emits `plan.request` with the user's input
2. The active planner generates a plan
3. Plan is delivered via `plan.result`
4. Optionally, the user is asked to approve via `plan.approval.request`
5. The agent injects the plan into its system prompt and begins iteration

## Plan Persistence

Plans are persisted to the session under `plugins/<planner-id>/<plan-id>/`:

| File | Content |
|------|---------|
| `plan.json` | The generated plan steps |
| `request.json` | The original plan request |
| `approval.json` | The approval decision (if applicable) |
