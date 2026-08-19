# Broker IO (dial-back transport)

`nexus.io.broker` is the IO transport for Nexus instances spawned by the
[session broker](../../guides/session-broker.md) (`cmd/nexus-broker`).

Unlike every other IO transport, this plugin **dials out** instead of
listening. `nexus.io.tui`, `nexus.io.browser`, and `nexus.io.realtime` all open
a listening socket and wait for a client to connect. The broker plugin does the
opposite: when an instance boots, the plugin dials **back** to the broker's
instance gateway over a single WebSocket. The broker is the only listening
socket in the system — there is no per-instance loopback port to allocate or
firewall.

You normally never configure this plugin by hand. The broker injects its config
via environment variables when it spawns an instance, and the plugin reads them
on boot. It is included for completeness and for anyone embedding the broker
protocol in a custom host.

## Details

| | |
|---|---|
| **ID** | `nexus.io.broker` |
| **Dependencies** | None |
| **Spawned by** | `cmd/nexus-broker` (one instance per lease) |
| **Listens?** | No — it dials out to the broker gateway |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `broker_addr` | string | `$NEXUS_BROKER_ADDR` | WebSocket URL of the broker's instance dial-back endpoint, e.g. `ws://127.0.0.1:8080/instance`. Falls back to the `NEXUS_BROKER_ADDR` env var (injected by the broker at spawn). When empty the plugin stays **dormant** — it does not dial and the engine still boots cleanly. |
| `lease_id` | string | `$NEXUS_BROKER_LEASE_ID` | Lease id the broker assigned to this instance; echoed in the `register` frame so the gateway can bind this socket to the lease. Falls back to the `NEXUS_BROKER_LEASE_ID` env var. When empty the plugin stays dormant. |
| `spawn_secret` | string | `$NEXUS_BROKER_SPAWN_SECRET` | Per-spawn secret the broker generated for this instance and injected at exec; echoed in the `register` frame alongside `lease_id` so the gateway can prove this process is one it spawned. Falls back to the `NEXUS_BROKER_SPAWN_SECRET` env var. Empty does **not** make the plugin dormant — it dials and is refused — because "no secret" is a diagnosable failure at the broker, not a reason to stay silent. Every broker requires it, with or without an `auth:` block. |

