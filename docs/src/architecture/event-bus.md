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

Every event is a typed container with metadata and a `Causation` block that
records its provenance:

```go
type Event[T any] struct {
    Type      string         // Dotted namespace (e.g., "llm.request")
    ID        string         // Random hex identifier
    Timestamp time.Time      // When the event was created
    Source    string         // Plugin ID that emitted this event
    Payload   T              // The event-specific data
    Causation EventCausation // Auto-filled by the bus — see Causation, below
}
```

See [Causation](./causation.md) for the full discussion of how `ParentID`,
`SessionID`, `AgentID`, `Sequence`, and `Depth` are populated and how
plugins push their own `CausationContext`. The short version: the bus does
the bookkeeping. Plugin authors don't have to thread session identity
through every emit site.

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

### Mutating instead of vetoing

The payload is a pointer, so a handler can **rewrite it in place** and return
without vetoing. The emitter then publishes the edited payload. This is the
replace path, and it composes: a handler at a lower priority sees the previous
handler's edits. The shipped `before:io.output` pipeline relies on it —
`content_safety` redacts at priority 8, then `stop_words` ban-checks the
already-redacted text at 10.

Mutate to change *what* happens; veto to stop it happening at all.

### `before:llm.request`: every producer goes through the hop

`before:llm.request` is where the engine seeds the cost-attribution tags
(`session_id`, `tenant`, `project`, `user`) and where every request-side gate
and curator runs — rate limiter, token budget, tool filter, prompt-injection
and stop-word scans, dynamic variables, routing, stream policy. A request
emitted straight onto `llm.request` skips all of them: it is billed to no
session and no gate ever sees it.

**Handler order.** Lower priorities run first, and a veto stops dispatch, so
an interceptor hides the request from everything after it. The shipped map:

| Priority | Handler | Role |
|---|---|---|
| -1000 | engine tag seeder | Seeds `session_id`, `tenant`, `project`, `user` (set-if-absent). First, so every later handler — and every fanout leg — sees them. |
| 2 | `nexus.provider.fanout` | Intercepts fanout roles; legs copy the (already seeded) request. |
| 3 | `nexus.provider.fallback` | Snapshots the request for failover. |
| 5 | engine schema registry | Attaches `_expects_schema` structured-output schemas. |
| 6 | `nexus.gate.endless_loop` | Iteration counter. |
| 7 | `nexus.gate.token_budget` | Ceiling check + reservation (tenant ceilings read the seeded `tenant`). |
| 8 | `nexus.discovery.progressive` | Discovery tool projection. |
| 9 | `nexus.gate.rate_limiter` | Throttle. |
| 10 | `nexus.gate.tool_filter` | Tool allow/blocklist. |
| 11 | `nexus.gate.prompt_injection` | Input scan. |
| 12 | `nexus.gate.stop_words`, `nexus.memory.tool_result_clear` | Ban scan; stale tool-result clearing. |
| 13 | `nexus.gate.approval_policy` | Policy HITL approval. |
| 14 | `nexus.memory.tool_def_pruner` | Tool-definition pruning. |
| 15 | `nexus.gate.context_window`, `nexus.skills` | Context estimate / compaction trigger; skill schema tagging. |
| 20 | `nexus.system.dynvars`, `nexus.io.test` (mock mode) | Runtime-context block; mock interception. |
| 45 / 50 | `nexus.router.classifier` / `nexus.router.metadata` | Model routing. |
| 10000 | engine stream policy | Final word on `Stream`. |

So **every producer of an `llm.request` publishes through
`engine.PublishLLMRequest`** rather than calling `bus.Emit("llm.request", …)`
directly — agent loops, planners, routers, memory summarisers, gate retries,
provider failover, RAG contextualisation, HITL synthesis, and embedder code
alike:

```go
res := engine.PublishLLMRequest(p.bus, req)
if !res.AwaitResponse() {
    // Blocked outright: nothing will answer this request.
    return fmt.Errorf("llm.request vetoed: %s", res.Veto.Reason)
}
// Published (or intercepted): wait for the correlated llm.response.
```

