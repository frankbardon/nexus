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

> `SendMessage` and `SendStreamingMessage` **drive a real Nexus turn**, every
> task they create is **persisted durably** in a principal-scoped,
> session-scoped SQLite store, and `GetTask`, `ListTasks` and `SubscribeToTask`
> **read it back**. `CancelTask` is the one remaining operation: it is routed,
> version-negotiated and authenticated, then answered with
> `UnsupportedOperationError`.

The Agent Card reports this rather than advertising an intention:
`capabilities.streaming`, `pushNotifications` and `extendedAgentCard` are all
derived from the set of operations the plugin actually implements. Wiring an
operation flips its capability in the same edit, so the card and the behaviour
cannot disagree.

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
neither — one this process was serving when it last stopped — gets its snapshot
and then a close, because nothing will ever update it again.

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
| **Subscriptions** | `agent.turn.start`, `agent.turn.end`, `llm.response`, `io.output`, `core.error` |
| **Emissions** | `before:io.input`, `io.input` |
| **Listens?** | Yes — loopback by default (`127.0.0.1:8091`) |

## How a message becomes a turn

```
SendMessage / SendStreamingMessage
  └─ message.parts (text)  ──▶  before:io.input ──▶ io.input
                                                       │
        Task SUBMITTED  ◀── the request was accepted    │
        Task WORKING    ◀── agent.turn.start            ▼
        Artifact (text) ◀── io.output / llm.response  the agent runs
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
whatever the output gates actually let through is what the client receives.
Richer artifacts — tool results, files, structured output — are a later story.

Refusals worth knowing about, each carrying the error type the specification
reserves for it: a non-text Part (`ContentTypeNotSupportedError`), a message
naming a `taskId` (`TaskNotFoundError` — nothing resumes a stored task yet),
`configuration.returnImmediately` (`UnsupportedOperationError` — see below), and
a second task while one is in flight (`UnsupportedOperationError` — the listener
fronts one agent loop, and two turns would interleave on the same bus).

`returnImmediately` is still refused, but no longer for the original reason. A
task returned early *can* now be polled — `GetTask` and `SubscribeToTask` are
wired. What stops it is lifetime: a run is registered as this listener's single
active task and released when the request that started it returns, so returning
early would release the slot mid-turn and leave the task stuck at `SUBMITTED`
while the agent answered into the void. Detaching a run's lifetime from its
request needs `CancelTask` alongside it, or a turn nobody is watching would hold
the process's only agent loop with nothing able to interrupt it.

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
