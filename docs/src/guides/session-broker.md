# Session Broker

The **session broker** (`cmd/nexus-broker`) is a standalone service that fronts
many OS-isolated `nexus` instances behind a single HTTP/WebSocket ingress.
Callers *claim* an instance, talk to it over a WebSocket, and *release* it when
done. Each instance is a separate `nexus` process, so tenant isolation is
**process isolation**.

It is a protocol-aware gateway, not a blind TCP proxy: it decodes every frame
and routes by lease and signal, which is how it tracks readiness, idleness, and
crashes.

> The broker is **not** an engine plugin. It lives under `cmd/nexus-broker` and
> is built separately from `cmd/nexus`. The only plugin involved is
> [`nexus.io.broker`](../plugins/io/broker.md), which runs *inside* each spawned
> instance and dials back to the broker.

## How it works

```
                          ┌───────────────────────────────────────┐
                          │            nexus-broker               │
   client ──HTTP POST────▶│  /claim  /release/{id}  /leases       │
          ◀──lease+ws_url─│                                       │
          ──WebSocket────▶│  /lease/{id}  ◀──frames──▶  /instance │
                          └───────────────────────────────────────┘
                                       │ exec() with env             ▲
                                       ▼                             │ dials back
                          ┌───────────────────────────────────────┐ │
                          │  nexus instance (own process)         │─┘
                          │   nexus.io.broker plugin              │
                          └───────────────────────────────────────┘
```

1. A caller `POST /claim`s with a full nexus config.
2. The broker acquires a capacity slot, mints a lease, writes the config to a
   temp file, and **cold-spawns** a `nexus` subprocess (`nexus_binary_path`),
   injecting the broker address and lease id as environment variables.
3. The instance's [`nexus.io.broker`](../plugins/io/broker.md) plugin dials
   **back** to the broker's `/instance` endpoint, registers its lease, and
   signals ready. The broker is the only listening socket.
4. `POST /claim` returns the lease id and a `ws_url`. The caller opens that
   WebSocket and IO frames flow client ↔ broker ↔ instance.
5. The instance is released on demand (`POST /release`), on idle
   (`idle_timeout`), or on crash. The session persists on disk and is resumable.

## Running the broker

The broker reads its own YAML config file (default `broker.yaml`, override with
`-config <path>`):

```yaml
# broker.yaml
listen_addr: ":8080"          # HTTP/WS gateway bind address
nexus_binary_path: "nexus"    # path to the nexus binary the broker exec()s
max_concurrent: 8             # max live instances; <=0 = unlimited
idle_timeout: 5m              # release a lease after this much inactivity; <=0 disables
queue_wait_timeout: 30s       # how long an over-cap claim waits in the FIFO queue; <=0 = no waiting
release_grace: 10s            # graceful-shutdown grace before force-kill
```

```bash
# build both binaries
go build -o bin/nexus ./cmd/nexus
go build -o bin/nexus-broker ./cmd/nexus-broker

# run the broker
bin/nexus-broker -config broker.yaml
```

