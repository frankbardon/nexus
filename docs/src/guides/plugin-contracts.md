# Plugin Contract Tests

Every Nexus plugin declares two event-contract methods on the `Plugin` interface:

```go
Subscriptions() []EventSubscription
Emissions()     []string
```

These declarations are used by the lifecycle manager for ordering and by observability tooling for plugin manifests. Without tests, nothing prevents a plugin from emitting an event type it never declared, or from declaring a subscription it never wires up. The **contract harness** in `pkg/testharness/contract/` makes those assertions cheap to write.

It's a separate, lighter wrapper than the integration harness in `pkg/testharness/`. The contract harness boots one plugin in isolation against a real `engine.Bus` plus a minimal `PluginContext` — temp data dirs, default host sandbox, optional session workspace. No engine `Boot`, no other plugins, no full session.

This guide is for unit-level contract assertions. For end-to-end agent-loop tests with multiple plugins active, see [Integration Testing](integration-testing.md) instead.

## When to use it

- Every new plugin should land with a `contract_test.go` (or `plugin_test.go`) that asserts its declared `Subscriptions()` and `Emissions()`.
- Use it for tests that drive the plugin's handlers via scripted bus events and assert which events come back out.
- Don't use it for tests of internal helpers that don't touch the bus — those live in regular `_test.go` files.

## Quick start

```go
package mygate

import (
    "testing"

    "github.com/frankbardon/nexus/pkg/events"
    "github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
    h := contract.NewContract(t, New)

    // 1. Static contract — declared sub/emit set.
    h.AssertSubscribesTo("before:io.output")
    if got := h.Plugin().Emissions(); len(got) != 1 || got[0] != "io.output" {
        t.Errorf("Emissions() = %v, want [io.output]", got)
    }

    // 2. Behavioral contract — drive the handler and assert.
    h.InjectVetoable("before:io.output", &events.AgentOutput{
        Role:    "assistant",
        Content: "this output is too long for the configured limit",
    })
    h.AssertEmitted("io.output")           // gate emits its system warning
    h.AssertNoUndeclaredEmissions()        // nothing outside the declared set
}
```

Cleanup is registered with `t.Cleanup` automatically. The harness drains the bus and calls `Shutdown` for you.

## API

```go
func NewContract(t *testing.T, factory engine.PluginFactory, opts ...ContractOption) *ContractHarness
```

Constructs the harness, calls `Init` and `Ready` on the plugin, registers cleanup. Fails the test on any error from those steps.

### Options

| Option | Effect |
|--------|--------|
| `WithPluginConfig(map[string]any)` | YAML-derived config map the plugin would normally receive. |
| `WithPluginID(string)` | Override the plugin ID (use for instance-suffixed IDs like `nexus.agent.subagent/researcher`). Defaults to `plugin.ID()`. |
| `WithSession()` | Boot with a real `SessionWorkspace` rooted in a temp dir. Enables plugins that touch `ctx.Session`, `ctx.DataDir`, or `ScopeSession` storage. Off by default to keep tests fast. |
| `WithLogger(*slog.Logger)` | Override the default discard logger. |

### Driving events

| Method | Purpose |
|--------|---------|
| `Inject(eventType, payload)` | Emit a normal event on the harness bus. The harness tags it as `OriginInject` so it's filtered out of plugin-emission checks. |
| `InjectVetoable(eventType, payload) VetoResult` | Emit a `before:*` event and return the resulting `VetoResult`. The vetoable wrapper protocol is handled for you. |

### Assertions

| Method | Asserts |
|--------|---------|
| `AssertSubscribesTo(types ...string)` | Plugin's static `Subscriptions()` declaration includes every type. Doesn't run the plugin — pair with `Inject` to verify the subscription actually fires. |
| `AssertEmitted(eventType)` | At least one plugin-origin event of this type was captured. |
| `AssertNotEmitted(eventType)` | No plugin-origin event of this type was captured. |
| `AssertEmittedInOrder(types ...string)` | Types appeared in the captured stream in the given relative order. Other emissions between them are ignored. |
| `AssertNoUndeclaredEmissions()` | Every plugin-origin event the harness saw is in the plugin's declared `Emissions()` list. Use after `Inject` to catch contract drift. |

`Captured()` returns every event observed (including injects); `PluginEmissions()` filters down to plugin-origin only.

## Patterns

### Plugin that mutates a request payload (no emissions)

Many plugins (embeddings adapters, rerankers, metadata router) subscribe to a request event and mutate the payload pointer in place rather than emitting a result. Their `Emissions()` is empty by design.

```go
func TestContract(t *testing.T) {
    h := contract.NewContract(t, New)
    h.AssertSubscribesTo("embeddings.request")

    req := &events.EmbeddingsRequest{Texts: []string{"foo"}}
    h.Inject("embeddings.request", req)

    if req.Provider != "nexus.embeddings.mock" {
        t.Errorf("provider not stamped: %q", req.Provider)
    }
    if got := h.Plugin().Emissions(); len(got) != 0 {
        t.Errorf("expected empty Emissions(), got %v", got)
    }
}
```

### Plugin that needs a session workspace

Plugins that persist files (longterm memory, planners, fileio) require `ctx.Session` to be non-nil. Pass `WithSession()`:

```go
h := contract.NewContract(t, New,
    contract.WithSession(),
    contract.WithPluginConfig(map[string]any{
        "scope":     "global",
        "auto_load": false,
    }),
)
```

The session workspace is rooted in `t.TempDir()` and cleaned up automatically.

### Plugin that needs an API key in config

Any plugin whose `Init` hard-rejects on missing credentials needs a stub key in test config:

```go
h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
    "api_key": "sk-mock-not-used",
}))
```

The harness never makes outbound HTTP calls during contract tests — the key just satisfies validation.

### Plugin that fails `Init` for negative-path tests

`NewContract` calls `t.Fatalf` on `Init` errors. To assert that `Init` correctly rejects bad config, bypass the harness and call `Init` directly:

```go
func TestContract_NoSteps_InitFails(t *testing.T) {
    p := New().(*Plugin)
    err := p.Init(engine.PluginContext{
        Bus:    engine.NewEventBus(),
        Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
        Config: map[string]any{}, // no required steps
    })
    if err == nil {
        t.Error("expected Init to fail when no steps configured")
    }
}
```

## Contract harness vs integration harness

| Aspect | `contract` harness | `testharness` (integration) |
|--------|--------------------|------------------------------|
| Scope | One plugin in isolation | Full engine boot |
| Build tag | None — runs in normal `go test` | `//go:build integration` |
| Plugins active | Just the one under test | Whatever the YAML config lists |
| Session | Optional via `WithSession()` | Always present |
| Mock LLM | Not applicable (plugin owns its bus) | Configured via `mock_responses` in YAML |
| Use for | Subscriptions/Emissions assertions, request-payload mutations | Multi-plugin agent loops, full event chains |

## Where it lives

The harness deliberately lives in `pkg/testharness/contract/`, not in `pkg/testharness/` itself. The integration harness imports `pkg/engine/allplugins` (which imports every plugin); putting contract code in the same package would create a cycle whenever a plugin imports the harness:

```
plugin → pkg/testharness → pkg/engine/allplugins → plugin
```

Splitting into a sub-package breaks the cycle.

## Reference

- Source: `pkg/testharness/contract/contract.go`
- Self-tests: `pkg/testharness/contract/contract_test.go`
- Examples: every `contract_test.go` and `plugin_test.go` under `plugins/`.
