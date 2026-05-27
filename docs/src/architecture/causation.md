# Causation

Every dispatched event in Nexus carries a `Causation` block that records its
provenance: the parent event that caused it, the session it belongs to, the
agent that produced it, and a monotonic per-session sequence number. The bus
populates these fields automatically — plugin authors don't have to remember
to set them.

## Why

Causation is the substrate the [Replay primitive](./replay.md) walks, the
attribution observability collectors filter on, and the dimension the
[Sub-agent delegation](./delegate.md) runtime uses to distinguish a parent's
work from a specialist's. Once it's present on every event, you can:

- Walk an entire session's causation DAG to debug what happened.
- Filter envelopes by `AgentID` to inspect just one specialist's work.
- Branch and fork: replay to a sequence, then continue along a different path.

## Schema

```go
type EventCausation struct {
    ParentID  string // EventID of the event whose handler emitted this one
    ParentSeq uint64 // Mirrors ParentID via the per-session monotonic sequence
    SessionID string // Session this event belongs to
    AgentID   string // Agent that produced it (sub-agent identity for delegate work)
    Sequence  uint64 // Monotonic per session; assigned at dispatch
    Depth     int    // Sub-agent recursion depth at emission time
}
```

`Causation` lives on both `Event[T]` and `EventMeta`. Wildcard subscribers
and filters see it without unwrapping the payload.

## How it's filled

Three sources, in priority order:

1. **Caller-set fields win.** Replay tools and sub-agent runtimes that need
   to override the auto-derived values do so by populating `Causation` on
   the `Event[any]` they pass to `EmitEvent`. The bus respects any
   non-zero / non-empty field.
2. **Dispatch stack** supplies `ParentID` and `ParentSeq`. The bus tracks
   the in-flight event per goroutine; anything emitted inside that
   goroutine's handler chain inherits the in-flight event as its parent.
3. **Causation context** supplies `SessionID`, `AgentID`, `Depth`. Two
   sources here:
   - `PushCausationContext(c) func()` — per-goroutine stack pushed by
     callers that have explicit knowledge of who they're running for
     (sub-agent dispatch, IO transports binding to a session).
   - `SetDefaultCausationContext(c)` — bus-wide fallback applied when the
     calling goroutine has nothing pushed. `Engine.StartSession` installs
     the `SessionID` here so every dispatched event carries session
     attribution even when emitters never call `PushCausationContext`.

## Pushing context

The typical pattern: push at the start of a scoped operation, defer the pop.

```go
if cc, ok := bus.(engine.CausationController); ok {
    pop := cc.PushCausationContext(engine.CausationContext{
        AgentID: "delegate/analyst/" + subSessionID,
        Depth:   parentDepth + 1,
    })
    defer pop()
}
// Every event emitted from this goroutine until pop() carries the AgentID
// and Depth above.
```

`CausationController` is an optional interface — checking the assertion at
call sites keeps embedders using a custom bus implementation untouched.

## Journal

The `journal.Envelope` written by `pkg/engine/journal` carries `Seq`,
`ParentSeq`, `ParentID`, `SessionID`, `AgentID`, and `Depth` alongside the
payload, so downstream replay (`pkg/replay`) and projection tools can
reconstruct the full causation DAG without re-deriving anything.

## Excluded events

Events on the journal exclusion set (`core.tick` by default) skip seq
assignment, dispatch-stack tracking, and the replay ring — and therefore
have `Sequence = 0` and no `ParentSeq` / `ParentID`. They still carry the
default `SessionID` / `AgentID` from the causation context so observability
tooling can attribute the heartbeats.