It runs the hook, emits `llm.request` with whatever the handlers left in the
payload unless one vetoed, and returns an `engine.LLMRequestResult`.
`PublishLLMRequestAsync` is the same with the emission dispatched by
`EmitAsync`, for a producer that bounds the provider round-trip under its own
deadline (the fanout judge); the hop itself still runs synchronously.

A veto comes in two kinds, and a producer waiting on its response must tell
them apart:

| Result | Meaning | Producer should |
|---|---|---|
| `Published` | `llm.request` was emitted. | Wait for the response. |
| `Intercepted()` | A handler vetoed because it **owns the answer** and will publish the correlated `llm.response` itself. It marked the request with `engine.InterceptLLMRequest` (`Metadata["_intercepted_by"]`). | Keep waiting, as if published. |
| `Blocked()` | Any other veto. Nothing will answer. | Give up now — don't sit out a timeout. |

The two interceptors in tree are the fanout coordinator (dispatches to several
providers, publishes one merged response) and the test IO plugin's mock mode.
`AwaitResponse()` is `Published || Intercepted()`.

A producer that blocks on its response — `delegate.SyncLLM` (behind the
delegate runtime and the subagent worker loop) and the HITL synthesizer —
collects it with `engine.AwaitLLMResponse(ctx, res, ch)`, where `ch` is fed by
the producer's own `llm.response` subscription, set up before the publish:

```go
res := engine.PublishLLMRequest(bus, req)
resp, err := engine.AwaitLLMResponse(ctx, res, respCh)
```

A block veto fails at once with `engine.ErrLLMRequestBlocked`. A published
request is answered inside the synchronous emit or not at all
(`engine.ErrNoLLMResponse`). An intercepted one is answered asynchronously, so
the helper waits on `ctx` and returns `ctx.Err()` when it ends first — `ctx` is
the only bound, so pass one that ends. Correlate on `RequestID`: interceptors
carry it onto their response (fanout's merged response also restores the
request's own `_source` and other metadata, but each leg carries the fanout's
`_source`). Fanout legs keep the parent's `RequestID` too, so skip
any response for which `engine.IsFanoutLegResponse` is true
(`Metadata["_fanout_provider"]`, `engine.MetaFanoutProvider`).

**Recursion.** A `before:llm.request` handler that itself produces a request
re-enters the hook: the classifier router's probe, and compaction triggered by
the context-window gate's `memory.compact.request`. The convention that keeps
this finite: every internal request carries a non-empty `Metadata["_source"]`,
and every handler that emits from inside the hook skips requests that carry
one (the classifier also stamps and checks `_routed_by`; fanout uses
`_fanout_id`; failover uses `_fallback_id`). Gate retries are driven from the
output side (`before:io.output`), not from a request handler, so they cannot
loop through the hook.

**Which handlers see internal requests.** The classifier router skips any
non-empty `_source` (its recursion guard). `endless_loop` and
`approval_policy` skip requests for which `engine.IsInternalLLMRequest` is
true; `context_window` and `discovery.progressive` act only when
`engine.IsConversationLLMRequest` is true — not internal and not a nested
`subagent` / `delegate` loop, whose history and tool set are its caller's,
not the conversation's (discovery also leaves a request offering no tools
alone). `prompt_injection` and `stop_words` skip requests for which
`engine.IsInternalLLMRequest` is true — a non-empty `_source` whose `task_kind`
is not an agent-loop turn (`react_main`, `planexec_step`,
`planexec_synthesize`, `orchestrator_decompose`, `orchestrator_synthesize`,
`subagent`, `delegate`) — because the user text a summary quotes was already
scanned on the turn that carried it. Agent turns stamp `_source` too (cost
attribution), so a bare `_source != ""` test is not an internal-request test.
The rest (`rate_limiter`, `token_budget`, `dynvars`, `tool_filter`, the
metadata router, the curators and stream policy) apply to them like any other
request. A blocked internal request
fails fast: a gate retry returns invalid, a summariser or compaction releases
its in-flight flag and leaves history as-is, a fallback retry leaves the
original provider error to surface.

