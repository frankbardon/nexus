# Request Headers (`X-Nexus-*`)

Every web-facing Nexus IO transport forwards the request headers whose names
begin with `X-Nexus-` into the engine, where plugins can read them. It is the
supported way for a client to pass per-request out-of-band context — a tenant,
a locale, a timezone, an upstream authorization subject, a trace correlator —
without smuggling it through the prompt.

```bash
curl -X POST http://localhost:8088/agui \
  -H 'Content-Type: application/json' \
  -H 'X-Nexus-Timezone: Europe/Amsterdam' \
  -H 'X-Nexus-Tenant-ID: acme' \
  -d '{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"what time is my 3pm?"}]}'
```

No configuration turns this on. A header in the namespace is carried; anything
else is not.

## What a plugin sees

The header name is normalized: the `X-Nexus-` prefix is stripped and the
remainder lowercased, so `X-Nexus-Tenant-ID` arrives as `tenant-id`. Matching
is case-insensitive, as HTTP field names are. Repeated field lines are joined
with `", "`.

There are two read seams, carrying the same values, and which one to use
depends on whether your plugin handles `io.input`.

**On the event** — for anything already subscribed to `io.input`:

```go
func (p *Plugin) handleInput(e engine.Event[any]) {
    in, ok := e.Payload.(events.UserInput)
    if !ok {
        return
    }
    if tz := in.Headers["timezone"]; tz != "" {
        // ...
    }
}
```

**On the session** — for a tool, a memory provider, an LLM provider, or
anything else that never sees the turn's input event:

```go
// One header.
if tenant, ok := p.session.RequestHeader("tenant-id"); ok {
    // ...
}

// Or all of them.
headers, err := p.session.RequestHeaders()
```

`p.session` is the `*engine.SessionWorkspace` from `PluginContext.Session`. It
is nil when the engine runs without a session, so guard the call.

## These values are not authenticated

A header says only what the caller typed. Nothing verifies it, and nothing
can: it is an unsigned string on an HTTP request.

That is not a reason to avoid them — it is a reason to use them for what they
are. `X-Nexus-Tenant: acme` is a useful statement of which tenant the caller
*claims*, once the credential the request also carried has been validated.
Credential validation is [`pkg/nexusauth`](./a2a.md#authentication)'s job, and
it is a different mechanism reached through the `auth:` block. A header is
never a substitute for it.

Two consequences follow from that, and both are deliberate:

- **Headers are invisible to the model by default.** They are bound into the
  reserved (`_`-prefixed) session-label namespace, which every
  `<session_context>` prompt builder filters out. A caller cannot get text in
  front of the LLM by inventing a header. Surfacing one is an explicit opt-in
  (below).
- **Nothing else can forge one.** The reserved namespace is unreachable from
  the general `before:session.tag.set` bus path, so no plugin — and no agent
  holding the `nexus.tool.session_tags` tools — can write or overwrite a
  `_header.*` label. The transport is the only writer.

## Surfacing a header to the model

When the agent itself needs a value — a timezone is the usual case — name it
in `nexus.system.dynvars`:

```yaml
plugins:
  nexus.system.dynvars:
    request_headers: [timezone, locale]
```

Each named header, when the current turn carries it, contributes one line to
the plugin's `<system-context>` block:

```
Request header X-Nexus-timezone: Europe/Amsterdam
```

It is an allowlist rather than a boolean on purpose: an operator who wants the
caller's timezone in the prompt gets exactly that, and does not silently also
get whatever new header a client starts sending next month.

## Lifetime

Headers describe **one request**, not the session. Every turn replaces the
whole bound set, including when the request carried none — a second turn that
drops `X-Nexus-Tenant` leaves nothing behind for a plugin to misread as still
current. They are bound before the turn's `io.input` is emitted, so a handler
never observes a turn ahead of the context it belongs to, and cleared when the
run ends.

For a WebSocket transport (`nexus.io.browser`, `nexus.io.realtime`) the
headers come from the **upgrade** request, because that is the only request a
WebSocket has. They are connection-scoped: every message on a connection
carries the values it was opened with, and a client that needs to change one
reconnects. When several browsers share a session, each turn carries the
headers of the connection that actually typed it.

## Bounds

Headers are persisted into session metadata and may be rendered into a prompt,
so the extraction is bounded (`pkg/nexusheaders`):

| Bound | Value |
|-------|-------|
| Headers per request | 32 |
| Name length | 128 bytes |
| Value length | 4096 bytes |
| Total names + values | 16384 bytes |

Exceeding a bound drops entries rather than failing the request — a header is
supplementary context, and refusing an otherwise valid turn over a 33rd one
would trade a real request for a cosmetic rule. Dropping is deterministic:
names are sorted, then kept while the bounds allow, so the same request always
yields the same map.

Control characters are stripped from values. This matters less for response
splitting (Go's HTTP server already rejects CR and LF in a field value) than
for where these values go: an embedded newline in a value rendered into a
system prompt would let a caller forge a line break in a block the agent reads
as structure.

## Transport coverage

| Transport | Where the headers come from |
|-----------|------------------------------|
| `nexus.io.agui` | the `POST /agui` request |
| `nexus.io.a2a` | the JSON-RPC or HTTP+JSON request, including the one that resumes a task parked at `INPUT_REQUIRED` |
| `nexus.io.browser` | the WebSocket upgrade, per connection |
| `nexus.io.realtime` | the WebSocket upgrade, per connection |
| `nexus.io.broker` | forwarded by `nexus-broker` on the instance IO envelope — see below |

`nexus.io.tui`, `nexus.io.oneshot` and `nexus.io.wails` have no HTTP request
behind them, so `UserInput.Headers` is nil and no `_header.*` label is bound.

### Through the session broker

An instance spawned by `cmd/nexus-broker` never sees the client's HTTP
request — the broker terminates it. The broker therefore extracts the
`X-Nexus-*` headers itself and forwards them on the `input` message of the
instance IO envelope, where `nexus.io.broker` applies them exactly as an
in-process transport would.

That forwarding covers the broker's **A2A** surface, which is the part of the
gateway that parses and translates client requests
(see [Session Broker](./session-broker.md)). Everything outside the `agents:`
namespace is still forwarded unparsed, so a client speaking a protocol the
broker does not decode carries no headers through the hop.

The envelope field is additive and `omitempty`: an older broker in front of a
newer instance simply never sets it, and the instance behaves as it did
before.

## Reference

- Convention, normalization and bounds: `pkg/nexusheaders`
- Event field: `events.UserInput.Headers` (schema v3)
- Session seam: `engine.SessionWorkspace.SetRequestHeaders` /
  `RequestHeaders` / `RequestHeader`
- Label namespace: `_header.<name>`, reserved — see
  [Sessions](../architecture/sessions.md)
