# Replay

Given a session ID, `pkg/replay` reconstructs the full causation DAG and
walks it. Replay is read-only and deterministic — it reads from the
durable journal and the scene patch journal; it does not re-run agents,
LLMs, or tools.

This is the foundation for debugging, audit trails, time-travel, and
reproducibility.

## API

```go
type Replay struct {
    SessionID string
    Events    []Event       // In seq order
    Scenes    []SceneSnap   // Scene state at the requested point in time
    LastSeq   uint64
}

type Event struct {
    Seq       uint64
    ParentSeq uint64
    ParentID  string
    EventID   string
    Type      string
    AgentID   string
    Depth     int
    Vetoed    bool
    Payload   any
}

type Options struct {
    SessionsRoot  string // Engine session root (typically ~/.nexus/sessions)
    AtSeq         uint64 // Stop after this seq; zero = full journal
    IncludeVetoed bool   // Keep vetoed before:* envelopes (default true)
}

func Session(ctx context.Context, sessionID string, opts Options) (Replay, error)
func SessionAt(ctx context.Context, sessionID string, atSeq uint64, opts Options) (Replay, error)
```

## Walking the DAG

The `Replay` value exposes three convenience walkers:

| Walker | Returns |
|--------|---------|
| `Roots()` | Events with `ParentSeq == 0` — operator-driven entry points like `io.session.start` and `io.input` arrivals. |
| `Children(seq)` | Events whose `ParentSeq` matches — the next layer of the DAG below a given node. |
| `ByAgent(id)` | Events whose `AgentID` matches — the value the [sub-agent `Causation.AgentID`](./causation.md) buys: filter the DAG to one specialist's work. |

For richer traversal, the flat `Events` slice is in seq order — most
custom walks are a single loop over it.

## Use cases

- **Debugging.** A user reports an issue with a session; `Session(ctx, id, opts)`
  reconstructs the events to read what happened.
- **Audit.** Compliance review walks the DAG to verify what the session
  did. `ByAgent` answers "what did the analyst posture decide?"
- **Time-travel.** `SessionAt(ctx, id, atSeq, opts)` rebuilds state as it
  looked at `atSeq`. Renderers consume the returned `Scenes` to show
  historical visual state.
- **Branch and fork.** Replay to a point, then resume the session along a
  different path with a new agent message. (The engine's existing rewind
  primitive consumes the DAG produced here.)

## Scene reconstruction

For every session whose `<session>/plugins/nexus.scene/scenes.jsonl`
exists, replay folds the JSONL stream through `scene.ShallowMerge` and
returns a `SceneSnap` per scene at the requested point in time. Sessions
without scenes return events without error.

The scene journal currently records its own per-scene mutation order, not
the bus seq — `AtSeq` filters by the number of scene-journal lines read,
not the bus dispatch seq. Improving this is a follow-up that adds a bus
seq to each scene journal line.

## Performance

For long sessions (thousands of events), full replay can be slow. The
engine's journal supports periodic snapshots in `pkg/engine/replay.go`
that replay can start from; integrating snapshot recovery into
`pkg/replay.Session` is a future optimization. Correctness does not
depend on snapshots.