Every config key, its type, and its default are listed in the authoritative
[Configuration Reference](../configuration/reference.md#session-broker-nexus-broker).

### Health check

```bash
curl -s http://localhost:8080/healthz
# {"status":"ok"}
```

## HTTP API

All control-plane calls are plain HTTP/JSON. There is **no authentication** in
v1 (see [caveats](#v1-caveats)).

### `POST /claim` — claim an instance

Body:

```jsonc
{
  "config": "engine:\n  name: example\n",  // required: full nexus config (YAML text)
  "session_id": "prior-session-id"          // optional: resume a persisted session
}
```

Success (`200`):

```jsonc
{
  "lease_id": "…",                          // handle for this instance
  "ws_url": "ws://host:port/lease/<lease>",  // client WebSocket endpoint
  "session_id": "…"                          // engine session id (see new-vs-resume below)
}
```

```bash
curl -s -X POST http://localhost:8080/claim \
  -H 'Content-Type: application/json' \
  -d '{"config":"engine:\n  name: example\n"}'
```

Error responses:

| Condition | Status | Body |
|-----------|--------|------|
| Missing/empty `config` | `400` | `{"error":"claim requires a non-empty config"}` |
| Over capacity, queue wait elapsed | `503` | `{"error":"capacity wait timed out"}` |
| At capacity, queueing disabled (`queue_wait_timeout <= 0`) | `503` | `{"error":"no capacity"}` |
| Instance exited before ready (e.g. resume of a missing/invalid session) | `502` | `{"error":"instance exited before signalling ready"}` |
| Instance did not become ready within the boot window | `504` | `{"error":"instance did not become ready in time"}` |

### New vs. resume

- **New session** — omit `session_id`. The engine generates a fresh session id
  and the instance reports it back; the broker returns it in the response.
  **Capture that id** if you want to resume the session later.
- **Resume** — set `session_id` to a previously returned id. The broker spawns
  the instance with `-recall <id>`, so the engine reloads that session from
  `~/.nexus/sessions/<id>/` and replays its history. The response echoes the
  requested id.

Resuming a session id that does not exist on disk makes the engine fail to boot;
the instance never signals ready and the claim returns **`502`** rather than
silently starting a new session.

### `POST /release/{lease_id}` — release an instance

Gracefully tears a live instance down: the broker sends a `shutdown` frame, the
instance's `nexus.io.broker` plugin emits `io.session.end`, and the engine
performs a clean `Stop` that flushes and **persists the session before exit**.
The broker waits up to `release_grace` and force-kills the process if that
window elapses (orphan prevention). The session directory under
`~/.nexus/sessions/<id>/` is left intact and remains resumable via `-recall`.

```bash
curl -s -X POST http://localhost:8080/release/lease-abc123
# {"status":"released","lease_id":"lease-abc123"}
```

| Outcome | Status | Body |
|---------|--------|------|
| Released (graceful or killed) | `200` | `{"status":"released","lease_id":"…"}` |
| Unknown / already-released lease | `404` | `{"error":"unknown lease"}` |
| Missing lease id in path | `400` | `{"error":"release requires a lease id"}` |

Release is **idempotent**: releasing an already-gone lease returns `404` rather
than erroring, and concurrent releases of the same lease collapse to one
teardown.

### `GET /leases` — list live instances

A read-only introspection surface. Returns the capacity/queue aggregates plus
every live lease, sorted by `created_at` then `lease_id`.

```bash
curl -s http://localhost:8080/leases
```

```jsonc
{
  "max_concurrent": 8,     // configured cap (0 = unlimited)
  "slots_in_use": 2,       // live instances currently holding a slot
  "queue_depth": 0,        // claims parked in the FIFO capacity wait queue
  "leases": [
    {
      "lease_id": "lease-abc123",
      "session_id": "…",
      "pid": 41234,
      "state": "active",           // "spawning" | "active" | "draining"
      "reason": "",                 // teardown reason once draining (e.g. "manual release", "idle")
      "last_activity": "2026-06-25T12:00:00Z",
      "created_at": "2026-06-25T11:59:30Z"
    }
  ]
}
```

Lease states:

| State | Meaning |
|-------|---------|
| `spawning` | The lease exists but its instance has not yet dialed back and registered — the claim is still booting an engine. |
| `active` | The instance has registered; frames can flow. |
| `draining` | A teardown (manual release, idle, or crash) has latched; the lease is on its way out. |

### Connecting over WebSocket

After a successful claim, open the returned `ws_url` and exchange IO frames.
A minimal sketch:

```javascript
const { lease_id, ws_url } = await (await fetch("http://localhost:8080/claim", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ config: "engine:\n  name: example\n" }),
})).json();

const ws = new WebSocket(ws_url);
ws.onmessage = (e) => console.log("frame:", e.data);
ws.onopen = () => {
  // send a user input message into the instance
  ws.send(JSON.stringify({ type: "input", content: "hello" }));
};

// later: release the instance (the session persists on disk)
await fetch(`http://localhost:8080/release/${lease_id}`, { method: "POST" });
```

The IO message shapes carried inside broker frames (`output`, `stream.delta`,
`input`, `approval.response`, …) are documented on the
[`nexus.io.broker` plugin page](../plugins/io/broker.md#how-it-works).

## Capacity and queueing

`max_concurrent` caps live instances. Each claim acquires a slot **before**
spawning, so the live count can never exceed the cap. When the cap is full a
claim does not fail immediately — it parks in a **FIFO wait queue** bounded by
`queue_wait_timeout`. The moment a slot frees (release, idle, or crash) it is
handed to the oldest waiter. Set `queue_wait_timeout` to `0` to disable waiting
(at-capacity claims are rejected immediately with `503 no capacity`); set
`max_concurrent` to `0` for unlimited instances.

## Idle reaping

If an instance receives no real client input for `idle_timeout`, the broker
releases it through the same teardown path as `POST /release` (so the session is
persisted). Only inbound `io` input frames (client → instance) reset the idle
timer — output, pings, and control frames do not. Set `idle_timeout` to `0` to
disable idle reaping.

## v1 caveats

The session broker is a **v1**. Understand these boundaries before deploying it:

- **No auth / no access control.** The broker trusts every caller. Tenant
  isolation is **process isolation, not access control** — any client that can
  reach the broker can claim, connect to, and release any instance. Front it
  with your own authenticating reverse proxy; treat the broker's listen address
  as a trusted-caller boundary only.
- **Single broker, single host.** No clustering or HA. A broker **restart
  orphans running instances** and loses all lease tracking — orphaned `nexus`
  processes must be cleaned up manually.
- **Cold-spawn per claim.** There is no pre-warm pool, so each claim pays full
  engine boot latency before the instance signals ready.
- **No OS-level per-tenant sandboxing.** Instances are separate processes but
  are not otherwise sandboxed from each other or the host beyond what the OS
  user provides.

## See also

- [`nexus.io.broker` plugin](../plugins/io/broker.md) — the dial-back transport
  inside each instance.
- [Configuration Reference](../configuration/reference.md#session-broker-nexus-broker)
  — authoritative broker + plugin config keys.
- [Sessions](../architecture/sessions.md) — on-disk session layout and `-recall`.
