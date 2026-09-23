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

## Conformance corpora: when two plugins must agree on a wire format

The contract harness pins one plugin against the bus. A different problem shows
up when **two plugins independently produce the same external format** and
nothing in the type system makes them agree — they cannot call each other (the
no-plugin-to-plugin-calls rule), so the only thing keeping them aligned is that
someone remembers to change both.

The answer is a **shared, test-only conformance corpus**: one package holding
the canonical vectors, the named invariants and the checking, which each plugin
imports from its own `_test.go` and drives with its own code. Neither plugin
imports the other; both import the corpus.

Three ship today:

| Corpus | Holds honest | Consumers |
|---|---|---|
| `pkg/a2a/a2aconform` | the A2A frame stream | `nexus.io.a2a`, `cmd/nexus-broker` |
| `pkg/engine/objectstore/objectstoretest` | the object-store backend contract | every `objectstore.Backend`, in-tree and out |
| `pkg/openaiconform` | the OpenAI Responses request body and reply parse | `nexus.llm.openai`, `nexus.llm.batch` |

They share a shape worth copying:

- **Vectors are data, not Go.** JSON documents, embedded with `go:embed` and
  decoded strictly, so a typo is a failure rather than an expectation that
  silently never runs — and so the corpus is readable by a reviewer who does not
  read Go.
- **Each vector carries a rationale**, printed on failure. The first instinct on
  a red conformance test is to change the expectation, and the rationale is what
  argues back.
- **The whole output is the expectation, not a list of spot checks.** The
  realistic drift is a *key-set* divergence — one side gains a field, or stops
  writing one — and every value check ever written still passes through it.
  `openaiconform.CheckBody` therefore compares key sets in both directions and
  reports `UNEXPECTED KEY` / `MISSING` by path.
- **Legitimate differences are an explicit allowlist**, enforced both ways. A
  key declared specific to one surface is *required absent* on the others, so
  "the other plugin quietly grew this field" fails as loudly as "this plugin
  stopped writing it". Deleting a key from a vector to make a run green is not
  a substitute, and `openaiconform` refuses a vector that tries.
- **The oracle has its own tests.** `check_test.go` feeds the pure checker
  deliberately-wrong bodies and asserts it rejects them, naming the invariant.
  A harness nobody has watched fail is not evidence.

### The OpenAI Responses corpus

`plugins/providers/openai` and `plugins/llm/batch` each build a
`/v1/responses` body and parse a `/v1/responses` reply, with no shared code: the
provider's builders are unexported methods on plugin state (auth mode,
multimodal config, prompt registry) the batch coordinator neither has nor
should have. A wire fix applied to one and not the other does not fail a build —
it surfaces as an HTTP 400 on batched traffic only, months later.

`pkg/openaiconform` pins both against one corpus. The invariants it asserts by
name are the ones the Responses migration settled: `store: false`; an explicit
`strict: false` on every function tool (an *absent* `strict` attempts strict
mode on this API, the reverse of Chat Completions); `max_output_tokens` not
`max_tokens`; `input` not `messages`; `text.format` not `response_format`;
`model` present on every request including Azure modes, where the chat path
strips it; a `reasoning` object rather than the chat scalar, applied last so it
strips the sampling parameters a reasoning model rejects. The reply half pins
text, tool calls keyed by `call_id`, the five usage counters and the
finish-reason translation, plus that a run which failed inside an HTTP 200 is
reported as a failure rather than as an empty success.

Scope is what **both** surfaces claim to serialize. The provider-only halves —
multimodal Items, prompt decoration, `tool_choice`, tool filtering, reasoning-Item
replay, predicted-output degradation — stay in that package's own tests, because
a vector exercising one would be testing a single plugin.

The drivers are `plugins/providers/openai/conform_test.go` and
`plugins/llm/batch/conform_test.go`. Adding a vector means adding a JSON file;
both suites pick it up with no code change.

## Reference

- Source: `pkg/testharness/contract/contract.go`
- Self-tests: `pkg/testharness/contract/contract_test.go`
- Examples: every `contract_test.go` and `plugin_test.go` under `plugins/`.
- Conformance corpora: `pkg/a2a/a2aconform`,
  `pkg/engine/objectstore/objectstoretest`, `pkg/openaiconform` (drivers in
  `plugins/providers/openai/conform_test.go` and
  `plugins/llm/batch/conform_test.go`).
