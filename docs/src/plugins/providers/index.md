# LLM Providers

LLM provider plugins handle communication with AI model APIs. They receive `llm.request` events, call the external API, and emit `llm.response` (or streaming chunks).

## Available Providers

| Plugin | ID | Service |
|--------|----|---------|
| [Anthropic](./anthropic.md) | `nexus.llm.anthropic` | Claude (direct HTTP, no SDK) |
| [OpenAI](./openai.md) | `nexus.llm.openai` | GPT / o-series (direct HTTP, no SDK) |

## Provider Architecture

Providers are low-level plugins that:

1. Subscribe to `llm.request` at high priority (10)
2. Resolve the requested model role via the Model Registry
3. Apply prompt registry sections to the system prompt
4. Make the API call (with streaming support)
5. Emit `llm.response` or `llm.stream.chunk` / `llm.stream.end`

Providers don't know about agents, tools, or conversations — they only translate between the Nexus event model and the external API.
