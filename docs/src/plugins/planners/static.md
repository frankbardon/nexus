# Static Planner

Returns a pre-configured set of steps from the YAML config. No LLM call is needed — the plan is always the same regardless of input. Useful for enforcing a consistent workflow.

## Details

| | |
|---|---|
| **ID** | `nexus.planner.static` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `approval` | string | `never` | Approval mode: `always`, `never`, `auto` (defaults to `never` for static) |
| `summary` | string | *(none)* | Human-readable summary of the plan |
| `steps` | list | *(required)* | List of step objects |

### Step Object

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `description` | string | Yes | What this step does |
| `instructions` | string | No | Detailed instructions for the agent |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `plan.request` | 50 | Receives plan requests |

### Emits

| Event | When |
|-------|------|
| `plan.result` | Immediately returns the configured plan |

## Example Configuration

```yaml
nexus.planner.static:
  approval: never
  summary: "Standard coding workflow"
  steps:
    - description: "Analyze the request and identify affected files"
    - description: "Plan the implementation approach"
    - description: "Implement the changes"
    - description: "Verify correctness"
    - description: "Summarize what was done"
```

### With Detailed Instructions

```yaml
nexus.planner.static:
  approval: always
  summary: "Code review workflow"
  steps:
    - description: "Read the changed files"
      instructions: "Use the file tool to read all files mentioned in the request. Understand the full context."
    - description: "Identify issues"
      instructions: "Check for bugs, security issues, performance problems, and style violations."
    - description: "Write the review"
      instructions: "Summarize findings by severity. Include code suggestions for each issue."
```