Config keys take precedence over the environment variables. The reference table
above is canonical; see the
[Configuration Reference](../../configuration/reference.md#nexusiobroker).

## How it works

On `Ready` (after the engine is fully up), the plugin:

1. **Dials** `broker_addr` over WebSocket using `github.com/coder/websocket`.
2. **Registers** by sending a `register` frame keyed by `lease_id` and carrying
   `spawn_secret` — this MUST be the first frame so the gateway can bind the
   socket to the lease. The secret rides on this frame and no other: every later
   frame is forwarded verbatim to the connected client.
3. **Announces readiness** with a `ready` frame. The broker's `POST /claim`
   handler is blocked on exactly this signal before it returns to the caller.
4. **Reports the session id** with a `session-id-report` frame so the broker can
   persist the engine-generated session id for a later `-recall` resume.
5. **Bridges IO** in both directions for the rest of the session.

If the connection drops, the plugin reconnects with exponential backoff
(250 ms → 5 s) until shutdown. That loop is also what makes **broker restart
recovery** work: a broker configured with a `state_dir` restores this instance's
lease at boot and accepts the re-registration, so a surviving instance rejoins
with no plugin configuration and no client involvement. See
[Surviving a restart](../../guides/session-broker.md#surviving-a-restart-set-state_dir).

### Output is buffered across a reconnect

Everything the instance emits while that socket is down is **held, not dropped**.

The reason is the backoff above. A broker restart puts the instance into the
reconnect loop for anywhere from 250 ms to several seconds, and the agent keeps
running throughout — so the window in which reattach is supposed to be seamless
is exactly the window in which output is produced with nowhere to go. Outbound
frames used to be written inline and discarded when the write failed, which lost
them **on the instance side**, before the broker had ever seen them and therefore
out of reach of the broker's own
[client replay buffer](../../guides/session-broker.md#frame-sequencing-and-the-replay-buffer).

How it behaves:

| | |
|---|---|
| **Bound** | 1 MiB per instance, fixed (not configurable) |
| **Measured in** | bytes, not frames |
| **Eviction** | oldest first |
| **On overflow** | a `WARN` naming `dropped_frames`, `dropped_bytes` and `limit_bytes`, rate-limited to one per second with cumulative counts |
| **Flushed** | after the `register` / `ready` / `session-id-report` handshake of the next session |

The bound is in **bytes** because outbound payloads span orders of magnitude — a
token delta is a few bytes, a tool result is tens of kilobytes — so a frame count
says nothing about how much memory a disconnected instance can pin. 1 MiB holds
many seconds of streaming for a chatty agent and still holds several whole tool
results for one that only emits large frames; it is the same figure as the
broker's per-lease `client_replay_buffer_bytes` default, so the two halves of one
stream pin comparable memory. Oldest-first eviction keeps the most recent tail,
which is the part a client reattaching after a gap actually needs. A single frame
larger than the whole bound is not retained at all — the alternative is a bound
one big tool result can breach.

The flush happens **after** the handshake, never before it: a buffered frame that
overtook the `register` frame would reach a broker that has not yet bound the
socket to the lease. A single write pump drains the buffer oldest-first, which is
also what makes emission order the wire order however many bus handlers are
producing at once, and a frame is removed from the buffer only once it has been
written — a write that fails on a dying socket leaves it at the head for the next
session.

`SendIO` never touches the socket. Bus dispatch is synchronous, so it runs on the
goroutine producing the agent's output; enqueuing under a short-lived lock keeps
a slow or dead link from stalling the agent.

Two cases deliberately do **not** buffer:

- A **dormant** plugin (no `broker_addr` or no `lease_id`) never dials, so its
  output is dropped at `DEBUG` exactly as before rather than pinning a megabyte
  for a link that is not coming up.
- **Shutdown** flushes on a bound, not on a promise. `Shutdown` gives the write
  pump up to 2 seconds to drain a live socket — so the last output before a
  locally initiated shutdown still reaches the broker — and skips the wait
  entirely when nothing is connected. A non-empty buffer can never turn a
  graceful shutdown into a hang. (On the broker-initiated path the `shutdown`
  frame has already ended the session, so there is no socket left to flush to.)

### Outbound (engine bus → broker → client)

These engine events are forwarded as IO messages inside broker frames:

| Bus event | IO message `type` |
|-----------|-------------------|
| `io.output` | `output` |
| `llm.stream.chunk` | `stream.delta` |
| `llm.stream.end` | `stream.end` |
| `io.status` | `status` |
| `io.approval.request` | `approval.request` |
| `hitl.requested` | `hitl.request` |
| `cancel.complete` | `cancel.complete` |

Output already delivered as `stream.delta` chunks is not re-sent as a final
`output` message (the plugin skips `io.output` events flagged `streamed`).

A `hitl.request` carries the question's `mode` and `choices` alongside its
`prompt`, spelled exactly as `nexus.io.browser` spells them. A responder answers
a multiple-choice question with a `choice_id`, so a payload carrying only the
prompt gives it no way to learn what the ids are. Both fields are `omitempty`, so
a free-text question's payload is byte-identical to one with no options at all —
a consumer that ignores them behaves exactly as it did before they existed.

The broker itself is now a second reader of this envelope: its
[A2A ingress](../../guides/session-broker.md#what-the-a2a-ingress-translates)
decodes these payloads to drive A2A tasks. A field added here must be mirrored
in `cmd/nexus-broker`, and a test enforces that (it parses this plugin's source
and fails if the two declarations disagree).

### Inbound (client → broker → engine bus)

Inbound IO messages are decoded and injected onto the bus:

| IO message `type` | Bus event |
|-------------------|-----------|
| `input` | `before:io.input` (vetoable) → `io.input` |
| `approval.response` | `io.approval.response` |
| `hitl.response` | `hitl.responded` |
| `cancel` | `cancel.request` |

`io.input` is emitted from a goroutine (not the read pump) because bus dispatch
is synchronous and an agent loop may block waiting on a HITL response — the same
pattern `nexus.io.browser` and `nexus.io.realtime` use.

### Graceful shutdown

When the broker tears a lease down (manual `POST /release`, idle, or crash
handling) it sends a `shutdown` frame. The plugin then:

- latches its reconnect loop off so the teardown is not undone by a retry, and
- emits `io.session.end`, which drives a clean engine `Stop` that flushes and
  persists the session before the process exits.

The plugin never hard-exits mid-write; the engine owns teardown ordering. The
broker bounds how long it waits for the process and force-kills it if the
graceful path overruns (`release_grace`).

Anything still in the outbound buffer is flushed on a bound rather than waited
out — see [Output is buffered across a reconnect](#output-is-buffered-across-a-reconnect).

## Security

The plugin makes **no authorization decisions**. It presents the credentials the
broker handed it and the broker gateway decides; there is nothing here to
configure or bypass.

### The spawn secret

The `register` frame carries a **second factor** beside the lease id. The lease
id alone is a poor authenticator for the dial-back socket: the same value appears
in `ws_url`s, client requests and logs, so anything that observes one could
otherwise impersonate an instance. The broker records the expected value on the
lease and injects it through the child's **environment** — never argv, which is
world-readable via `ps` and `/proc`. Both values must match or the gateway closes
the socket with the same `policy violation` close an unknown lease gets.

How the broker produces the value depends on whether it keeps state, and the
plugin cannot tell the difference — it echoes whatever it was given:

- **No `state_dir`** — 128 bits of `crypto/rand` per spawn, held only in memory.
- **`state_dir` set** — derived as `HMAC-SHA256(<state_dir>/spawn-key, lease_id)`,
  so a restarted broker can recompute the value this instance is still holding and
  let it reattach. The secret itself is still never written to disk.

Enforcement is decided entirely by the broker, and it is **unconditional**: the
secret is required on every registration — with an `auth:` block or without one,
on a freshly claimed lease or on one restored after a broker restart. That block
configures how *clients* are verified; it never described what the broker should
believe about an unverified dialer.

> **Breaking change.** Enforcement used to be gated on the broker having an
> `auth:` block, so a `nexus` binary predating the protocol kept registering on an
> unauthenticated broker. It no longer does, and dropping the `auth:` block is no
> longer a workaround. Upgrade the instance binary.

Alongside the secret, the broker validates the `register` frame's **schema
version** against its own and refuses a mismatch. Both failures are `WARN`s that
name their own fix, because the symptom otherwise looks like a network fault:
claims time out with `instance did not become ready in time` while the child is
alive and connecting fine.

Reattaching to a lease restored after a broker restart is the case where this
matters most. The broker only knows that the recorded pid is alive, and a pid can
be recycled to an unrelated process while the broker is down; the secret is the
only thing that distinguishes the genuine instance. An instance binary too old to
send one cannot reattach and its lease is reaped after `reattach_window`.

The plugin never logs the secret. Its init record carries a
`spawn_secret_present` boolean instead, which is what you want when diagnosing a
refused registration: it answers whether the value ever reached the process.

See the [session broker guide](../../guides/session-broker.md#v1-caveats) for the
full list of broker-level limitations.

## Example configuration

You rarely write this by hand — the broker injects both values as environment
variables at spawn. When you do set them explicitly:

```yaml
nexus.io.broker:
  broker_addr: "ws://127.0.0.1:8080/instance"
  lease_id: "lease-abc123"
  spawn_secret: "9d4e7a10c3b28f56ae0192b3c4d5e6f7"   # only a broker can mint a usable one
```

Omit `broker_addr` and `lease_id` (or leave their env vars unset) and the plugin
stays dormant, so a config that activates the plugin outside a broker still boots
without error. `spawn_secret` does not affect dormancy, but omitting it means
every registration is refused: no broker accepts a register frame without one.

## See also

- [Session Broker guide](../../guides/session-broker.md) — running the broker,
  the HTTP API, and the new-vs-resume flow.
- [Configuration Reference](../../configuration/reference.md#nexusiobroker).
