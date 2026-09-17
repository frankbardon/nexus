# Delegate (`nexus.agent.delegate`)

Exposes the `delegate` tool — the LLM-facing surface of the
[sub-agent delegation](../../architecture/delegate.md) primitive. A parent
agent picks a registered [posture](postures.md) by name; the runtime spawns
an isolated sub-session with its own envelope identity, runs the LLM loop
filtered to the posture's `AllowedTools`, and enforces the posture's
`DefaultBudget` with optional per-call overrides.

## Details

| | |
|---|---|
| **ID** | `nexus.agent.delegate` |
| **Dependencies** | *(none)* |
| **Requires** | Capability `posture.registry` (typically `nexus.agent.postures`) |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_depth` | int | `3` | Hard cap on sub-agent recursion depth across all postures. Individual postures may tighten with `max_recursion_depth`. |
| `cache_size` | int | `256` | Capacity of the in-process LRU result cache (entries, not bytes). Zero disables eviction. |
| `cache` | bool | `true` | Set `false` to disable result caching entirely. |

## Tool definition

The plugin registers a single tool, `delegate`, with these parameters:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `posture` | string | yes | Registered `AgentPosture` name. |
| `task` | string | yes | Natural-language description of what the sub-agent should accomplish. |
| `context` | object | no | Structured context the sub-agent receives alongside the task. Serialized into the initial user message under `<delegate_context>`. |
| `max_tokens` | int | no | Override the posture's default token budget for this call. |
| `max_tool_calls` | int | no | Override the posture's default tool-call budget. |
| `timeout_seconds` | int | no | Override the posture's default timeout. |

The tool's JSON output is a `delegate.Output`:

```json
{
  "Result": "...",
  "Status": "success",   // success | partial | error | timeout | cancelled | cache_hit
  "Error": "",
  "TokensUsed": 1832,
  "ToolCallsUsed": 4,
  "Elapsed": 8421000000,
  "SubSessionID": "abcd...",
  "PostureName": "analyst",
  "PostureVer": "a1b2c3d4e5f6...",
  "Depth": 1
}
```

The parent agent branches on `Status` to decide whether to retry with a
larger budget, fall back to handling the task itself, or surface the
partial result.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handle invocations of the `delegate` tool. |
| `tool.register` | *(default)* | Build the snapshot of catalog tools so the runtime can filter to the posture's `AllowedTools`. |

### Emits

| Event | When |
|-------|------|
| `tool.register` | Registers the `delegate` tool at boot. |
| `delegate.start` | A sub-session is about to begin. |
| `delegate.complete` | A sub-session has finished. |
| `llm.request` / `before:llm.request` | Per LLM iteration inside the sub-session. |
| `tool.invoke` / `before:tool.invoke` | Per tool call the sub-agent dispatches. |
| `tool.result` / `before:tool.result` | The final response to the parent agent. |

See [delegate events](../../events/reference.md#delegate-events) for
`delegate.start` / `delegate.complete` payload shape.

## Example

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.agent.postures
    - nexus.agent.delegate
    - nexus.tool.web
    - nexus.memory.capped

  nexus.agent.postures:
    scan_dirs:
      - ~/.nexus/postures

  nexus.agent.delegate:
    max_depth: 3
    cache_size: 512

  nexus.agent.react:
    system_prompt: |
      When a task benefits from a different reasoning style or a restricted
      tool surface, call the delegate tool with the appropriate posture
      (analyst, summarizer, auditor, ...).
```

## History isolation

A sub-session runs its own in-process history and contributes nothing to the
parent's conversation. Every `llm.request` the runtime makes is stamped
`_source: "delegate.<sub_session_id>"` and `task_kind: "delegate"`, and the two
consumers filter on different halves of that: `nexus.agent.react` ignores a
response whose `_source` is not its own, and the memory plugins skip
`task_kind: "delegate"` (`plugins/memory/internal/internalflow`).

Both halves are load-bearing. The `task_kind` entry was missing until this
predicate gained it, so a sub-agent's tool-calling response was recorded into
the top-level history and the parent's next request carried two consecutive
assistant turns — which Gemini rejects with `400 INVALID_ARGUMENT` ("function
call turn comes immediately after a user turn"). A memory plugin added outside
this repository must apply the same predicate.

## Causation

Every event emitted from inside a sub-session carries the sub-agent's
`AgentID` (`delegate/<posture>/<sub_session_id>`) and `Depth` on
`Event.Causation`. The replay primitive and observability tooling use
this to attribute work to the right specialist.
