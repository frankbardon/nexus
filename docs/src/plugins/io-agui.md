# AG-UI Serve Transport (`nexus.io.agui`)

`nexus.io.agui` exposes Nexus over the [AG-UI protocol](https://docs.ag-ui.com)
("Agent-User Interaction"), the open, event-based standard for connecting
streaming agents to user-facing applications. It stands up an HTTP listener so
that **any** standards-compliant AG-UI client — CopilotKit/React, the AG-UI
terminal client, or a framework integration (LangGraph, CrewAI, Pydantic AI,
Google ADK, Mastra, …) — can drive a Nexus agent with no Nexus-specific client
code.

This is a **serve** transport: it accepts AG-UI requests and streams AG-UI
responses. It is additive and external-facing — it does not replace the
`nexus.io.browser` / `nexus.io.wails` Envelope wire that backs the built-in web
and desktop UIs. See [I/O Transport Plugins](./io/index.md) for the transport
family, and `.claude/docs/io-transport.md` for why AG-UI intentionally sits
outside the browser↔wails parity rule.

## Details

| | |
|---|---|
| **ID** | `nexus.io.agui` |
| **Dependencies** | None |
| **Wire format** | AG-UI over HTTP + SSE (defined by `pkg/agui`, not the `pkg/ui` Envelope) |
| **Endpoint** | `POST /agui` (plus `OPTIONS /agui` for CORS preflight) |
| **Spec version** | `v1 (docs.ag-ui.com, 2026-07-10)` — pinned in `pkg/agui` as `agui.SpecVersion` |

The codec is hand-rolled in `pkg/agui` (no third-party SDK), matching the
raw-`net/http`, minimal-dependency house style. Because the spec is tracked
manually, the targeted version is pinned in one place (`agui.SpecVersion`) and
quoted above.

## How it works

A client `POST`s a `RunAgentInput` JSON body to `/agui` and receives a
`text/event-stream` (SSE) response carrying one **run**: a well-formed AG-UI
lifecycle from `RunStarted` to `RunFinished` (or `RunError`). The stream flushes
incrementally as bus events arrive — nothing is buffered until the end.

**Inbound (client → bus).** The request's `messages` are mapped to a Nexus
`io.input`:

- The trailing `user` message becomes the live turn content.
- Any earlier messages ride as `PreloadMessages`, so a resumed thread keeps its
  prior context.
- `threadId` is recorded as the Nexus **session** id; `runId` identifies the
  **turn**.

`io.input` is published vetoably (`before:io.input` first); a veto ends the run
with `RunError`.

**Outbound (bus → client).** The plugin subscribes to the same engine bus
events as the browser transport and translates each into canonical AG-UI SSE.
Bus handlers only enqueue translated events onto the active run's channel; a
single HTTP handler goroutine is the sole SSE writer, so the stream is
race-free.

## Event mapping

Nexus bus events map near-1:1 onto the canonical AG-UI event taxonomy:

| Nexus bus event | AG-UI event(s) | Notes |
|---|---|---|
| *(run accepted)* | `RunStarted` | Emitted eagerly on accept so even an agent-less run is well-formed. `threadId` / `runId` echoed. |
| `agent.turn.start` | `StepStarted` | Each turn/iteration opens a step; the step name derives from `TurnID`. |
| `agent.turn.end` | `StepFinished`, then `RunFinished` | A top-level turn end closes the open step **and** terminates the run/stream. |
| `llm.stream.chunk` | `TextMessageStart` → `TextMessageContent` | `TextMessageStart` (role `assistant`) is emitted lazily on the first non-empty delta; subsequent deltas append content. |
| `llm.stream.end` | `TextMessageEnd` | Closes the open streamed text message. |
| `io.output` | `TextMessageStart` → `TextMessageContent` → `TextMessageEnd` | Self-contained triple. Skipped when the same content was already streamed via `llm.stream.chunk`; still rendered when a non-streaming provider (mock / batch) flags output `streamed` but emitted no chunks, so text is never dropped. |
| `tool.call` | `ToolCallStart` → `ToolCallArgs` → `ToolCallEnd` | Arguments are fully resolved on the bus (not streamed), so the three events are emitted together; args are JSON-encoded. |
| `tool.result` | `ToolCallResult` | Correlated to the call by `toolCallId`; `Error` content is surfaced in place of `Output` when present. |
| `thinking.step` | `ReasoningStart` → `ReasoningMessageContent` | `ReasoningStart` opens lazily on the first step; `ReasoningEnd` is emitted at turn end. |
| *(failure / disconnect / veto / concurrent run)* | `RunError` | Terminal; ends the stream. |

Both `RunFinished` and `RunError` are terminal — the SSE stream ends on either.

### Non-canonical events: the `Custom` superset

Nexus emits rich events that have **no** canonical AG-UI equivalent. Rather than
drop them, they ride the AG-UI `Custom` event as a documented **superset**: the
`Custom.name` is the Nexus bus event type and `Custom.value` is the JSON-encoded
payload. Stock AG-UI clients that only understand canonical events can safely
ignore `Custom` without losing the run's canonical lifecycle; Nexus-aware
clients can opt in.

The bridged bus events are:

- `workflow.progress`
- `subagent.started`
- `subagent.iteration`
- `subagent.complete`
- `code.exec.stdout`

> AG-UI also defines a `Raw` event for passthrough of an upstream provider's
> native event shape. Nexus-specific events use `Custom` (name + JSON value)
> consistently; `Raw` is available in `pkg/agui` for future passthrough needs.

## `threadId` / `runId` semantics

- **`threadId` ↔ Nexus session.** The `threadId` is recorded as the session id
  on the inbound `io.input`. Because the serving session is persistent and lives
  in-process, a `threadId` must route to the **same** Nexus instance across
  runs — the terminal-run/resume model is emulated as *virtual runs* over one
  live session, not by reconnecting to a stateless backend.
- **`runId` ↔ Nexus turn.** Each `POST` is one run == one turn. Message ids in
  the outbound stream are derived deterministically from the `runId` so a client
  can correlate streamed text, tool calls, and results within the run.

## Concurrency and scope

One in-flight run per listener (single engine/session per listener, mirroring
`nexus.io.browser`). A second `POST` while a run is active receives a terminal
`RunStarted` + `RunError` stream rather than interleaving into the live run. On
client disconnect or engine shutdown, the active run fails with `RunError` and
its handler returns promptly, releasing the slot.

## Exposure, auth, and CORS

Safe by default: the listener **binds loopback** (`127.0.0.1:8090`) so the
endpoint is never network-exposed without an explicit operator opt-in.

- **Bearer auth** is enforced only when a non-empty token is resolved. An inline
  `bearer_token` takes precedence; otherwise `bearer_token_env` names an
  environment variable to read it from. When set, every request must carry
  `Authorization: Bearer <token>`.
- **CORS** is off by default (same-origin only). `cors_origins` accepts a YAML
  list (or comma-separated string); a single `*` echoes any request `Origin`,
  while an explicit list echoes only matching origins. `OPTIONS /agui` answers
  preflight for browser AG-UI clients.

## Configuration

The canonical, always-current key list lives in the
[Configuration Reference](../configuration/reference.md#nexusioagui). The keys
are summarized here for convenience:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `bind` | string | `127.0.0.1:8090` | `host:port` the HTTP listener binds to. Loopback by default. |
| `bearer_token` | string | *(empty)* | Inline bearer token. Takes precedence over `bearer_token_env`. |
| `bearer_token_env` | string | *(empty)* | Env var name to read the bearer token from (used only when `bearer_token` is empty). |
| `cors_origins` | list&lt;string&gt; | *(empty)* | Allowed CORS origins. `*` echoes any Origin; a list echoes only matches; empty means same-origin only. Also accepts a comma-separated string. |

### Example configuration

```yaml
plugins:
  nexus.io.agui:
    bind: "127.0.0.1:8090"
    bearer_token_env: "AGUI_BEARER_TOKEN"
    cors_origins:
      - "https://app.example.com"
```

## See also

- [Configuration Reference — `nexus.io.agui`](../configuration/reference.md#nexusioagui) — canonical config keys.
- [I/O Transport Plugins](./io/index.md) — the transport family.
- [Browser UI](./io/browser.md) — the session-scoped Envelope transport AG-UI mirrors for scope/exposure.
