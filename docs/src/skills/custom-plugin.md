# Creating a Custom Plugin

This guide walks through creating a new Nexus plugin from scratch.

## Plugin Template

Create a new package under `plugins/`:

```
plugins/
  mycat/
    mycat.go        # Main plugin file
    mycat_test.go   # Tests
```

### Minimal Plugin

```go
package mycat

import (
    "context"
    "log/slog"

    "github.com/frankbardon/nexus/pkg/engine"
)

const pluginID = "nexus.mycat"

type Plugin struct {
    bus    engine.EventBus
    logger *slog.Logger
}

func New() engine.Plugin {
    return &Plugin{}
}

func (p *Plugin) ID() string      { return pluginID }
func (p *Plugin) Name() string    { return "My Category Plugin" }
func (p *Plugin) Version() string { return "0.1.0" }

func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
    p.bus = ctx.Bus
    p.logger = ctx.Logger
    // Read config from ctx.Config
    // Set up subscriptions
    return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(ctx context.Context) error { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
    return []engine.EventSubscription{
        {EventType: "io.input", Priority: 50},
    }
}

func (p *Plugin) Emissions() []string {
    return []string{"io.output"}
}
```

## Reading Configuration

Plugin config comes as `map[string]any` from the YAML:

```go
func (p *Plugin) Init(ctx engine.PluginContext) error {
    // Read a string config value
    if v, ok := ctx.Config["my_setting"].(string); ok {
        p.mySetting = v
    }

    // Read an int (YAML numbers may come as float64)
    if v, ok := ctx.Config["max_items"].(int); ok {
        p.maxItems = v
    } else if v, ok := ctx.Config["max_items"].(float64); ok {
        p.maxItems = int(v)
    }

    // Read a bool with default
    p.enabled = true
    if v, ok := ctx.Config["enabled"].(bool); ok {
        p.enabled = v
    }

    return nil
}
```

## Subscribing to Events

### Via Subscriptions() (preferred for static subscriptions)

```go
func (p *Plugin) Subscriptions() []engine.EventSubscription {
    return []engine.EventSubscription{
        {EventType: "io.input", Priority: 50},
        {EventType: "tool.result", Priority: 50},
    }
}
```

The lifecycle manager will wire these up automatically and call the handler. You'll need to implement event routing in your handler.

### Via Bus.Subscribe() (for dynamic subscriptions)

```go
func (p *Plugin) Init(ctx engine.PluginContext) error {
    ctx.Bus.Subscribe("custom.event", p.handleCustomEvent,
        engine.WithPriority(50),
        engine.WithSource(pluginID),
    )
    return nil
}

func (p *Plugin) handleCustomEvent(event engine.Event[any]) {
    // Handle the event
}
```

## Emitting Events

```go
// Simple emit
p.bus.Emit("my.event", MyPayload{
    Field: "value",
})

// Vetoable emit (for before:* events)
result, err := p.bus.EmitVetoable("before:my.action", &engine.VetoResult{})
if result.Vetoed {
    p.logger.Info("action vetoed", "reason", result.Reason)
    return
}
```

## Creating a Tool Plugin

Tool plugins register themselves and handle invocations:

```go
func (p *Plugin) Init(ctx engine.PluginContext) error {
    p.bus = ctx.Bus
    p.logger = ctx.Logger

    // Subscribe to tool invocations
    ctx.Bus.Subscribe("tool.invoke", p.handleInvoke, engine.WithPriority(50))

    return nil
}

func (p *Plugin) Ready() error {
    // Register the tool
    p.bus.Emit("tool.register", events.ToolDef{
        Name:        "my_tool",
        Description: "Does something useful",
        Parameters:  `{"type":"object","properties":{"input":{"type":"string","description":"The input"}},"required":["input"]}`,
    })
    return nil
}

func (p *Plugin) handleInvoke(event engine.Event[any]) {
    call, ok := event.Payload.(events.ToolCall)
    if !ok || call.Name != "my_tool" {
        return
    }

    input, _ := call.Arguments["input"].(string)

    // Do something with input
    result := processInput(input)

    p.bus.Emit("tool.result", events.ToolResult{
        ID:     call.ID,
        Name:   call.Name,
        Output: result,
        TurnID: call.TurnID,
    })
}
```

## Using the Session Workspace

```go
func (p *Plugin) Init(ctx engine.PluginContext) error {
    // Get plugin-specific data directory
    dataDir := ctx.DataDir // ~/.nexus/sessions/<id>/plugins/<plugin-id>/

    // Or use session directly
    ctx.Session.WriteFile("plugins/"+pluginID+"/state.json", data)

    return nil
}
```

## Using the Prompt Registry

```go
func (p *Plugin) Init(ctx engine.PluginContext) error {
    ctx.Prompts.Register("my-context", 50, func() string {
        if p.hasContext {
            return "## My Context\n" + p.contextData
        }
        return "" // Empty string = section omitted
    })
    return nil
}
```

## Using the Model Registry

```go
func (p *Plugin) Init(ctx engine.PluginContext) error {
    // Resolve a model role
    cfg, found := ctx.Models.Resolve("reasoning")
    if found {
        p.logger.Info("using model", "model", cfg.Model, "provider", cfg.Provider)
    }
    return nil
}
```

## Registering the Plugin

Add your plugin to `cmd/nexus/main.go`:

```go
import "github.com/frankbardon/nexus/plugins/mycat"

// In main():
eng.Registry.Register("nexus.mycat", mycat.New)
```

Then activate it in your config:

```yaml
plugins:
  active:
    - nexus.mycat

  nexus.mycat:
    my_setting: "value"
    max_items: 10
```

## Testing

Use the standard Go testing framework. You can create a test EventBus for unit tests:

```go
func TestPlugin(t *testing.T) {
    bus := engine.NewEventBus()
    p := New()

    err := p.Init(engine.PluginContext{
        Config: map[string]any{"my_setting": "test"},
        Bus:    bus,
        Logger: slog.Default(),
    })
    if err != nil {
        t.Fatal(err)
    }
}
```

## Conventions

- **Plugin ID**: `nexus.<category>.<name>` (e.g., `nexus.tool.mytool`)
- **Logging**: Use the provided `slog.Logger`, not `fmt.Println`
- **Error wrapping**: Use `fmt.Errorf("context: %w", err)`
- **No direct plugin-to-plugin calls**: Always communicate through events
- **Declare all emissions**: List every event type in `Emissions()`
