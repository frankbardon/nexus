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
5. Emit `llm.response` or `llm.stream.chunk` / `llm.stream.end`

Providers don't know about agents, tools, or conversations — they only translate between the Nexus event model and the external API.

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
