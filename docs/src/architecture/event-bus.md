# Event Bus

The event bus is the central nervous system of Nexus. Every plugin communicates exclusively through it — emitting events when something happens, and subscribing to events it cares about.

## Interface

```go
type EventBus interface {
    Emit(eventType string, payload any) error
    EmitEvent(event Event[any]) error
    EmitAsync(eventType string, payload any) <-chan error
    Subscribe(eventType string, handler HandlerFunc, opts ...SubscribeOption) (unsubscribe func())
    SubscribeAll(handler HandlerFunc) (unsubscribe func())
    EmitVetoable(eventType string, payload any) (VetoResult, error)
    Drain(ctx context.Context) error
}
```

## Events

Every event is a typed container with metadata:

```go
type Event[T any] struct {
    Type      string    // Dotted namespace (e.g., "llm.request")
    ID        string    // Random hex identifier
    Timestamp time.Time // When the event was created
    Source    string    // Plugin ID that emitted this event
    Payload   T         // The event-specific data
}
```

Event types follow a dotted namespace convention:

| Prefix | Domain |
|--------|--------|
| `core.*` | Engine lifecycle (boot, ready, shutdown, tick, error) |
| `io.*` | User input/output, approvals, status |
| `llm.*` | LLM requests, responses, streaming |
| `tool.*` | Tool invocation and results |
| `agent.*` | Agent turns, plans, subagent lifecycle |
| `memory.*` | Conversation storage, queries, compaction |
| `skill.*` | Skill discovery, activation, resources |
| `session.*` | Session file events |
| `plan.*` | Planning requests, results, progress |
| `cancel.*` | Cancellation requests and coordination |
| `thinking.*` | Thinking step persistence |

## Subscribing to Events

Plugins declare their subscriptions in the `Subscriptions()` method:

```go
func (p *MyPlugin) Subscriptions() []engine.EventSubscription {
    return []engine.EventSubscription{
        {EventType: "io.input", Priority: 50},
        {EventType: "tool.result", Priority: 50},
    }
}
```

Or subscribe dynamically during `Init()`:

```go
func (p *MyPlugin) Init(ctx engine.PluginContext) error {
    ctx.Bus.Subscribe("some.event", p.handleEvent, engine.WithPriority(10))
    return nil
}
```

### Subscribe Options

| Option | Description |
|--------|-------------|
| `WithPriority(int)` | Execution order — lower values run first. Default is 0. |
| `WithFilter(EventFilter)` | Predicate function that must return `true` for the handler to fire |
| `WithSource(pluginID)` | Tag the subscription with the subscribing plugin's ID |

### Priority Ordering

Handlers for the same event type execute in priority order (ascending). This is how the system ensures, for example, that the LLM provider processes requests before observers log them.

Common conventions:
- **5–10** — High priority (providers, cancellation handlers)
- **50** — Normal priority (most plugins)
- **90** — Low priority (observers, persistence)

### Wildcard Subscriptions

`SubscribeAll()` registers a handler that receives every event, regardless of type. This is used by the event logger to capture all activity:

```go
ctx.Bus.SubscribeAll(func(event engine.Event[any]) {
    // Logs every event in the system
})
```

## Emitting Events

Plugins emit events by calling `Emit()` with a type string and payload:

```go
ctx.Bus.Emit("tool.result", events.ToolResult{
    ID:     callID,
    Name:   "shell",
    Output: output,
})
```

Plugins must declare all event types they may emit in the `Emissions()` method:

```go
func (p *MyPlugin) Emissions() []string {
    return []string{"tool.result", "tool.register", "core.error"}
}
```

## Async Emit

`EmitAsync()` dispatches an event in a separate goroutine, returning immediately with a channel that receives nil on success or an error:

```go
ch := ctx.Bus.EmitAsync("llm.request", request)
// ... do other work ...
if err := <-ch; err != nil {
    // handle error
}
```

Handlers still run synchronously within the goroutine — `EmitAsync` only makes the *dispatch* non-blocking relative to the caller. Used by the fanout plugin to send parallel requests to multiple providers.

## Vetoable Events

Events prefixed with `before:` support vetoing. This enables approval workflows — for example, the TUI can present an approval dialog before a tool runs.

```go
result, err := ctx.Bus.EmitVetoable("before:tool.invoke", toolCall)
if result.Vetoed {
    // Action was blocked
    fmt.Println("Vetoed:", result.Reason)
    return
}
// Proceed with the action
ctx.Bus.Emit("tool.invoke", toolCall)
```

Handlers veto by modifying the payload:

```go
func (p *MyPlugin) handleBeforeToolInvoke(event engine.Event[any]) {
    vr := event.Payload.(*engine.VetoResult)
    vr.Vetoed = true
    vr.Reason = "User denied tool execution"
}
```

## Event Filters

Filters are predicate functions that gate handler execution:

```go
ctx.Bus.Subscribe("llm.response", p.handleResponse,
    engine.WithPriority(10),
    engine.WithFilter(func(meta engine.EventMeta) bool {
        return meta.Source == "nexus.llm.anthropic"
    }),
)
```

## Draining

`Drain()` waits for all in-flight events to complete. This is used during shutdown to ensure no events are lost:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
bus.Drain(ctx)
```

## Thread Safety

The event bus is safe for concurrent use. Handler registration and event dispatch use read-write locks. Handler slices are copied before dispatch to allow concurrent emits without blocking.
