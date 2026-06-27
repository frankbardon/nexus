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

Config keys take precedence over the environment variables. The reference table
above is canonical; see the
[Configuration Reference](../../configuration/reference.md#nexusiobroker).

## How it works

On `Ready` (after the engine is fully up), the plugin:

1. **Dials** `broker_addr` over WebSocket using `github.com/coder/websocket`.
2. **Registers** by sending a `register` frame keyed by `lease_id` — this MUST
   be the first frame so the gateway can bind the socket to the lease.
3. **Announces readiness** with a `ready` frame. The broker's `POST /claim`
   handler is blocked on exactly this signal before it returns to the caller.
4. **Reports the session id** with a `session-id-report` frame so the broker can
   persist the engine-generated session id for a later `-recall` resume.
5. **Bridges IO** in both directions for the rest of the session.

If the connection drops, the plugin reconnects with exponential backoff
(250 ms → 5 s) until shutdown.

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

## Security

There is **no auth in the plugin itself**. The broker gateway owns lease
validation and any transport-level authentication. The session broker ships with
**no auth in v1** — it is a trusted-caller boundary only. See the
[session broker guide](../../guides/session-broker.md#v1-caveats) for the full
list of v1 limitations.

## Example configuration

You rarely write this by hand — the broker injects both values as environment
variables at spawn. When you do set them explicitly:

```yaml
nexus.io.broker:
  broker_addr: "ws://127.0.0.1:8080/instance"
  lease_id: "lease-abc123"
```

Omit both keys (or leave the env vars unset) and the plugin stays dormant, so a
config that activates the plugin outside a broker still boots without error.

## See also

- [Session Broker guide](../../guides/session-broker.md) — running the broker,
  the HTTP API, and the new-vs-resume flow.
- [Configuration Reference](../../configuration/reference.md#nexusiobroker).
