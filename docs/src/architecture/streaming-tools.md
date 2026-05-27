# Streaming Tools

A standard Nexus tool is request-response: an agent emits `tool.invoke`,
the tool runs, the tool emits `tool.result`. For long-running operations
that produce incremental output, the agent has to wait for the whole call
to complete — UIs and observability collectors see nothing in between.

`pkg/streamtool` defines the contract a tool implements when it wants to
publish intermediate output while it runs.

## When to use it

A tool should be channel-aware when:

- The work takes more than a few seconds.
- It produces meaningful intermediate output a consumer might render
  (token stream, report sections, file-by-file progress).
- It can be cancelled cleanly.

For quick request-response operations, the standard tool interface is fine.

## Contract

```go
type ChannelTool interface {
    Name() string
    Stream(ctx context.Context, input map[string]any) (<-chan ToolEvent, error)
}

type ToolEvent struct {
    Kind     Kind   // Progress / Partial / Complete / Error
    Sequence int    // Monotonic per Stream invocation, starts at 1
    Payload  any
    Progress float64 // 0.0–1.0 if known, -1 otherwise
    Err      error   // Set on KindError
}
```

The tool owns the channel's lifetime: it must close the channel when work
completes or `ctx` cancels, and it must end the stream with `KindComplete`
or `KindError`.

## Bridge

`streamtool.Bridge(ctx, bus, tool, call)` drains the channel and projects
each event onto the bus:

- `KindProgress` → `tool.stream.progress`
- `KindPartial` → `tool.stream.partial`
- `KindComplete` → final `tool.result` carrying the payload
- `KindError` → final `tool.result` with the error

All projected envelopes inherit the originating `tool.invoke`'s ID as
their [`Causation.ParentID`](./causation.md) automatically — the bus's
per-goroutine dispatch context handles the propagation. UIs subscribed to
`tool.stream.partial` for live rendering, observability collectors
shipping the stream to Otel, and the parent agent's `tool.result` handler
all see the same call linked through causation.

`Bridge` blocks until the channel closes or `ctx` cancels and returns nil
on graceful completion, the stream's error on `KindError`.

## Wiring it into a plugin

A tool plugin keeps its `Init` and tool registration unchanged. In the
`tool.invoke` handler it instantiates a `ChannelTool`, hands it to
`Bridge`, and lets Bridge emit the final `tool.result` instead of doing
that itself:

```go
func (p *Plugin) onToolInvoke(ev engine.Event[any]) {
    call, _ := ev.Payload.(events.ToolCall)
    if call.Name != p.toolName {
        return
    }
    go func() {
        _ = streamtool.Bridge(context.Background(), p.bus, &myStreamingTool{...}, call)
    }()
}
```

The plugin still declares `tool.stream.progress`, `tool.stream.partial`,
and `tool.result` in its `Emissions()`.

## Events

See [`tool.stream.*` events](../events/reference.md#tool-stream-events) in
the events reference.
