# A2A serve transport

`nexus.io.a2a` exposes a Nexus instance as an [Agent2Agent
(A2A)](https://a2aproject.github.io/A2A/) agent. It stands up one HTTP listener
carrying three surfaces:

| Surface | Path | Purpose |
|---|---|---|
| Discovery | `GET /.well-known/agent-card.json` | The Agent Card: who this agent is, where its bindings live, and how to authenticate. |
| JSON-RPC 2.0 binding | `POST <jsonrpc_path>` (default `/a2a`) | A2A specification §9. |
| HTTP+JSON/REST binding | `<rest_prefix>/…` (default `/a2a/v1`) | A2A specification §11. |

The wire format is entirely [`pkg/a2a`](https://github.com/frankbardon/nexus),
Nexus's hand-rolled A2A codec targeting specification 1.0.x. This plugin
contributes the listener, the credential guard, the card assembly and the
routing.

It is the A2A sibling of [`nexus.io.agui`](../io-agui.md) and inherits that
plugin's exemption from the browser/wails transport parity rule: an external
interop transport is not a Nexus UI, so nothing here is back-ported into
`nexus.io.browser` or `nexus.io.wails`.

## Current maturity

> Every A2A operation outside the push-notification family is wired.
> `SendMessage` and `SendStreamingMessage` **drive a real Nexus turn**, every
> task they create is **persisted durably** in a principal-scoped,
> session-scoped SQLite store, `GetTask`, `ListTasks` and `SubscribeToTask`
> **read it back**, a human-in-the-loop question **parks** the task at
> `INPUT_REQUIRED` and is resumed by a message naming the same `taskId`, and
> `CancelTask` **settles** a task at `CANCELED`. A turn publishes its answer, its
> structured output, **every** tool result and every file it wrote as Artifacts,
> and the Nexus telemetry extension is declared and honoured.

The Agent Card reports this rather than advertising an intention:
`capabilities.streaming`, `pushNotifications` and `extendedAgentCard` are all
derived from the set of operations the plugin actually implements. Wiring an
operation flips its capability in the same edit, so the card and the behaviour
cannot disagree. A2A declares no capability boolean for cancellation — it is
part of the core task surface — so the card's honest statement there is simply
that `CancelTask` dispatches instead of refusing.

## Task store

Every task is recorded in `<session>/plugins/nexus.io.a2a/store.db`, opened
through the engine's per-plugin [storage](../../architecture/storage.md)
capability at session scope. The record carries the task id, its `contextId`,
the current state and timestamp, the full status history, the artifacts, message
references for both sides of the exchange, and the authenticated `Principal`
that created it. The task row is written **before** the turn is allowed to
start, and every transition is written through as the frame reporting it is
queued for the wire, so the store never lags what a client has been told.

Reads are principal-scoped by construction: the store hands out a view bound to
one `Principal` and every statement on that view names `principal_id`. There is
no unscoped query in the API, so enumerating another principal's tasks is not an
expression the package can form.

Retention (`tasks.ttl`, `tasks.max_per_context`) is documented in the
[configuration reference](../../configuration/reference.md#task-retention).

## Reading tasks back

| Operation | JSON-RPC | REST |
|---|---|---|
| `GetTask` | `{"method":"GetTask","params":{"id":"…"}}` | `GET <rest_prefix>/tasks/{id}` |
| `ListTasks` | `{"method":"ListTasks","params":{…}}` | `GET <rest_prefix>/tasks?…` |
| `SubscribeToTask` | `{"method":"SubscribeToTask","params":{"id":"…"}}` | `POST <rest_prefix>/tasks/{id}:subscribe` |

**`GetTask`** returns the task with its status, its artifacts and its history.
History is the trail of message *references* the store retained, rendered as
text messages — the client-assigned `messageId`, the role and the text that
travelled — not a replay of Nexus's own conversation buffer. Because this
transport accepts text parts only and emits text artifacts only, that rendering
is lossless for everything the agent can currently say or hear.
`configuration.historyLength` is honoured: unset keeps everything retained, `0`
omits history, and `N` keeps the **most recent** `N` messages.

**`ListTasks`** pages the caller's own tasks, newest first, and supports every
filter the specification defines: `contextId`, `status`, `statusTimestampAfter`
(inclusive), `historyLength` and `includeArtifacts`. Artifacts are **off by
default** to keep a page small, which is the specification's own default;
history is not, so pass `historyLength=0` for a compact listing. `pageSize` defaults to 50 and is bounded to
1–100. The `nextPageToken` is a **keyset** cursor over `(created_at, rowid)`,
not an offset: a task created or evicted while a client is paging cannot make it
skip or repeat a row. A token this server did not mint is an
`InvalidParamsError` rather than a silent restart from the top.

**`SubscribeToTask`** attaches an SSE stream to an existing task and always
opens with the task's current state, so a client that joined mid-turn learns
what it missed before it sees anything new. If the task is live, the subscriber
joins the run's fan-out and receives exactly the frames every other attached
stream receives. If it is already terminal, the opening snapshot carries the
terminal state and the stream closes at once rather than hanging. A task that is
neither gets its snapshot and then a close, because nothing will ever update it
again — though a task this process was serving when it last stopped is settled at
`FAILED` on open, so its snapshot names a real ending rather than a stale
`WORKING`.

### Task ownership is not enumerable

Every read goes through the store's principal-scoped view. A task belonging to
**another** principal answers exactly as an id nobody ever minted does: the same
`TaskNotFoundError`, the same HTTP 404, the same body, from the same single
indexed lookup. There is no "exists but is not yours" response, because that
would be an existence oracle for task ids the caller was never told — the same
reasoning behind the session broker's `errTicketRejected`.

With no `auth:` block configured every caller is unauthenticated and shares one
partition, which is another reason the listener binds loopback by default.

## Details

| | |
|---|---|
| **ID** | `nexus.io.a2a` |
| **Dependencies** | None |
| **Requires** | None |
| **Subscriptions** | `agent.turn.start`, `agent.turn.end`, `llm.request`, `llm.response`, `io.output`, `core.error`, `hitl.requested`, `hitl.responded`, `tool.invoke`, `tool.result`, `thinking.step`, `subagent.started`, `subagent.iteration`, `subagent.complete` |
| **Emissions** | `before:io.input`, `io.input`, `hitl.responded`, `hitl.cancel`, `cancel.request` |
| **Listens?** | Yes — loopback by default (`127.0.0.1:8091`) |

## How a message becomes a turn

```
SendMessage / SendStreamingMessage
  └─ message.parts (text)  ──▶  before:io.input ──▶ io.input
                                                       │
        Task SUBMITTED  ◀── the request was accepted    │
        Task WORKING    ◀── agent.turn.start            ▼
        Artifact (tool) ◀── tool.result               the agent runs
        Artifact (file) ◀── a path a tool.result named
        metadata (ext)  ◀── thinking.step / tool.invoke / subagent.* / usage
        Artifact (text) ◀── io.output / llm.response
        Task COMPLETED  ◀── agent.turn.end
        Task FAILED     ◀── core.error (fatal or retries exhausted)
```

One call is one Task is one turn. `SendMessage` **blocks** until that task is
terminal and returns the finished Task — A2A's default (§3.2.2) —
while `SendStreamingMessage` writes the same frames as SSE and closes the stream
the moment a frame reports a terminal state. Both bindings render from one
translation, so they cannot report different outcomes for the same turn.

The turn's final assistant text is published as an Artifact carrying a text
Part. It is taken from `io.output` rather than straight from the model, so
whatever the output gates actually let through is what the client receives. What
else a turn publishes is covered in [What a turn
publishes](#what-a-turn-publishes) below.

Refusals worth knowing about, each carrying the error type the specification
reserves for it: a non-text Part (`ContentTypeNotSupportedError`), an inline
`taskPushNotificationConfig` (`PushNotificationNotSupportedError`), and a
genuinely concurrent second task while one is in flight
(`UnsupportedOperationError` — the listener fronts one agent loop, and two turns
would interleave on the same bus).
`configuration.returnImmediately` is honoured, and a message naming a `taskId`
is a continuation rather than a refusal; both are covered below.

## What a turn publishes

A2A puts task **output** in artifacts and conversation in messages (§3.7). Four
things a Nexus turn produces are output by that reading:

| Artifact | Contents |
|---|---|
| `<taskId>-response` | The answer as a text Part, plus an `application/json` Part when the answer *is* a JSON document. |
| `<taskId>-tool-<callId>` | One per tool result: the output as text (or the error, flagged), plus a JSON Part when the tool produced structured output. |
| `<taskId>-file-<path>` | One per file the turn wrote: the bytes inline as a base64 `raw` Part, with the filename and media type. |
| `<taskId>-artifacts-truncated` | Only when the task spent its artifact budget: how many artifacts were withheld. |

**Structured output is a document, not a string.** When the final text parses as
a JSON object or array — one surrounding markdown fence is unwrapped first — a
second Part carries it as real `application/json`, so a client decodes rather
than re-parses. The text Part stays first, because it is what every A2A client
can render. When an `llm.request` declared a `json_schema`, the artifact's
metadata names it under `nexus.output.schema`.

**Tool results are artifacts unconditionally.** There is no key to disable them.
An interop transport whose observability depends on the operator having switched
it on is one a partner cannot rely on; the volume that buys is answered by the
caps below rather than by a flag.

**A human-in-the-loop question is not an artifact.** It rides the
`INPUT_REQUIRED` status message and the task's message history — see
[below](#human-in-the-loop-is-input_required). A request for input is not output,
and putting it in the output channel as well would count one event twice.

### Files, and what is missed

A file is published only when a `tool.result` **reports** having written it:
through the engine's `ToolResult.OutputFile` field (honoured for every tool), or
through a structured-output key named by `artifacts.file_sources` — whose default
rule is `nexus.tool.fileio`'s `write_file` reporting `path`.

Snapshot-diffing the session workspace is deliberately out of scope, so **a write
by an uninstrumented path is missed by design**. A shell command redirecting into
a file reports stdout, stderr and an exit code and nothing about the file, so
nothing is published for it — which is why `nexus.tool.shell` has no default rule
rather than a rule that cannot fire. An operator whose shell wrapper *does*
report a written path adds it to `artifacts.file_sources`.

Every reported path is resolved against `artifacts.file_base_dir` (the session's
`files/` directory unless configured) and **confined to it**, symlinks followed.
A path that escapes is dropped rather than clamped: a tool reporting
`../../.ssh/id_rsa` is either broken or hostile, and inlining what it named into
a response that leaves the process cannot be walked back.

### The caps are load-bearing

Unconditional tool-result artifacts, times inline base64 file parts, times a
disk-persisted store, is an unbounded product. Three caps make it bounded, and
each one **degrades** rather than dropping silently:

| Cap | Default | Over it |
|---|---|---|
| `artifacts.max_file_bytes` | 256 KiB | The file becomes a metadata note naming it, its size and the cap. |
| `artifacts.max_tool_output_bytes` | 16 KiB | The text is truncated on a rune boundary with a note saying how much was shown. |
| `artifacts.max_task_bytes` | 1 MiB | Further artifacts are suppressed and one notice artifact says how many. |

The store's worst case is `artifacts.max_task_bytes x tasks.max_per_context` —
about 200 MiB at the shipped defaults. Full key documentation is in the
[configuration
reference](../../configuration/reference.md#artifacts).

`configuration.acceptedOutputModes` is honoured rather than merely validated: a
request naming only text media types receives no JSON Part and no inline file
contents, though the files are still reported as notes so the client knows they
exist.

## The Nexus extension

Thinking steps, tool calls, subagent progress and token counts have no canonical
A2A field, so they ride the Nexus extension:

```
https://github.com/frankbardon/nexus/a2a/extensions/agent-events/v1
```

It is declared in the Agent Card under `capabilities.extensions`, never
`required`, and carried as `TaskStatusUpdateEvent.metadata` keyed by that URI.
The status those frames carry is the task's **current** state rather than a
hard-coded `WORKING`, so a telemetry frame emitted while the task is parked does
not tell a client the task went back to work.

A client opts in per request with the `A2A-Extensions` service parameter, and the
response echoes back what was actually **activated**. A client that did not ask
receives a stream with no extension metadata on it at all — the point of an
opt-in is that it is honoured by not sending, not by the client filtering.

Telemetry is the one frame class that is **not** persisted. Storing it would put
a `WORKING` transition in the task's status history for every thinking step, so
`GetTask` would replay a turn's reasoning as state changes the task never made.
It is a live signal on an attached stream; the store records what the task *is*.

## A task outlives the request that started it

A run is this listener's single active task and is released when the **task**
reaches a terminal state, not when the HTTP request that started it returns —
and the release happens **before** the response reporting that terminal state is
written, on both the blocking and the streaming path. So a client that has read a
terminal Task may immediately send again on the same `contextId` without meeting
`TASK_ALREADY_IN_FLIGHT`. `slot_test.go` pins that ordering; the guide states the
contract and the client-visible reasoning behind it, including why a task parked
at `INPUT_REQUIRED` deliberately keeps the slot — see [A terminal response means
the slot is already
back](../../guides/a2a.md#a-terminal-response-means-the-slot-is-already-back).

That one change is what makes the rest of this page possible. A client can
disconnect mid-turn without failing its own task — the turn carries on, `GetTask`
still answers, and `SubscribeToTask` reattaches to exactly where it got to. A
question can stay parked for as long as a human takes to answer it. And
`configuration.returnImmediately` is answerable: the call returns the task as it
stands and the client follows it by other means (streaming ignores the flag,
since a stream is already that follow-up).

The cost is that a turn nobody is watching holds the slot until something ends
it. `CancelTask` is that something, which is why the two landed together, and
why an unanswered question has a deadline.

A task left non-terminal by a **process restart** is settled at `FAILED` when the
store next opens, with a status message saying the agent stopped while it was
running. Nothing would ever move such a task again — no run drives it and no bus
event will name it — and retention only evicts terminal tasks, so leaving it as
found would mean an immortal row reporting `WORKING` for ever and counting
against the per-context cap.

## Human-in-the-loop is `INPUT_REQUIRED`

When a Nexus agent asks a human something — `nexus.control.hitl`'s `ask_user`
tool, or any plugin emitting `hitl.requested` — the task parks:

```
hitl.requested  ──▶  Task INPUT_REQUIRED, question on status.message
                     (stream stays open; state written through to the store)

SendMessage{taskId, contextId}  ──▶  hitl.responded  ──▶  Task WORKING
                                                          (same turn, no io.input)
```

The task stays **live** while parked. Open SSE streams stay open — §11.7's close
rule keys off terminal states and this is not one — because closing on a
non-terminal state is indistinguishable client-side from a dropped connection. A
parked stream is kept warm with SSE comment records so proxy idle timeouts do
not kill it.

The client resumes by sending a new message carrying the **same `taskId` and
`contextId`**, which is A2A's own resume mechanism (§3.4). The answer is routed
to `hitl.responded` and the task returns to `WORKING` **inside the turn that
asked** — no `io.input` is emitted and no second task is created. A
multiple-choice question renders its option ids into the question text, and an
answer matching one of them (case-insensitively) is delivered as that choice
rather than as free text.

Continuing a task is refused with `UnsupportedOperationError` when it is already
terminal, when the message names a different `contextId` than the task's, or
when the task is not waiting for input. A `taskId` belonging to another
principal answers exactly as an unknown one does: `TaskNotFoundError`.

Because a parked task holds the process's one agent loop, the wait is bounded by
`tasks.input_timeout` (default `15m`). On expiry the task is driven to `FAILED` —
a real terminal transition, so the store, every attached subscriber and the
client all agree — and `hitl.cancel` retracts the question so the blocked agent
unblocks. `"0s"` disables the deadline; the consequence is a task that can stay
parked until the process exits.

**A client must act on the `INPUT_REQUIRED` frame, not on the stream ending.**
Holding the stream open is deliberate (above), so a client that waits for the
stream to end before reading it will wait until a deadline fires rather than
seeing the question. Nexus's own outbound leg,
[`nexus.agent.a2a_remote`](../agents/a2a-remote.md#chaining-works-on-both-bindings),
stops reading on the interruption frame and resumes on a fresh connection, so it
chains a question at its default `stream: true`. The blocking `SendMessage`
binding is equally fine: it returns the parked Task as soon as it parks.

## Cancelling a task

`CancelTask` (`POST <rest_prefix>/tasks/{id}:cancel`) settles the task at
`TASK_STATE_CANCELED` and **then** tells the bus, in that order:

1. `hitl.cancel`, if the task was parked on a question — otherwise the agent
   would stay blocked on an answer that is never coming.
2. `cancel.request`, which is the `control.cancel` capability's entry point.
   Cancellation is that plugin's job for every transport; this one asks, exactly
   as the TUI and browser transports do.

Settling first is what keeps the stream contract intact: once the task is
terminal every later frame is dropped, so nothing produced by the teardown can
arrive after the frame that closed the stream.

Cancelling an **already-terminal** task is refused with `TaskNotCancelableError`
(HTTP 400 / `FAILED_PRECONDITION`) and writes nothing. Reporting success would
tell a client its cancel took effect on a task that had already completed and
whose output it is about to read.

## `contextId` is the Nexus session

An A2A context is a conversation, and so is a Nexus session. They map onto each
other — but a Nexus process owns exactly **one** session, fixed at boot, with one
`memory.history` buffer and no bus primitive that starts a second session or
resets history. So:

- The first call **claims** the session. A client that names no `contextId` is
  assigned the session id and gets it back on the Task.
- Later calls naming the **same** context continue the conversation, history
  intact.
- A **different** `contextId` is refused, naming the bound one. Accepting it
  would hand the caller a conversation already carrying another context's
  history while calling it new, which is worse than an error. One instance per
  context is the answer, and the [session
  broker](../../guides/session-broker.md) exists to automate that.

The context is resolved before the in-flight slot is checked, so this refusal
(`CONTEXT_NOT_SERVED`, permanent) is never issued as `TASK_ALREADY_IN_FLIGHT`
(transient) merely because a task happened to be running, and a refused request
leaves no binding behind. The guide sets out [which refusal a client gets and why
the difference
matters](../../guides/a2a.md#which-refusal-you-get-and-why-the-difference-matters).

## Configuration

The [Configuration Reference](../../configuration/reference.md#nexusioa2a) is
canonical for every key, its type and its default. A minimal working block:

```yaml
plugins:
  active:
    - nexus.io.a2a

  nexus.io.a2a:
    bind: "127.0.0.1:8091"
    bearer_token_env: NEXUS_A2A_TOKEN
    artifacts:
      # Every knob has a non-zero default; these are the shipped values.
      max_file_bytes: 262144
      max_tool_output_bytes: 16384
      max_task_bytes: 1048576
    card:
      name: "Nexus Research Agent"
      description: "Runs research turns with web search and file tools."
      version: "1.2.0"
      skills:
        - id: research
          name: "Research a topic"
          description: "Searches the web and summarizes findings with citations."
          tags: ["research", "search"]
```

Exactly one of `card:` (inline) or `card_file:` (a JSON Agent Card document on
disk, `~`-expanded through `engine.ExpandPath`) is required. They are mutually
exclusive rather than merged: a card is a public contract, and a field-level
merge means the document an operator reads in one place is not the one that gets
served.

## Three decisions worth knowing about

### The card is half hand-authored, half derived

`card:` supplies identity, provider, modes and **skills**. Everything that
describes what the listener actually does — `supportedInterfaces`,
`capabilities`, `securitySchemes`, `securityRequirements` — is **derived** and
overwrites whatever the card source carried, `card_file` included. There are no
config keys for those, deliberately: a card naming a URL nothing is bound to, a
capability nothing implements, or a scheme nothing enforces is worse than no
card at all.

Skills in particular are **not** taken from `nexus.skills` or the tool catalog.
An internal catalog churns with every plugin an operator enables; a discovery
document that churned with it would leak internal structure and break clients
that keyed off it.

### The card endpoint is public by default

Specification §8.2 makes the well-known URI a pre-authentication bootstrap step
— a client fetches the card precisely to learn which credentials to obtain — so
gating it behind those same credentials is circular. The card stays
unauthenticated even when every operation is guarded.

That is safe because the listener binds loopback by default, and because the
card's contents are hand-authored, so what it reveals is what an operator chose
to reveal. Move `bind` off loopback and still need the card private? Set
`card_requires_auth: true` and distribute the document out-of-band, which §8.2
sanctions as "Direct Configuration" — at the cost of being undiscoverable to
clients that have not already been told about you.

### An absent `A2A-Version` header is read as 1.0, not 0.3

Specification §3.6.2 says an agent MUST read an empty `A2A-Version` as `0.3`.
That rule protects clients that predate the parameter from an agent that
silently upgraded under them. This listener has never served `0.3` and its card
advertises `1.0` on every interface, so a header-less request is not a `0.3`
client — there are none — it is a `1.0` client whose HTTP layer omitted a
header. Refusing it buys no compatibility and costs interop.

Every response therefore carries `A2A-Version: 1.0` so the client can see what
it was processed as. Set `strict_version_header: true` to restore the literal
behaviour (useful for a conformance harness). An *explicit* `A2A-Version: 0.3`
is refused either way — the policy only governs absence.

## Authentication

Two spellings, mutually exclusive, identical to `nexus.io.agui`:

- `bearer_token` / `bearer_token_env` — one shared secret, desugared into a
  one-entry `static` validator (which makes the comparison constant-time).
- `auth:` — the full `pkg/nexusauth` validator chain: `static`, `jwks`,
  `introspect`, `proxy_headers`.

Setting both is a boot error naming both keys. Setting neither disables
authentication entirely, which is safe only because the bind address defaults to
loopback — change `bind` and configure auth in the same commit.

The card's `securitySchemes` are derived from the chain, so what a client is
told to present is what is enforced. Note that a `proxy_headers` validator
publishes **no** scheme: it accepts no client credential at all, only an
identity a trusted fronting proxy already established.

See [Agent Card security
schemes](../../configuration/reference.md#agent-card-security-schemes) for the
full mapping.

## Trying it

```bash
bin/nexus -config configs/test-a2a-serve.yaml
```

> That config's `nexus.io.test` block carries `timeout: 20s`, which ends the
> session — and the process — twenty seconds after boot. Raise it for a longer
> window to poke at the endpoint by hand.

```bash
# Discovery needs no credentials.
curl -s localhost:18191/.well-known/agent-card.json | jq

# Operations do. This one runs a turn and streams it.
curl -sN localhost:18191/a2a \
  -H 'Authorization: Bearer test-a2a-token' \
  -H 'A2A-Version: 1.0' \
  -H 'Content-Type: application/a2a+json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"SendStreamingMessage","params":
       {"message":{"messageId":"m1","role":"ROLE_USER",
        "parts":[{"text":"hello"}],"contextId":"demo"}}}'
```

Each SSE record carries one `StreamResponse`: the opening Task in
`TASK_STATE_SUBMITTED`, a `TASK_STATE_WORKING` status update, an artifact
holding the reply, and the `TASK_STATE_COMPLETED` update that closes the stream.
Send the same `contextId` again to continue the conversation.

```bash
# List the tasks this token owns, then read one back.
curl -s 'localhost:18191/a2a/v1/tasks?pageSize=5' \
  -H 'Authorization: Bearer test-a2a-token' -H 'A2A-Version: 1.0' | jq

curl -s localhost:18191/a2a/v1/tasks/<task-id> \
  -H 'Authorization: Bearer test-a2a-token' -H 'A2A-Version: 1.0' | jq
```

Swap the method for `CancelTask` to see the `UnsupportedOperationError`
(`-32004`) the one unwired operation still returns, ask for a task id that does
not exist for the `TaskNotFoundError` (`-32001`), or drop the `Authorization`
header for the `401` and its RFC 6750 challenge.

## See also

- [A2A Interoperability guide](../../guides/a2a.md) — the protocol mapping, a
  worked end-to-end example, and what is deliberately unsupported
- [Configuration Reference — `nexus.io.a2a`](../../configuration/reference.md#nexusioa2a)
- [AG-UI transport](../io-agui.md) — the structural sibling
- [Authentication (`auth:`)](../../configuration/reference.md#authentication-auth)
