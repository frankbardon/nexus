# Remote AG-UI Agents (`nexus.agent.agui_remote`)

The **consume** side of Nexus's AG-UI integration. Where
[`nexus.io.agui`](../io-agui.md) *serves* a Nexus agent over the AG-UI wire,
`nexus.agent.agui_remote` lets a Nexus agent *call* a remote AG-UI agent — any
service that speaks the AG-UI protocol, including another Nexus instance running
`nexus.io.agui` — as if it were a local delegate.

Each configured remote agent is registered as an LLM-facing tool (default name
`delegate_agui_<name>`). When the parent agent calls it, the plugin builds an
AG-UI `RunAgentInput` from the delegated task, streams the remote run over the
AG-UI wire (HTTP POST + SSE) via the reusable AG-UI client, maps the remote
run's event stream back onto the Nexus bus, and returns the remote run's
terminal outcome as the `tool.result` the parent expects.

From the parent agent's perspective a remote AG-UI call is a single tool call,
exactly like the local [`delegate`](delegate.md) and [`subagent`](subagent.md)
primitives — the transport just happens to be the AG-UI wire instead of an
in-process sub-session.

## Details

| | |
|---|---|
| **ID** | `nexus.agent.agui_remote` |
| **Dependencies** | *(none)* |
| **Requires** | *(none)* |
| **Source** | `plugins/agents/aguiremote/plugin.go` |

## Configuration

The full, authoritative key list lives in the
[configuration reference](../../configuration/reference.md#nexusagentagui_remote).
In brief:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `agents` | list | *(required)* | Non-empty list of remote AG-UI agents to expose. Each entry is a mapping (see below). |
| `timeout_seconds` | int | `120` | Default per-call timeout (seconds). Overridable per agent and per call. |
| `cache_size` | int | `128` | Capacity of the in-process LRU result cache (entries). Zero disables eviction. |
| `cache` | bool | `true` | Set `false` to disable result caching entirely. |

Each `agents[]` entry:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `name` | string | *(required)* | Human-friendly identifier; used to derive the default tool name. |
| `endpoint` | string | *(required)* | Full AG-UI POST endpoint URL (e.g. `https://host/agui`). |
| `tool_name` | string | `delegate_agui_<name>` | Override the LLM-facing tool name. |
| `description` | string | *(auto)* | Override the tool description shown to the LLM. |
| `bearer_token` | string | *(none)* | Static bearer token for the `Authorization` header. Prefer `bearer_token_env`. |
| `bearer_token_env` | string | *(none)* | Name of an environment variable holding the bearer token. Read at `Init`. |
| `timeout_seconds` | int | *(plugin default)* | Per-agent default timeout, overriding the plugin-level value. |

### Authentication

Secrets never live in config files: prefer `bearer_token_env` and point it at an
environment variable. When set, the client sends `Authorization: Bearer <token>`
on the POST that opens the remote run — matching the bearer auth
[`nexus.io.agui`](../io-agui.md) enforces on the serve side. `bearer_token` (an
inline literal) is supported for quick local testing but is only used when
`bearer_token_env` is unset.

## Tool definition

Each remote agent registers one tool. The default name is
`delegate_agui_<name>` (the `name` lowercased with non-alphanumeric runs
collapsed to `_`); override it with `tool_name`.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task` | string | yes | Natural-language description of what the remote agent should accomplish. |
| `context` | object | no | Structured context passed alongside the task. Serialized into the initial user message under an XML `<delegate_context>` boundary. |
| `timeout_seconds` | int | no | Override the remote agent's default timeout for this call. |

## Event & result mapping

While the remote run streams, the plugin translates the incoming AG-UI events
onto the caller's bus so local observers (journal, TUI, observability) see the
remote work as a sub-run:

| Remote AG-UI event | Mapped Nexus event |
|--------------------|--------------------|
| `TextMessageContent` / `TextMessageChunk` | `io.output` (role `assistant`) — streamed text deltas |
| `TextMessageEnd` | `subagent.iteration` — a message boundary |
| `ToolCallStart` / `ToolCallArgs` / `ToolCallEnd` | accumulated into a `subagent.iteration` with the tool call |
| *(run begins)* | `subagent.started` |
| *(run ends)* | `subagent.complete` — carries the terminal result or error |

The terminal outcome — accumulated text deltas, or the `RunFinished` result
payload when the remote streamed no text — becomes the `tool.result` `Output`
returned to the parent agent. The whole `tool.result` passes through the
vetoable `before:tool.result` gate first.

Every mapped event carries the remote sub-run's causation identity
(`AgentID = agui_remote/<name>/<spawn_id>`, `Depth = parent + 1`), so remote
work slots into the causation tree beneath the caller just like a local
delegate.

## Failure behavior

All failure modes surface as a **clean tool error** (`tool.result.Error`) — never
a hang or panic — and are mirrored on `subagent.complete.Error`:

| Condition | Result |
|-----------|--------|
| Remote emits `RunError` | `remote agui run error: <code>: <message>` |
| Transport error / endpoint unreachable | `remote agui transport error: ...` |
| Stream read error mid-run | `remote agui stream error: ...` |
| Non-2xx rejection (e.g. `401` from bearer auth) | `remote agui rejected request: HTTP <code>` |
| Per-call timeout elapses | context-deadline error surfaced as a stream/transport error |
| Remote interrupts awaiting input | `remote agui agent interrupted awaiting input: <prompt>` (a one-shot delegate cannot resolve a remote HITL interrupt) |
| Remote run cancelled | `remote agui run cancelled` |

The parent agent's loop consumes the error `tool.result` and continues normally,
so a flaky or unreachable remote never stalls the caller.

## Caching

Identical calls replay from an in-process LRU keyed by
`endpoint + task + canonicalized context` (mirroring the local `delegate`
cache), so repeated delegations do not re-hit the remote endpoint. A cache hit
still emits a `subagent.started` / `subagent.complete` pair so observers see the
call. Errors are never cached. Set `cache: false` to disable.

## Example

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.agent.agui_remote
    - nexus.memory.capped

  nexus.agent.agui_remote:
    timeout_seconds: 90
    agents:
      - name: researcher
        endpoint: https://research.internal/agui
        bearer_token_env: RESEARCH_AGUI_TOKEN
        description: A specialist research agent reachable over AG-UI.
      - name: legal
        endpoint: https://legal.internal/agui
        tool_name: ask_legal

  nexus.agent.react:
    system_prompt: |
      When a task needs specialist knowledge, delegate it: call
      delegate_agui_researcher for research questions or ask_legal for
      legal review. Pass a clear task and any relevant context.
```

## Loopback (serve ↔ consume)

Because [`nexus.io.agui`](../io-agui.md) speaks the same AG-UI wire, you can
point `nexus.agent.agui_remote` at another Nexus instance's serve endpoint. This
loopback topology is the cheapest faithful end-to-end proof of the consume path
and is exactly what the integration test
(`tests/integration/agui_consume_test.go`) exercises: a caller engine delegates
to a loopback `nexus.io.agui` serve engine and receives the remote agent's
result back as a `tool.result`.

## See also

- [AG-UI Serve (`nexus.io.agui`)](../io-agui.md) — the serve side of the same wire.
- [Delegate](delegate.md) / [Subagent](subagent.md) — the local sub-agent primitives this mirrors.
- [Sub-agent delegation](../../architecture/delegate.md) — the shared delegation model.
