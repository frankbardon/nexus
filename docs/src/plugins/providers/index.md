# LLM Providers

LLM provider plugins handle communication with AI model APIs. They receive `llm.request` events, call the external API, and emit `llm.response` (or streaming chunks).

## Available Providers

| Plugin | ID | Service |
|--------|----|---------|
| [Anthropic](./anthropic.md) | `nexus.llm.anthropic` | Claude (direct HTTP, no SDK) |
| [OpenAI](./openai.md) | `nexus.llm.openai` | GPT / o-series (direct HTTP, no SDK) |
| [Gemini](./gemini.md) | `nexus.llm.gemini` | Google Gemini — public api-key + Vertex AI; thinking, multimodal, code execution, prompt caching |
| [Fallback](./fallback.md) | `nexus.provider.fallback` | Automatic provider failover coordinator |
| [Fanout](./fanout.md) | `nexus.provider.fanout` | Parallel multi-provider dispatch |

## Provider Architecture

Providers are low-level plugins that:

1. Subscribe to `llm.request` at high priority (10)
2. Resolve the requested model role via the Model Registry
3. Apply prompt registry sections to the system prompt
4. Make the API call (with streaming support)
5. Publish the response via `engine.PublishLLMResponse` (**not** a bare `bus.Emit("llm.response", ...)`), and while streaming publish text deltas through an `engine.StreamPublisher` (**not** a bare `bus.Emit("llm.stream.chunk", ...)`)

Providers don't know about agents, tools, or conversations — they only translate between the Nexus event model and the external API.

### Publishing stream chunks

A streaming provider does not emit `llm.stream.chunk` itself. It creates one
`engine.StreamPublisher` per turn and feeds deltas to it:

```go
pub := engine.NewStreamPublisher(p.bus, turnID, requestID)
defer pub.Close()

for /* each SSE frame */ {
    fullContent.WriteString(delta)  // the model's text, verbatim
    pub.Text(delta)                 // what a UI is allowed to see
}

pub.Close()  // flush any held tail before llm.stream.end
```

The publisher runs the vetoable `before:llm.stream.chunk` hook before releasing
anything, so a gate can redact a delta, suspend the stream while it decides, or
block it outright — see
[Event Bus → `before:llm.stream.chunk`](../../architecture/event-bus.md#beforellmstreamchunk-gate-the-stream-itself).
Three things a provider author needs to know:

- **Keep accumulating the model's real text.** `fullContent` and what the
  publisher releases diverge whenever a gate redacts or blocks, and that is
  intended: `PublishLLMResponse` must still see what the model actually said,
  so the response-level gate adjudicates the real thing.
- **`Close()` on every path**, including error returns — `defer` it. A stream
  abandoned mid-flight would otherwise leave a gate's hold outstanding and a UI
  showing a review indicator nothing ever clears. `Close` is idempotent, so
  calling it explicitly before `llm.stream.end` (to order the final flush
  correctly) is fine.
- **Tool-call chunks go through `pub.ToolCall`**, which bypasses the text gate
  by design. Whether a tool may run is decided by `before:llm.response` and
  `before:tool.invoke`, both of which see more than a partial JSON fragment.

With no gate subscribed the publisher is a pass-through: the same text, the
same chunk boundaries, no buffering.

### Publishing responses

Every `llm.response` a provider produces must go through
`engine.PublishLLMResponse(bus, resp)`. It runs the vetoable
`before:llm.response` hook, lets gates replace or block the response, clears
`ToolCalls` on a vetoed one, and then emits `llm.response` exactly once:

```go
engine.PublishLLMResponse(p.bus, resp)
```

Declare `"before:llm.response"` in the plugin's `Emissions()` alongside
`"llm.response"`; the contract harness checks declared emissions against
runtime behaviour.

The one exception is journal replay, which publishes the recorded response
directly — that is re-published history and re-gating it would break replay
fidelity.

Full contract: [Event Bus → `before:llm.response`](../../architecture/event-bus.md#beforellmresponse-veto-means-substitute).

## Structured Output

When `ResponseFormat` is set on an `LLMRequest`, providers map it to their native structured output mechanism if supported, or simulate it otherwise.

### Capability Matrix

| Provider | Native Support | Strategy |
|----------|---------------|----------|
| **OpenAI** | Yes | Maps directly to `response_format` in the API payload |
| **Anthropic** | No | Simulates via tool-use-as-schema: injects synthetic tool, forces tool choice, unwraps tool call arguments as structured response |
| **Gemini** | Yes | Maps to `generationConfig.responseMimeType` + `responseSchema` (incompatible JSON Schema keywords stripped) |
| **Other/unknown** | No | Ignores the field; `json_schema` gate handles validation downstream |

### Metadata Flag

Providers set `LLMResponse.Metadata["_structured_output"] = true` when structured output enforcement was used (native or simulated). Downstream consumers (like the `json_schema` gate) can check this flag to skip redundant validation.
