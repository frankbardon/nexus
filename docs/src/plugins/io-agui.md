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
lifecycle from `RUN_STARTED` to `RUN_FINISHED` (or `RUN_ERROR`). The stream flushes
incrementally as bus events arrive — nothing is buffered until the end.

**Inbound (client → bus).** The request's `messages` are mapped to a Nexus
`io.input`:

- The trailing `user` message becomes the live turn content.
- Any earlier messages ride as `PreloadMessages`, so a resumed thread keeps its
  prior context.
- `threadId` is recorded as the Nexus **session** id; `runId` identifies the
  **turn**.

`io.input` is published vetoably (`before:io.input` first); a veto ends the run
with `RUN_ERROR`.

**Outbound (bus → client).** The plugin subscribes to the same engine bus
events as the browser transport and translates each into canonical AG-UI SSE.
Bus handlers only enqueue translated events onto the active run's channel; a
single HTTP handler goroutine is the sole SSE writer, so the stream is
race-free.

## Event mapping

Nexus bus events map near-1:1 onto the canonical AG-UI event taxonomy. The
AG-UI wire `type` discriminator is UPPER_SNAKE_CASE (per the AG-UI protocol);
the values below are the exact strings emitted on the SSE stream:

| Nexus bus event | AG-UI event(s) | Notes |
|---|---|---|
| *(run accepted)* | `RUN_STARTED` | Emitted eagerly on accept so even an agent-less run is well-formed. `threadId` / `runId` echoed. |
| `agent.turn.start` | `STEP_STARTED` | Each turn/iteration opens a step; the step name derives from `TurnID`. |
| `agent.turn.end` | `STEP_FINISHED`, then `RUN_FINISHED` | A top-level turn end closes the open step **and** terminates the run/stream — the run that *carries* that turn, never whichever run holds the slot. See [A turn's events belong to its own run](#a-turns-events-belong-to-its-own-run-not-to-the-slot). |
| `llm.stream.chunk` | `TEXT_MESSAGE_START` → `TEXT_MESSAGE_CONTENT` | `TEXT_MESSAGE_START` (role `assistant`) is emitted lazily on the first non-empty delta; subsequent deltas append content. |
| `llm.stream.end` | `TEXT_MESSAGE_END` | Closes the open streamed text message. |
| `io.output` | `TEXT_MESSAGE_START` → `TEXT_MESSAGE_CONTENT` → `TEXT_MESSAGE_END` | Self-contained triple. Skipped when the same content was already streamed via `llm.stream.chunk`; still rendered when a non-streaming provider (mock / batch) flags output `streamed` but emitted no chunks, so text is never dropped. |
| `tool.invoke` | `TOOL_CALL_START` → `TOOL_CALL_ARGS` → `TOOL_CALL_END` | The agent emits `tool.invoke` (not `tool.call`) to run a tool. Arguments are fully resolved on the bus (not streamed), so the three events are emitted together; args are JSON-encoded. Internal sub-calls (non-empty `ParentCallID`) still render but never suspend the run. |
| `tool.result` | `TOOL_CALL_RESULT` | Correlated to the call by `toolCallId`; `Error` content is surfaced in place of `Output` when present. |
| `thinking.step` | `REASONING_START` → `REASONING_MESSAGE_CONTENT` | `REASONING_START` opens lazily on the first step; `REASONING_END` is emitted at turn end. |
| *(failure / disconnect / veto / concurrent run)* | `RUN_ERROR` | Terminal; ends the stream. |

Both `RUN_FINISHED` and `RUN_ERROR` are terminal — the SSE stream ends on either.

### Non-canonical events: the `CUSTOM` superset

Nexus emits rich events that have **no** canonical AG-UI equivalent. Rather than
drop them, they ride the AG-UI `CUSTOM` event as a documented **superset**: the
`Custom.name` is the Nexus bus event type and `Custom.value` is the JSON-encoded
payload. Stock AG-UI clients that only understand canonical events can safely
ignore `CUSTOM` without losing the run's canonical lifecycle; Nexus-aware
clients can opt in.

The bridged bus events are:

- `workflow.progress`
- `subagent.started`
- `subagent.iteration`
- `subagent.complete`
- `code.exec.stdout`

> AG-UI also defines a `RAW` event for passthrough of an upstream provider's
> native event shape. Nexus-specific events use `CUSTOM` (name + JSON value)
> consistently; `RAW` is available in `pkg/agui` for future passthrough needs.

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
  … RUN_STARTED … STEP_STARTED … (agent calls ask_user) …
  … STATE_SNAPSHOT … MESSAGES_SNAPSHOT … RUN_FINISHED(interrupt)  ← run 1 ends, agent PARKED

POST /agui  { threadId: T, runId: R2, resume:[…] }          ← run 2 continues the SAME turn
  … RUN_STARTED … (agent unblocks, finishes) … RUN_FINISHED     ← turn complete
```

The `threadId` is identical across the runs; each continuation uses a **fresh
`runId`**. No `messages` are needed on a resume — the `resume[]` items are the
payload the server correlates back to its pending interrupt(s). Because the
session is persistent and in-process, a `threadId` must route to the **same**
Nexus instance across its runs (see `threadId` / `runId` semantics below).

Two flows ride the identical suspend/resume machinery:

- **Human-in-the-loop (HITL).** A `hitl.requested` during a run (e.g. the agent
  calling the `ask_user` tool) emits a `STATE_SNAPSHOT` + `MESSAGES_SNAPSHOT` then
  `RUN_FINISHED(interrupt)`; the resume emits `hitl.responded` to unblock the
  waiter.
- **Client-executed (frontend) tools.** Tools the client advertises via
  `RunAgentInput.tools` are surfaced to the agent (the plugin appends them to the
  synchronous `tool.catalog.query` snapshot, scoped to exactly the advertising
  run — they never leak into later runs or shadow a same-named server tool). When
  the agent calls one, its `tool.invoke` streams the `TOOL_CALL_START/ARGS/END`
  sequence and then the run ends interrupt-style: there is no in-process handler
  to produce a `tool.result`, so the **client** runs the tool and resumes with a
  tool result. The plugin feeds that result back to the parked agent as the
  `tool.result` it was waiting on, and the continuation streams on a fresh run.

A server-side Nexus catalog tool is never intercepted: its own handler runs
inline and produces the `tool.result` that streams as a normal `TOOL_CALL_RESULT`.
Client tools are distinguished purely by **origin** (they came from
`RunAgentInput.tools`).

### The interrupt anchor

`RUN_FINISHED(interrupt)` carries an `Interrupt` payload in its `result` field.
The client renders it and echoes its `interruptId` in the resume. It provides:

| Field | Meaning |
|---|---|
| `interruptId` | The anchor the client echoes back in `resume[].interruptId`. Distinct from any internal request id. |
| `prompt` | The rendered question/approval text (HITL) or a client-tool hint. |
| `mode` | `free_text`, `choices`, or `both` — controls the response affordance. |
| `choices` / `defaultChoiceId` | The options (and deadline default) for a `choices`/`both` interrupt. |

The interrupt kind (HITL vs client tool) is also mirrored in the `STATE_SNAPSHOT`
under an `interrupt` (HITL) or `toolCall` (client tool) anchor, so a client that
restores from state alone — rather than replaying `MESSAGES_SNAPSHOT` — still has
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
terminal `RUN_ERROR` stream and leaves the parked agent untouched for a corrected
retry.

The reusable pure-Go conformance client (`pkg/agui/aguiclient`) provides
constructors for these payloads — `ResumeInput`, `ResolveChoice`, `ResolveText`,
`ResolveToolResult`, and `Cancel` — plus `Result.Interrupt()` to extract the
anchor from a `RUN_FINISHED(interrupt)`. The end-to-end interrupt/resume and
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
`RUN_STARTED` + `RUN_ERROR` stream rather than interleaving into the live run. On
client disconnect or engine shutdown, the active run fails with `RUN_ERROR` and
its handler returns promptly, releasing the slot — and a disconnect that lands
while a turn is still running also stops that turn, below.

### A dead stream cancels the turn behind it

Releasing the slot is not enough on a disconnect: failing the run only closes a
channel on this side of the bus, and the agent never hears about it. So a run
whose SSE stream dies **while a turn is still running** also asks the
`control.cancel` capability to stop that turn — the plugin emits
`cancel.request` (declared in its `Emissions()`) naming the turn the vanished
client orphaned. Both stream-death exits are wired: the request-context watcher
that fires when the client goes away, and a failed SSE write on a broken
socket. Without it the orphaned turn runs to completion for nobody, iterating,
calling tools and spending the session's token budget with no reader on the
other end.

The cancel is **resumable, not destructive**. `nexus.control.cancel` owns turn
cancellation for every transport (the TUI, the browser and `nexus.io.a2a` all
enter the same way) and answers with `cancel.active`; the agent loop turns that
into `cancel.complete` with `Resumable: true` and then its final
`agent.turn.end` — which is also what releases the identity the run bound, so a
cancelled turn closes its
[identity lifetime](#identity-lifetime-the-turn-not-the-request) through the
ordinary path with no second mechanism.

A **deliberate** suspension is never cancelled. A HITL park and a
client-executed-tool suspend both end the run with the agent alive and parked
on purpose, and two independent guards keep them out of this path: only the
caller whose own failure performed the run's one-shot close reaches it (a
completed, parked or retracted run was closed by something else first), and a
park marks the run suspended *before* that close, so a disconnect racing a park
still reads as a park. A run that never saw an `agent.turn.start` — an input
vetoed before any agent ran, say — has no turn to name and emits nothing. Nor
is engine teardown stream death: `Shutdown` fails the in-flight run without
calling in, since a cancel emitted into a stopping bus would reach an agent
that is going away regardless.

**There is no configuration key for any of this — it is always on.** Cancelling
an orphaned turn is a correctness property of the transport, not a policy
choice; the work is suspended and resumable, so nothing is lost by it.

### A turn's events belong to its own run, not to the slot

The slot and the turn have different lifetimes, and a cancelled turn does not
go quiet the instant the slot is freed: the disconnect frees the slot and only
*then* asks the agent to stop, so the cancellation's own `io.output` and
`agent.turn.end` arrive on the bus after the next `POST` has already taken the
slot. Those events are scoped by `TurnID`, so they reach the run that carries
that turn and no other. Without that scoping the successor's stream was
terminated by a turn it never ran — `RUN_FINISHED` with no `RUN_STARTED`, an
HTTP 200 on a stream that never starts — and the departed turn's cancellation
notice was rendered as the answer to the next question.

Two rules, because the two questions differ:

- **Terminating.** An `agent.turn.end` finishes the run only when that run
  bound that turn. A different id is another turn's; an id arriving at a run
  that has bound none is too, since a run is published only after its
  `RUN_STARTED` is queued and a continuation adopts its parked turn before
  publication, so a run cannot miss its own turn start. An event naming **no**
  turn is uncorrelatable and is taken — leaving a client on a stream that never
  terminates is worse. This is the rule `nexus.io.a2a` already states for its
  own task lifetime.
- **Delivering.** `io.output`, `tool.invoke` and `tool.result` are refused only
  when they can be *proved* foreign: a named turn at a run that bound none, or
  the turn the previous run was carrying. Their emitters are an open set — most
  gates emit `io.output` with no `TurnID` at all, and `nexus.agent.aguiremote` /
  `nexus.agent.a2aremote` republish a delegated remote's narration under a
  synthetic sub-turn id on purpose — so a strict match would delete legitimate
  events. `tool.invoke` matters most here: a client-executed tool call from a
  departed turn would not merely render onto the successor's stream, it would
  suspend it.

The two **HITL** handlers take the delivering rule, because a park is not a
termination:

- `hitl.requested` carries a real agent-loop turn wherever it carries one at
  all — `nexus.control.hitl` copies the asking tool call's own `TurnID`
  verbatim, and `nexus.agent.a2aremote` and the ICM workflow both set the
  spawning loop's turn — so it is comparable to the turn the run bound. A
  question from a turn this run does not carry would **park** it: an interrupt
  outcome for a question the client never asked, the run's own turn left
  running with no stream, and the `interruptId` → request mapping recorded
  against the wrong thread and run, so the resume `POST` that follows answers
  the departed turn's question. Two of the five emitters set **no** `TurnID` at
  all (`nexus.gate.approval_policy` carries the turn in its `ActionRef`
  metadata; `plugins/memory`'s approval helper names none), which is why the
  permissive rule is the right one here: a strict match would drop a question
  the agent is blocked on and park that turn forever.
- `hitl.cancel` carries **no turn id at all** — `events.HITLCancel` is a
  `RequestID` and a `Reason` — so its discriminator is *recovered* rather than
  read: every request this plugin rendered was recorded in `p.pending` with the
  turn it suspended, so a retraction naming one of those is scoped exactly as a
  turn-carrying event is. That matters because `cancelTerminal` closes the SSE
  of whatever holds the slot. The mapping is dropped **unconditionally** — the
  request is retracted whoever holds the slot — while only the *termination* is
  gated. A retraction this plugin cannot correlate (no mapping, or a mapping
  whose request named no turn) terminates the current run exactly as it always
  did, on the same asymmetry: leaving a client on a stream that never ends is
  worse than ending one early.

`llm.stream.*`, `thinking.step` and `llm.response` are **not** scoped this way
and cannot be: their `TurnID` is the *provider's* per-call identifier (Gemini
synthesises one per request, Anthropic uses the API response id), not the agent
loop's turn, so no equality test against the run's turn would ever hold.

## Exposure, auth, and CORS

Safe by default: the listener **binds loopback** (`127.0.0.1:8090`) so the
endpoint is never network-exposed without an explicit operator opt-in.

- **Bearer auth** is enforced only when a non-empty token is resolved. An inline
  `bearer_token` takes precedence; otherwise `bearer_token_env` names an
  environment variable to read it from. When set, every request must carry
  `Authorization: Bearer <token>`.
- **Identity providers** — an optional [`auth:` block](#authentication-auth)
  configures the full `pkg/nexusauth` validator chain (`static`, `jwks`,
  `introspect`, `proxy_headers`) instead of a single shared token.
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
| `bearer_token` | string | *(empty)* | Inline bearer token. Takes precedence over `bearer_token_env`. Mutually exclusive with `auth`. |
| `bearer_token_env` | string | *(empty)* | Env var name to read the bearer token from (used only when `bearer_token` is empty). Mutually exclusive with `auth`. |
| `auth` | map | *(absent)* | Validator-chain block, parsed by the same `pkg/nexusauth` parser the session broker uses. See [Authentication](#authentication-auth) below. |
| `cors_origins` | string or list&lt;string&gt; | *(empty)* | Allowed CORS origins. `*` echoes any Origin; a list echoes only matches; empty means same-origin only. Accepts a YAML list or a single comma-separated string. |
| `emit_state` | bool | `false` | Opt-in AG-UI shared-state emission: mirror the scene store as a shared-state document and emit `STATE_SNAPSHOT`/`STATE_DELTA` on the run stream. See [Shared state](#shared-state) below. |

The block is validated against `plugins/io/agui/schema.json` **before `Init`
runs**, with `additionalProperties: false` at every level including inside
`auth:` and inside each `validators[]` entry. An unknown key aborts the boot and
names the offender — a misspelled auth key that was silently ignored would mean
an unauthenticated listener with no warning.

### Authentication (`auth:`)

The transport authenticates through the shared identity layer, `pkg/nexusauth` —
the same validator chain `cmd/nexus-broker` uses. Pointing both hosts at one
parser is what makes OIDC available here without any AG-UI-specific
identity code.

Two spellings are accepted, and they are **mutually exclusive**:

- **`bearer_token` / `bearer_token_env`** — one shared secret. Unchanged, **not
  deprecated**, and still the right amount of configuration for a loopback
  listener fronting one developer's UI. It is desugared into a one-entry
  `static` validator; the only visible difference is that the token comparison is
  now constant-time.
- **`auth:`** — the full validator chain: `static` (a token table), `jwks` (OIDC
  JWTs verified against the issuer's published keys), `introspect` (opaque tokens
  verified via RFC 7662), and `proxy_headers` (an identity a fronting
  authenticating proxy already established). Validators are tried in the order
  listed and the first one that accepts wins, so cheap validators belong first.

Setting **both** fails the boot with an error naming both keys. That is a
deliberate choice over a precedence rule: two sources for one security decision
means one of them is stale. (`bearer_token` together with `bearer_token_env`
remains legal, with its original precedence — inline first, then the environment
variable.)

Setting **neither** disables authentication and admits every request, exactly as
before. That is only safe because the listener binds loopback; if you change
`bind`, configure auth in the same change.

```yaml
plugins:
  nexus.io.agui:
    bind: "0.0.0.0:8090"
    auth:
      validators:
        - type: jwks
          issuer: "https://id.example.com/"
          jwks_url: "https://id.example.com/.well-known/jwks.json"
          audience: "nexus-agui"
          principal_claim: sub
          scopes_claim: scope
```

Every validator key, default and validation rule is documented once in the
[Configuration Reference](../configuration/reference.md#authentication-auth).
The one difference from the broker is that `admin_scope` is broker-only and is
rejected here as an unknown key; unknown keys are rejected at every level in both
hosts.

**What is gated:** `POST /agui`, and nothing else. `OPTIONS /agui` stays
unauthenticated — a browser never attaches `Authorization` to a CORS preflight.
CORS headers are written **before** the auth check, so a browser can read a `401`
rather than seeing an opaque network error.

**Refusals:** `401` with a `WWW-Authenticate: Bearer realm="nexus-agui"`
challenge (plus `error="invalid_token"` when a credential was presented and
rejected), `403` with `error="insufficient_scope"`, and `503` with a
`Retry-After` when a validator could not reach a verdict — an identity-provider
outage must not read to a client as "re-authenticate". See the
[status mapping table](../configuration/reference.md#authentication-auth-on-nexusioagui).

**Principal:** the resolved identity is recorded on the `agui run started` log
record as `principal_id` (empty when auth is disabled). It is also bound into
the session's tag store: `startRun`/`resumeRun` write it as the reserved
`_principal_id` session label before the run's `io.input` (or, on resume,
`hitl.responded`) is emitted, and released by the `agent.turn.end` of the turn
it was bound for — not by the HTTP request returning (see
[Identity lifetime](#identity-lifetime-the-turn-not-the-request) below). Every
run/resume re-binds fresh from that request's own resolved principal — a
resumed thread under a different principal gets a new bind, never a stale one,
and a run that resolved no principal clears the label rather than inheriting
the previous run's. This is a pure
observability seam: one listener still serves a single session and one run at
a time, so nothing in this transport itself keys behaviour on the bound
identity. An external consumer (e.g. an embedder's own authorization layer)
subscribes to `session.tag.set` / `session.tag.deleted` to observe the bind
and the clear. See [Session Tags](../architecture/session-tags.md)
for the tag store itself.

**Business context:** each `RunAgentInput.context` item (`description` /
`value`) is written directly as a general-namespace session tag
(`description` -> key, `value` -> value) at the same points, with no veto —
this is already-authenticated, already-decoded input. A client cannot use
this to write the reserved `_principal_id` key: the reserved prefix (`_`) is
enforced at the tag store regardless of caller, so a `context` item whose
`description` starts with `_` is rejected rather than silently overwriting
the bound identity.

#### Identity lifetime: the turn, not the request

`_principal_id` and the `_header.*` labels are bound **per run** and cleared by
the `agent.turn.end` of **the turn they were bound for**. Neither the HTTP
request nor the AG-UI run is the boundary, because one Nexus turn can span
several runs (see [Interrupts](#interrupts-hitl-and-client-executed-tools)):

- **Bound** in `startRun` / `resumeRun`, before that run's `io.input` (or, on a
  continuation, its `hitl.responded` / `tool.result`), so a subscriber of
  `session.tag.set` never sees the turn's first downstream event ahead of the
  identity it belongs to.
- **Correlated** with the first `agent.turn.start` observed under the run that
  bound them. A continuation run adopts the parked turn instead, because a
  resumed turn emits no fresh `agent.turn.start` — the agent never left it.
- **Cleared** when `agent.turn.end` arrives for that same turn. `react`,
  `planexec` and `orchestrator` each emit it exactly once per top-level turn on
  every exit — normal completion, cancel, plan-not-approved — so a turn that
  ends at all ends its identity with it.
- **Held** across anything that ends the run while the work continues: a HITL
  park, a client-executed-tool suspend, a client disconnect, or the handler
  simply returning early. De-authenticating a turn that is still running is
  strictly worse than holding the bind — an embedder whose tools read identity
  fresh off the session and fail closed turns one dropped label into a refusal
  loop that burns the turn's whole budget.
- **`Shutdown` is the only backstop**, and it clears unconditionally, for a
  turn that never emits `agent.turn.end` at all (a wedged provider, a killed
  sub-process). There is deliberately no TTL, lease or timer on this path: a
  deadline would re-create the original bug for any legitimately long turn.

Correlating on the *turn*, not just on the binding run, is what makes the clear
precise. On the happy path the turn ends while its own run is still draining
SSE, so "skip the clear whenever a run is active" would never clear anything;
and after a late-turn race the newer run is both the active run and the
identity owner, so a run-pointer comparison cannot tell an abandoned older
turn's end from its own. The `TurnID` can, so an older turn ending never wipes
a newer run's bind.

This is `nexus.io.agui`'s rule, not a shared one. `nexus.io.a2a` clears the
request headers from its own terminal sequence and binds no principal at all;
`nexus.io.broker` binds identity on every forwarded `input` frame and has no
separate clear. See
[Request Headers — Lifetime](../guides/request-headers.md#lifetime) for the
per-transport table.

### Shared state

With `emit_state: true`, the transport mirrors the session's scene store
(`nexus.scene`) as the AG-UI **shared state** document so a frontend can render
and track agent state. The mapping is:

- The scene store emits `scene.created` / `scene.patched` / `scene.deleted` on
  the bus, each carrying the scene's full post-mutation content. The transport
  tracks these into a document keyed by `scene_id` (value = the scene's current
  content). It never calls the scene plugin directly — the bus events are the
  sole input.
- On run start, a `STATE_SNAPSHOT` of the current document is emitted right after
  `RUN_STARTED`.
- Each scene mutation during the run emits a `STATE_DELTA` whose `delta` is an
  **RFC 6902 JSON Patch** from the previous document to the new one. The
  `STATE_SNAPSHOT` always precedes any `STATE_DELTA` on the stream, and applying the
  deltas in order to the snapshot reconstructs the state (verified end to end by
  the `TestAGUIState_*` integration tests as well as the `pkg/agui` unit tests).
  This aligns AG-UI's `STATE_DELTA` with the scene store's patch model while
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
  **before** the initial `STATE_SNAPSHOT` is emitted, so the snapshot reflects the
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
that change flows back out as a `STATE_DELTA` (completing the round-trip). The
transport's `stateMu` and the scene store's own lock serialize concurrent client
and agent mutations, so ordering is deterministic (client seed first, then agent
writes in bus order) and no half-applied document is ever observed.

Because the mirror is seeded to the same value the scene store echoes back, the
seed itself produces **no** `STATE_DELTA` — only genuine agent mutations do. The
`TestAGUIState_InboundSeedObserved` and `TestAGUIState_ConflictAgentWins`
integration tests exercise this round-trip: the client seed appears in the
initial `STATE_SNAPSHOT` and is read back through `scene_get`, and a subsequent
agent `scene_patch` on the same `scene_id` wins on the overlapping key (with the
client's untouched keys preserved by shallow-merge) and surfaces as exactly one
`STATE_DELTA`.

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

For an OIDC deployment, replace `bearer_token_env` with an
[`auth:` block](#authentication-auth) — the two are mutually exclusive.

## Request headers

Request headers named `X-Nexus-*` are forwarded into the engine on every turn
this transport starts, keyed by normalized name (`X-Nexus-Tenant-ID` ->
`tenant-id`). Plugins read them from `events.UserInput.Headers` or, outside the
`io.input` path, from `session.RequestHeaders()`. Nothing authenticates them —
they are the caller's own statement about its request — so they are bound to
the reserved, prompt-invisible session-label namespace and surfaced to the
model only when an operator names one in `nexus.system.dynvars.request_headers`.
No configuration turns the forwarding on. See
[Request Headers](../guides/request-headers.md).

## See also

- [Configuration Reference — `nexus.io.agui`](../configuration/reference.md#nexusioagui) — canonical config keys.
- [I/O Transport Plugins](./io/index.md) — the transport family.
- [Browser UI](./io/browser.md) — the session-scoped Envelope transport AG-UI mirrors for scope/exposure.
