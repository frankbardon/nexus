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
| `tool.invoke` | `ToolCallStart` → `ToolCallArgs` → `ToolCallEnd` | The agent emits `tool.invoke` (not `tool.call`) to run a tool. Arguments are fully resolved on the bus (not streamed), so the three events are emitted together; args are JSON-encoded. Internal sub-calls (non-empty `ParentCallID`) still render but never suspend the run. |
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

## Interrupts: HITL and client-executed tools

AG-UI uses a **terminal-run** model for anything that needs input mid-run: the
run *ends* with an interrupt outcome and the client starts a **continuation run**
carrying `resume[]`. Nexus emulates this as *virtual runs* over one persistent
in-process session — the agent stays parked in-process and a continuation `POST`
unblocks it.

### One Nexus turn spans multiple AG-UI runs

The load-bearing consequence of the terminal-run model is that **a single Nexus
turn can span several AG-UI runs**. When an agent needs input, the current run
ends — but the Nexus session (and the parked agent) stays alive. Each subsequent
resume opens a *new* run over the *same* thread until the turn finally completes:

```text
POST /agui  { threadId: T, runId: R1, messages:[…] }        ← run 1 begins the turn
  … RunStarted … StepStarted … (agent calls ask_user) …
  … StateSnapshot … MessagesSnapshot … RunFinished(interrupt)  ← run 1 ends, agent PARKED

POST /agui  { threadId: T, runId: R2, resume:[…] }          ← run 2 continues the SAME turn
  … RunStarted … (agent unblocks, finishes) … RunFinished     ← turn complete
```

The `threadId` is identical across the runs; each continuation uses a **fresh
`runId`**. No `messages` are needed on a resume — the `resume[]` items are the
payload the server correlates back to its pending interrupt(s). Because the
session is persistent and in-process, a `threadId` must route to the **same**
Nexus instance across its runs (see `threadId` / `runId` semantics below).

Two flows ride the identical suspend/resume machinery:

- **Human-in-the-loop (HITL).** A `hitl.requested` during a run (e.g. the agent
  calling the `ask_user` tool) emits a `StateSnapshot` + `MessagesSnapshot` then
  `RunFinished(interrupt)`; the resume emits `hitl.responded` to unblock the
  waiter.
- **Client-executed (frontend) tools.** Tools the client advertises via
  `RunAgentInput.tools` are surfaced to the agent (the plugin appends them to the
  synchronous `tool.catalog.query` snapshot, scoped to exactly the advertising
  run — they never leak into later runs or shadow a same-named server tool). When
  the agent calls one, its `tool.invoke` streams the `ToolCallStart/Args/End`
  sequence and then the run ends interrupt-style: there is no in-process handler
  to produce a `tool.result`, so the **client** runs the tool and resumes with a
  tool result. The plugin feeds that result back to the parked agent as the
  `tool.result` it was waiting on, and the continuation streams on a fresh run.

A server-side Nexus catalog tool is never intercepted: its own handler runs
inline and produces the `tool.result` that streams as a normal `ToolCallResult`.
Client tools are distinguished purely by **origin** (they came from
`RunAgentInput.tools`).

### The interrupt anchor

`RunFinished(interrupt)` carries an `Interrupt` payload in its `result` field.
The client renders it and echoes its `interruptId` in the resume. It provides:

| Field | Meaning |
|---|---|
| `interruptId` | The anchor the client echoes back in `resume[].interruptId`. Distinct from any internal request id. |
| `prompt` | The rendered question/approval text (HITL) or a client-tool hint. |
| `mode` | `free_text`, `choices`, or `both` — controls the response affordance. |
| `choices` / `defaultChoiceId` | The options (and deadline default) for a `choices`/`both` interrupt. |

The interrupt kind (HITL vs client tool) is also mirrored in the `StateSnapshot`
under an `interrupt` (HITL) or `toolCall` (client tool) anchor, so a client that
restores from state alone — rather than replaying `MessagesSnapshot` — still has
everything it needs to resume.

### The `resume[]` wire shape

Each `resume[]` item names an `interruptId`, a `status`, and an optional
`payload`. The payload fields depend on the interrupt kind:

| `status` | Interrupt kind | `payload` fields | Effect |
|---|---|---|---|
| `resolved` | HITL | `choiceId`, `freeText`, `editedPayload` | Answers the prompt. A `choices`-only interrupt drops stray `freeText`. All fields optional; an empty payload accepts the default. |
| `resolved` | client tool | `output`, `error` | Becomes the parked agent's `tool.result`. Empty resolves the call with empty output (the agent still advances). |
| `cancelled` | either | *(none)* | Abandons the interrupt: a HITL waiter unblocks as cancelled; a client-tool call resolves with an error `tool.result` so the agent's loop still advances. |