**Task kinds are engine constants.** Every `task_kind` value a producer
stamps is an exported constant in `pkg/engine/llm_request.go`, set under the
key `engine.MetaTaskKind`. Agent-loop turns: `TaskKindReactMain`,
`TaskKindPlanexecStep`, `TaskKindPlanexecSynthesize`,
`TaskKindOrchestratorDecompose`, `TaskKindOrchestratorSynthesize`, and the
nested loops `TaskKindSubagent` and `TaskKindDelegate`. Internal sub-flows:
`TaskKindPlan`, `TaskKindClassify`, `TaskKindSummarise`, `TaskKindCompaction`,
`TaskKindGateRetry`, `TaskKindFanoutJudge`, `TaskKindRAGContextualize` and
`TaskKindHITLSynthesize`. Producers and consumers (`IsInternalLLMRequest`'s
turn table, the memory plugins' `internalflow` skip list) both use the
constants, so a spelling cannot drift between the two sides; the values are
wire values (cost records, router rules on `metadata.task_kind`) and do not
change. A new kind gets a constant here, and an agent-loop kind must also be
added to the turn table or every gate treats its turns as internal.

**Every agent loop stamps `turn_id`.** `LLMRequest` has no `TurnID` field,
so the turn a request serves rides `Metadata[engine.MetaTurnID]`
(`"turn_id"`). `nexus.agent.react`, `nexus.agent.planexec` (step and
synthesis), `nexus.agent.orchestrator` (decompose and synthesis) and
`nexus.planner.dynamic` stamp their current turn on every `llm.request`
they publish. The nested loops — `nexus.agent.subagent` and the
`pkg/delegate` runtime — stamp the **parent** turn they serve, not their
private `subagent_<id>` / `delegate_<id>` turn: that id only correlates
the loop's own tool results and names no turn a transport ever saw
start. `nexus.gate.approval_policy` copies the key onto the
`HITLRequest.TurnID` an `llm.request` rule raises, and a turn-scoped
transport (`nexus.io.agui`) only resumes and finishes a run whose turn
it recognises (#218). Providers echo request metadata, so
`nexus.rag.citations` and `nexus.io.voice` read the same key off
`llm.response`. A new agent loop must stamp it too.

**The one exception: fanout legs.** The fanout coordinator's per-provider legs
are emitted directly with `EmitAsync`. Each leg is a copy of a request that is
already inside the hop (fanout's own `before:llm.request` handler); offering
every leg again would count one logical request N times against the rate
limiter and token budget under one shared `RequestID`. A leg inherits the parent's
tags (cloned per leg): the parent was seeded before fanout saw it, so legs are
billed to the same session and tenant.

### `before:llm.response`: veto means substitute

`before:llm.response` is the one vetoable event where a veto does **not** block
the action. Agent loops (react, planexec, orchestrator) subscribe to
`llm.response` and block their turn waiting for it, so suppressing the event
would hang the turn rather than block anything. A veto therefore changes *which*
response is published, and exactly one `llm.response` is always emitted.

Producers must publish through `engine.PublishLLMResponse` rather than calling
`bus.Emit("llm.response", …)` directly:

```go
// In a provider plugin, replacing a bare bus.Emit:
engine.PublishLLMResponse(p.bus, resp)
```

It runs the hook, applies the rules below, and emits. Handlers have two moves:

| Handler does | Result |
|---|---|
| Mutates `Content`, no veto | The edit is published as the model's response. Tool calls intact. |
| Vetoes, leaves `Content` alone | Substitute whose `Content` is the veto reason. |
| Mutates `Content`, then vetoes | Substitute keeping the handler's text — the handler dictates the replacement. |

**A vetoed response never carries tool calls.** `ToolCalls` is cleared on the
substitute, so a blocked response cannot drive tool execution. This is the
security value of the hook and the reason the logic lives in one helper rather
than at each provider's emit site. The substitute also sets
`FinishReason: "vetoed"` and stamps `Metadata["_vetoed"] = true` plus
`Metadata["_veto_reason"]`.

`RequestID`, `Model`, `Tags`, `Usage` and `CostUSD` survive substitution:
correlation must still work, and the tokens were spent whether or not the
answer ships, so `nexus.gate.token_budget` still sees them.

A response already stamped `_vetoed` skips the hook. Without that guard, a gate
that vetoes and then drives a corrective LLM request could veto its own
replacement indefinitely.

**Why not `before:io.output`?** That hook fires *after* the agent loop has
executed the response's tool calls. It can scrub the prose the user finally
reads; it cannot stop an action. `before:llm.response` sits between the provider
and the loop, so it governs tool calls, intermediate iterations, and reasoning —
not just final answers.

**Streaming.** Providers emit `llm.stream.chunk` as tokens arrive and
`llm.response` only at the end, so a veto *here* cannot unsay text a UI has
already rendered. Gates that steer the loop — blocking tool calls, replacing
what enters conversation history — work unchanged under streaming regardless.

Gates that must prevent **disclosure** handle
[`before:llm.stream.chunk`](#beforellmstreamchunk-gate-the-stream-itself) as
well, which adjudicates each delta before it is released. A gate that does not
does not silently lose its guarantee: the engine reads the active plugins'
declared `Subscriptions()` at boot and clears `Stream` on outbound requests
when any plugin gates `before:llm.response` without gating
`before:llm.stream.chunk`, logging which plugin cost the deployment its
streaming. See [`core.streaming`](../configuration/reference.md#corestreaming)
for the opt-out.

**Never interpolate model output into a veto reason.** The reason becomes the
substitute's `Content` whenever the handler doesn't dictate its own, and
output-side gates (`content_safety`, `stop_words`, `json_schema`,
`output_length`) deliberately skip a substituted response rather than
re-judging operator-authored text. A reason built as
`fmt.Sprintf("blocked: %s", resp.Content)` would therefore hand the user the
exact content the gate just blocked, past every gate that would have caught
it. Describe what tripped; log the offending text separately.

**Provider-side execution already happened.** Some providers run tools on
their own servers and surface the results while converting the response —
Gemini's code execution emits `tool.invoke` / `tool.result` before
`PublishLLMResponse` is reached, and Anthropic's server-side `web_search`
lands in the response metadata. A veto stops the *client-side* tool calls the
response asked for; it cannot un-run what the provider already executed. Gate
`before:llm.request` to stop those from being offered in the first place.

**Replay bypasses the hook.** During journal replay, providers publish the
recorded `llm.response` directly. Those are re-published history — already
post-gate on the original run — so re-gating them would double-apply and break
replay fidelity.

### `before:llm.stream.chunk`: gate the stream itself

`before:llm.response` governs what the loop consumes, and it does that well —
but it fires once, at end of stream, after every token has been rendered. A
gate whose purpose is non-disclosure needs to act earlier than that, and the
alternative (turn streaming off) costs every turn its time-to-first-token.

So providers do not emit `llm.stream.chunk` directly. Every text delta goes
through `engine.StreamPublisher`, which offers it to `before:llm.stream.chunk`
as an `*events.StreamSegment` before any of it reaches the bus. The invariant
lives in one helper, for the same reason `PublishLLMResponse` does: so it
cannot be forgotten at one provider's emit site.

Handlers pass, redact, suspend, or block:

```go
func (p *Plugin) handleBeforeStreamChunk(event engine.Event[any]) {
    vp, _ := event.Payload.(*engine.VetoablePayload)
    seg, _ := vp.Original.(*events.StreamSegment)

    // Defer the trailing token: it may still be growing into a match.
    seg.HoldFrom(trailingTokenStart(seg.Content))

    // Scan the cumulative turn, act on matches that have not shipped yet.
    for _, loc := range p.pattern.FindAllStringIndex(seg.Full(), -1) {
        if loc[1] <= seg.PendingOffset() {
            continue // adjudicated on an earlier segment
        }
        vp.Veto = engine.VetoResult{Vetoed: true, Reason: "prohibited content"}
        return
    }
}
```

**Suspension is what makes this a real guarantee rather than a faster
retraction.** Withheld bytes are never emitted, so a block prevents disclosure
outright — for every transport, including third-party clients whose protocol
offers no way to take text back. Held text is re-offered, prepended to the
next delta, so a handler may keep holding across arbitrarily many segments —
but only while segments keep arriving. The final flush at end of stream ignores
`Hold` and offers the tail once more, so a handler still undecided there must
release or veto rather than defer. A gate consulting a slow reviewer has the
model's generation time to answer, and must block if it has not.

**Detection is independent of the hold.** Because handlers scan the cumulative
turn and act on where a match *ends*, a match is caught on the segment that
completes it however the provider chunked it. The hold bounds the *residue* —
how much of a match's leading text had already shipped when the block landed.

**A block is terminal for the turn.** The publisher drops what it held, emits
`llm.stream.retract`, and swallows every later delta including tool-call
chunks. The provider still accumulates the model's real text and still
publishes it through `PublishLLMResponse`, so the response-level gate
substitutes exactly as it does without streaming.

**The ungated path is unchanged.** With nothing subscribed, the publisher
emits each delta verbatim — same text, same chunk boundaries, no buffering,
and no hook dispatch at all. That last part is deliberate: `before:*` events
are journaled like any other, so firing one per token delta unconditionally
would roughly double journal volume on every streaming turn.

**Tool-call chunks bypass the text gate.** They are not disclosure, and
authority over whether they may run belongs to `before:llm.response` (which
clears `ToolCalls` on every substitution) and `before:tool.invoke` (which sees
parsed arguments). Gating a partial JSON fragment here would duplicate that
with strictly less information.

**Never quote blocked text into a veto reason** — the same rule as
`before:llm.response`, and for the same reason: the reason is user-visible.

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

Semantics:

- **In flight** means any `Emit`/`EmitEvent`/`EmitVetoable` that has entered and not yet returned, nested emits included. A handler's nested emit keeps the bus busy until it returns too.
- **Drain never blocks an emit.** Emits issued while `Drain` waits, nested or top-level, dispatch normally. `Drain` waits for a quiescent moment (in-flight count reaches zero). It does not close the bus or reject new events, so a bus that never goes quiet is bounded only by `ctx`.
- **Drain holds no lock while waiting and starts no goroutine.** It returns `nil` once the bus is idle (immediately if it already is, even with a done `ctx`), or `ctx.Err()` on expiry. Either way the bus stays fully usable, so plugin `Shutdown` hooks that emit after a timed-out drain do not hang.

In-flight tracking is a mutex-guarded counter plus an "idle" channel that is closed on every 1 → 0 transition. It replaced a `sync.WaitGroup` whose `Add`/`Wait` race guard was a mutex held across the whole `Wait`. That mutex deadlocked against nested emits from the handlers `Drain` was waiting on, then hung `Engine.Stop` after the drain timeout ([#172](https://github.com/frankbardon/nexus/issues/172), [#202](https://github.com/frankbardon/nexus/issues/202)).

`LifecycleManager.Shutdown` emits `core.shutdown`, drains for up to `engine.shutdown.drain_timeout` (or the largest plugin `DrainOverride`), logs `drain timed out` on expiry, and then shuts plugins down regardless.

## Thread Safety

The event bus is safe for concurrent use. Handler registration and event dispatch use read-write locks. Handler slices are copied before dispatch to allow concurrent emits without blocking.
