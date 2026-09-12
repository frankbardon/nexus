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

**The verified caller identity** — where the transport has an `auth:` chain —
is read from the same session, and is the value to gate on:

```go
if id, ok := p.session.PrincipalID(); ok {
    // id was checked by a pkg/nexusauth validator, not asserted by the caller.
}
```

## This is a data-injection mechanism, not an auth surface

Treat `X-Nexus-*` as a way to hand plugins some data about the request, and
nothing more. A header says only what the caller typed — nothing verifies it,
and nothing can: it is an unsigned string on an HTTP request. Any caller that
can reach the transport can send any value.

So it is the right tool for **descriptive context** that shapes behaviour —
tenant, locale, timezone, a feature flag, a correlation id — and the wrong tool
for anything that grants access. Do not gate a decision on a header, and do not
put a secret in one.

Verified identity is a separate mechanism with a separate slot. A transport's
`auth:` block builds a [`pkg/nexusauth`](./a2a.md#authentication) validator
chain, and the identity it *checks* is bound to the reserved `_principal_id`
label, readable via `session.PrincipalID()`. That is what an authorization
decision belongs on. The two are deliberately kept in differently-named slots
so a reader can always tell which it is holding:

| | `_header.<name>` | `_principal_id` |
|---|---|---|
| Source | what the caller asserted | what a validator checked |
| Trust | none | the transport's `auth:` chain |
| Use for | context, behaviour | authorization |
| Read with | `session.RequestHeaders()` | `session.PrincipalID()` |

Both live in the reserved (`_`-prefixed) namespace, which buys two things:

- **Invisible to the model by default.** Every `<session_context>` prompt
  builder filters reserved keys out, so a caller cannot get text in front of
  the LLM by inventing a header. Surfacing one is an explicit opt-in (below).
- **Unforgeable by plugins.** The reserved namespace is unreachable from the
  general `before:session.tag.set` bus path, so no plugin — and no agent
  holding the `nexus.tool.session_tags` tools — can write or overwrite either
  label. The transport is the only writer.

A header named `principal_id` lands at `_header.principal_id` and is not the
verified identity; the two cannot collide.

### Headers are persisted

Bound headers are written into `SessionMeta.Labels`, which lives in
`metadata/session.json` — and with `core.object_store` enabled, that file is
snapshotted to the remote store at every turn boundary.

That is a feature for context and a liability for secrets. **Never put an
access token, API key or password in an `X-Nexus-*` header**: you would be
writing a live credential to disk and to a bucket. Pass a subject or an opaque
correlator instead and exchange it for the credential downstream, keeping the
secret out of the session tree entirely.

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
| `nexus.io.broker` | forwarded by `nexus-broker` on every client-facing surface — see below |

`nexus.io.tui`, `nexus.io.oneshot` and `nexus.io.wails` have no HTTP request
behind them, so `UserInput.Headers` is nil and no `_header.*` label is bound.

### Through the session broker

An instance spawned by `cmd/nexus-broker` never sees the client's HTTP
request — the broker terminates it — so the broker extracts the `X-Nexus-*`
headers itself and passes them across the hop. Both of its client-facing
surfaces are covered, by two mechanisms, because the broker relates to them
differently.

On the **A2A** surface (`/agents/<name>/...`) the broker decodes the request
and builds the instance's `input` payload itself, so it simply sets the
headers on that message.

On the **generic client stream** (`GET /leases/{lease_id}/stream`) the broker
is an opaque pipe: the client speaks the instance IO envelope directly and the
gateway forwards its frames verbatim, never decoding or rewriting them. That
rule is what lets an instance run a newer build than the broker in front of
it, and it is not worth breaking to attach a header. So the headers travel as
a frame of their own — the broker sends an instance-bound `client.headers`
payload immediately ahead of each client IO frame, reporting that connection's
headers, and `nexus.io.broker` applies it to the input that follows.

Injecting a broker-authored frame is not the same as modifying a client's:
every byte a client sent still arrives unchanged, and an instance that does
not recognise the payload ignores it, so an older instance behind a newer
broker is unaffected. The announcement is re-sent ahead of every IO frame
rather than once at connect, which costs one small frame per user action and
in exchange has no state to go stale — an announcement sent before the
instance has dialled back would simply be dropped, and an instance re-spawned
mid-connection after a crash would otherwise come up knowing nothing.

An empty announcement is sent too, and is what clears the instance's copy, so
a second client connection that sends no header does not inherit the first
one's values.

**The announcement wins over a `headers` field on the client's own `input`
envelope.** Because the pipe is opaque, a client can set that field and the
broker cannot strip it. The announcement comes from the HTTP request — the hop
an operator's reverse proxy controls and injects into — so if the client's
field won, anything a trusted proxy asserted could be overridden by the caller
it was asserting about.

Both the envelope field and the announcement are additive: an older broker
sends neither, and the instance behaves exactly as it did before.

#### The verified principal crosses the hop too

The broker resolves a principal for every client request — from a `?ticket=`
or an `Authorization` header, through its `auth:` chain — and gates lease
ownership on it. That identity is forwarded to the instance alongside the
headers, on its own `principal_id` field, and `nexus.io.broker` binds it to
`_principal_id`. So a plugin inside a broker-spawned instance reads the same
identity the gateway checked, not merely what the caller asserted.

The two travel together on one payload because they describe the same
connection and must never be observed apart — an instance holding one turn's
identity beside another turn's attributes would be worse than holding neither.
They stay separate *fields* because only one of them is verified. On the
generic pipe a client authors its own `input` payload and could set
`principal_id` on it, so the broker's announcement wins outright: the
announcement is broker-originated, and the broker only ever emits it from the
principal its own validator resolved.

With authentication disabled the announcement carries no principal, which is
the honest answer — there is then no verified identity, only claims.

#### Lock the broker to your gateway

**Recommended:** make the broker's client endpoints reachable only from the
service that fronts them. Any client that can reach
`GET /leases/{lease_id}/stream` with a valid lease credential can set its own
`X-Nexus-*` headers, so if tickets can reach a scripted client, every header
value is caller-chosen. Network isolation is what turns "our gateway stamps
these" from an assumption into a control.

This is not a reason to distrust the mechanism — headers are caller-supplied by
design and the guide says so throughout — but if you intend to *act* on a
header, that action is only as trustworthy as the set of things that can reach
the endpoint. Identity is the exception: `_principal_id` is checked by the
broker's own validator, so it holds regardless of who connects.

## Reference

- Convention, normalization and bounds: `pkg/nexusheaders`
- Event field: `events.UserInput.Headers` (schema v3)
- Session seam: `engine.SessionWorkspace.SetRequestHeaders` /
  `RequestHeaders` / `RequestHeader`
- Label namespace: `_header.<name>`, reserved — see
  [Sessions](../architecture/sessions.md)
- Verified identity: `engine.ReservedPrincipalIDKey` (`_principal_id`),
  `SessionWorkspace.SetPrincipalID` / `PrincipalID`