```jsonc
// HITL resume: pick a choice.
{ "threadId":"T", "runId":"R2",
  "resume":[ { "interruptId":"int-…", "status":"resolved",
               "payload": { "choiceId":"staging" } } ] }

// Client-tool resume: return the tool's output.
{ "threadId":"T", "runId":"R2",
  "resume":[ { "interruptId":"int-…", "status":"resolved",
               "payload": { "output":"sunny, 24C" } } ] }

// Cancel either kind.
{ "threadId":"T", "runId":"R2",
  "resume":[ { "interruptId":"int-…", "status":"cancelled" } ] }
```

As AG-UI requires, **all** open interrupts on a thread must be addressed in one
resume request: a resume that references an unknown/expired interrupt, addresses
one twice, or leaves an open interrupt unaddressed is rejected with a clean
terminal `RunError` stream and leaves the parked agent untouched for a corrected
retry.

The reusable pure-Go conformance client (`pkg/agui/aguiclient`) provides
constructors for these payloads — `ResumeInput`, `ResolveChoice`, `ResolveText`,
`ResolveToolResult`, and `Cancel` — plus `Result.Interrupt()` to extract the
anchor from a `RunFinished(interrupt)`. The end-to-end interrupt/resume and
client-tool round-trips are exercised in
`tests/integration/agui_hitl_test.go`.

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
| `emit_state` | bool | `false` | Opt-in AG-UI shared-state emission: mirror the scene store as a shared-state document and emit `StateSnapshot`/`StateDelta` on the run stream. See [Shared state](#shared-state) below. |

### Shared state

With `emit_state: true`, the transport mirrors the session's scene store
(`nexus.scene`) as the AG-UI **shared state** document so a frontend can render
and track agent state. The mapping is:

- The scene store emits `scene.created` / `scene.patched` / `scene.deleted` on
  the bus, each carrying the scene's full post-mutation content. The transport
  tracks these into a document keyed by `scene_id` (value = the scene's current
  content). It never calls the scene plugin directly — the bus events are the
  sole input.
- On run start, a `StateSnapshot` of the current document is emitted right after
  `RunStarted`.
- Each scene mutation during the run emits a `StateDelta` whose `delta` is an
  **RFC 6902 JSON Patch** from the previous document to the new one. The
  `StateSnapshot` always precedes any `StateDelta` on the stream, and applying the
  deltas in order to the snapshot reconstructs the state (verified end to end by
  the `TestAGUIState_*` integration tests as well as the `pkg/agui` unit tests).
  This aligns AG-UI's `StateDelta` with the scene store's patch model while
  normalizing the scene store's shallow-merge semantics into a valid JSON Patch
  computed from full content.

The document is session-scoped and persists across runs on the listener, so a
later run's snapshot reflects scenes created by an earlier run.

#### Inbound state (client → agent)

A client may send a shared-state document on `RunAgentInput.state` to seed or
edit state the agent then observes. The document uses the **same scene-keyed
shape** the transport emits outbound: a JSON object whose keys are `scene_id`s
and whose values are that scene's content.

- Inbound state is applied at run start (and on a resume/continuation run)
  **before** the initial `StateSnapshot` is emitted, so the snapshot reflects the
  client's view and the agent's first turn observes it.
- To make a client write real (not just a mirror update), each `scene_id →
  content` entry is pushed into the scene store via a bus-emitted `scene_create`
  `tool.invoke` carrying an explicit `scene_id`. The scene plugin creates the
  scene under that id, or **shallow-merges** the content as a patch when the scene
  already exists (client edits a scene the agent created preserve keys the client
  did not send). The agent then reads the seeded state through the normal
  `scene_get` / `scene_list` tools. No direct plugin-to-plugin call is made — the
  bus is the only channel.
- A non-object state document (or otherwise malformed) is logged and skipped; it
  never fails the run. Inbound state is a no-op when `emit_state` is off.

**Conflict / ordering semantics — client-state-seeds-then-agent-wins.** The
client seed is fully applied before the run's `io.input` is emitted, so the agent
always starts from the seeded state. For the rest of the run, agent-side scene
mutations are **last-writer** over the same `scene_id`: a later `scene_patch`
overwrites the client's value per the scene store's shallow-merge semantics, and
that change flows back out as a `StateDelta` (completing the round-trip). The
transport's `stateMu` and the scene store's own lock serialize concurrent client
and agent mutations, so ordering is deterministic (client seed first, then agent
writes in bus order) and no half-applied document is ever observed.

Because the mirror is seeded to the same value the scene store echoes back, the
seed itself produces **no** `StateDelta` — only genuine agent mutations do. The
`TestAGUIState_InboundSeedObserved` and `TestAGUIState_ConflictAgentWins`
integration tests exercise this round-trip: the client seed appears in the
initial `StateSnapshot` and is read back through `scene_get`, and a subsequent
agent `scene_patch` on the same `scene_id` wins on the overlapping key (with the
client's untouched keys preserved by shallow-merge) and surfaces as exactly one
`StateDelta`.

The `scene_create` tool accepts an optional `scene_id` argument to support this
seeding; when omitted the store assigns an id as before, so existing agent usage
is unchanged.

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
