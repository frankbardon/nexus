# Event Bus Tap

Streams the event bus to consumers outside the process over Server-Sent
Events. Use it when something external (a dashboard, a debugger, a log
shipper, another service) needs to see what Nexus is doing as it happens,
without being compiled in as a plugin.

The tap is **read-only**. There is no inbound path, it emits nothing on the
bus, and it cannot veto. Wildcard handlers run after every typed handler, so
`before:*` events reach the tap with their outcome already decided: they are
reported with `vetoed` / `veto_reason`, never influenced.

## Details

| | |
|---|---|
| **ID** | `nexus.observe.tap` |
| **Dependencies** | None |
| **Activation** | Only when listed in `plugins.active` (it binds a listener) |

## Configuration

```yaml
plugins:
  active:
    - nexus.observe.tap

  nexus.observe.tap:
    bind: 127.0.0.1:8095
    exclude_events: [llm.stream.chunk]
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `bind` | string | `127.0.0.1:8095` | Listener address. Loopback by default. |
| `auth` | map | *(none)* | `pkg/nexusauth` validator chain, same shape as [`nexus.io.agui`'s `auth:`](../io-agui.md#authentication-auth). Absent admits every caller. |
| `include_events` | list | `[]` (all) | Operator ceiling: only these types are ever streamed. |
| `exclude_events` | list | `[]` | Operator ceiling: these types are never streamed. |
| `ring_size` | int | `8192` | Recent frames kept in memory for resume and ring backfill. |
| `client_buffer` | int | `1024` | Per-client queue depth before the client is disconnected. |
| `heartbeat_interval` | duration | `15s` | SSE keepalive comment interval; `0` disables. |

Patterns are an exact type or end in `*` for a prefix match (`llm.*`,
`before:*`, `*`). An event outside the ceiling is never marshaled, so
excluding high-volume types also saves the cost.

## Endpoints

| Route | Auth | Purpose |
|-------|------|---------|
| `GET /events` | yes, when `auth` is set | The stream. |
| `GET /healthz` | no | `{"ok":true,"epoch":"…","clients":N}` |

### Query parameters on `/events`

| Param | Meaning |
|-------|---------|
| `types` | Comma-separated include patterns (narrows the ceiling). |
| `exclude` | Comma-separated exclude patterns. |
| `backfill` | Where to start: `none` (default, live from now), `ring` (the in-memory ring, then live), `journal` (the whole session journal, then the ring, then live). |
| `since_seq` | With `ring` or `journal` backfill, skip events with bus seq ≤ this. |
| `last_event_id` | Same as the `Last-Event-ID` header, for clients that cannot set headers. |

```bash
# Everything from now on
curl -N http://127.0.0.1:8095/events

# The whole session so far, then live, tools and LLM only
curl -N 'http://127.0.0.1:8095/events?backfill=journal&types=tool.*,llm.*'
```

## Wire format

Each event is one SSE frame whose `data` is the same
[journal envelope](../../architecture/causation.md) that
`<session>/journal/events.jsonl` stores, so the file and the stream parse
one shape:

```
id: 4f1c…:812
data: {"seq":640,"ts":"…","type":"tool.invoke","event_id":"…","source":"nexus.agent.react","parent_seq":638,"session_id":"…","side_effect":true,"payload":{…}}
```

Envelope frames carry no `event:` line, so a browser `EventSource.onmessage`
receives the whole firehose. Control frames are named events:

| Event | Data | Meaning |
|-------|------|---------|
| `gap` | `{"reason":…}` | Something requested cannot be served. Reasons: `epoch_changed` / `resume_point_evicted` (Last-Event-ID unusable; the requested `backfill` runs instead), `journal_unavailable`, `journal_read_error`, `not_yet_journaled` (with `after_seq` / `through_seq`: seqs in that range may be missing). |
| `overflow` | `{"last_event_id":…}` | This client fell `client_buffer` frames behind and is being disconnected. Reconnect with that id. |

### Order, ids and resume

Frames arrive in **dispatch-completion order, not seq order**. The bus runs
wildcard handlers after typed ones, so a nested event reaches the tap before
the event whose handler emitted it, and concurrent emitters interleave. The
journal writer reorders by seq because it is a durable log; the tap does not,
because holding children for their parent would buffer every
`llm.stream.chunk` until the `llm.request` that produced it returned. Sort by
`seq` (or rebuild the tree from `parent_seq`) if you need causal order.

The SSE `id` is therefore not the seq but `<epoch>:<cursor>`: a tap-local
delivery position that is exact in the order frames were sent. `Last-Event-ID`
resumes precisely from it while the ring still holds the next frame. The epoch
changes every process start (including a broker re-spawn), so an id from an
earlier process reports `gap` / `epoch_changed` and falls back to the
requested `backfill`.

Journal backfill frames carry no `id:`. A client that drops mid-backfill keeps
its previous id and so re-runs the backfill rather than resuming into its
middle.

### Journal backfill and its gap

The journal flushes on turn boundaries by default (`journal.fsync:
turn-boundary`), so the file routinely trails the bus by the current turn. The
ring covers that tail and the two are de-duplicated by seq. A `gap` /
`not_yet_journaled` frame appears only when events are in neither: they fell
off the ring (or predate the tap's subscription beyond the bus's 64-event
replay ring) and the journal has not flushed them yet. Raise `ring_size` if
you see it.

## Backpressure

Dispatch never waits on a consumer. Each client has a bounded queue; one that
fills is dropped with an `overflow` frame after its already-queued frames are
written. Excluded and filtered events never take queue space.

## Security

Payloads are unredacted: `llm.request` carries full prompts, `tool.invoke` full
arguments. The tap is meant for a trusted consumer. Keep it on loopback, or set
`auth:` before binding it anywhere else.

## Events

### Subscribes To

Uses `SubscribeAllReplay()` at Init, so boot-time events still in the bus ring
are included.

### Emits

None.
