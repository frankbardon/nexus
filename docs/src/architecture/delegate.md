# Sub-agent Delegation

Delegation is the first-class operation a parent agent uses to call another
agent with a different reasoning posture, system prompt, allowed-tools
subset, and resource budget. From the parent's perspective a delegate call
is a single tool invocation; underneath, the runtime spawns a sub-session
that has its own context window, its own envelope identity
([`Causation.AgentID` and `Depth`](./causation.md)), and its own budget.

The runtime lives at `pkg/delegate.Runtime`; the tool surface is the
`nexus.agent.delegate` plugin.

## Lifecycle of a call

1. The parent's LLM emits a `delegate` tool call.
2. The plugin resolves the posture by name through the
   [posture registry](./postures.md).
3. The runtime checks recursion depth (per-posture `max_recursion_depth`
   first, then the global `MaxDepth`).
4. The runtime computes a [cache key](#caching). On a hit, the cached
   `Output` returns immediately as `StatusCacheHit` — no model calls, no
   tool calls, no budget consumption.
5. The runtime pushes a `CausationContext` carrying the sub-agent's
   `AgentID` and `Depth`, then enters the isolated LLM loop.
6. Each iteration emits an `llm.request` tagged with the sub-session's
   source, collects the response, runs any tool calls (filtered to the
   posture's `AllowedTools`), and appends results to the sub-session's
   history.
7. The loop exits on a tool-call-free response (`StatusSuccess`), budget
   exhaustion (`StatusPartial`), error (`StatusError`), timeout
   (`StatusTimeout`), or ctx cancel (`StatusCancel`).
8. Successful and partial outputs are cached. The final `tool.result`
   returns the `Output` to the parent agent as JSON.

## Budgets

Budgets are non-negotiable, enforced by the runtime, and resolved per-call:

```go
type Overrides struct {
    MaxTokens    int
    MaxToolCalls int
    Timeout      time.Duration
}
```

Per-call overrides win when non-zero; otherwise the posture's
`DefaultBudget` applies. `Timeout` becomes a `context.WithTimeout` around
the loop. `MaxTokens` is checked after each LLM response and short-circuits
with `StatusPartial`. `MaxToolCalls` is checked before dispatching each
tool batch and likewise short-circuits with `StatusPartial`.

The parent receives the `Output.Status` and decides whether to retry with a
larger budget, fall back to handling the task itself, or surface the
partial result.

## Recursion

Sub-agents can themselves call `delegate`. Two caps gate the depth:

- The runtime's `MaxDepth` (default 3, configurable via the plugin's
  `max_depth`) is the global ceiling.
- Each posture may set `max_recursion_depth` to tighten the ceiling for
  itself.

Exceeding either cap returns `ErrRecursionLimit` (`StatusError`) without
spinning up a sub-session.

## Caching

The runtime's `Cache` interface (default: `MemoryCache`, an in-process LRU)
keys results on the SHA-256 of:

- Posture `Name`
- Posture `Version` (the content hash; edits invalidate cached results)
- The `Task` string
- The canonicalized `Context` map (keys sorted, values JSON-marshaled)
- The sorted `AllowedTools` list

Cache hits return `Status = cache_hit` and a fresh `SubSessionID`; `Elapsed`
reflects only the lookup time. Operators can plug in a Redis-backed cache
by implementing the `Cache` interface and assigning it on the runtime.

The cache is bypassed for errors and timeouts — operators want a retry to
re-execute, not replay a transient failure.

## Tool filtering

`AllowedTools` is a closed list. The runtime snapshots the live tool
catalog on every invocation, intersects it with `AllowedTools`, and offers
only the intersection to the sub-agent's LLM. An empty list means "all
tools the catalog currently advertises" — useful for trusted postures.

## Observability

Every call emits a `delegate.start` / `delegate.complete` pair on the bus;
see the [events reference](../events/reference.md#delegate-events). Every
LLM and tool envelope from inside the sub-session carries the sub-agent's
`AgentID` and `Depth` so observability tooling (otel spans, log shipping)
can attribute the work.

## Public API

```go
type Input struct {
    Posture     string
    Task        string
    Context     map[string]any
    ParentTurn  string
    ParentDepth int
    Overrides   Overrides
}

type Output struct {
    Result        string
    Status        Status // success / partial / error / timeout / cancelled / cache_hit
    Error         string
    TokensUsed    int
    ToolCallsUsed int
    Elapsed       time.Duration
    SubSessionID  string
    PostureName   string
    PostureVer    string
    Depth         int
}

func (r *Runtime) Run(ctx context.Context, in Input) (Output, error)
```

## Configuration

See [`nexus.agent.delegate`](../configuration/reference.md#nexusagentdelegate)
in the configuration reference.
