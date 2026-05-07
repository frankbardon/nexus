# Native Realtime API Integration — Deferred

Both **OpenAI Realtime API** and **Gemini Multimodal Live** are entire new
wire protocols separate from the standard chat/generate endpoints already
used by `plugins/providers/openai/` and `plugins/providers/gemini/`. They
require per-provider WebSocket clients with their own session lifecycle,
tool-use envelopes, audio-streaming framing, and turn-taking semantics —
each on the order of a full provider plugin's implementation surface.

That is too large to ship in the multimodal-foundation PR ([#93][pr]),
which limits scope to the building blocks: blob store, multimodal
`MessagePart` plumbing, the `EmbeddingsRequest.Inputs` shape, the Cohere
multimodal embeddings adapter, and the `nexus.memory.vector`
`store_images` opt-in. Native realtime adapters are tracked under issue
[#91][issue] as a follow-up.

Until they land, voice-mode users go through the standard pipeline:

```
ASR (e.g. Whisper) → llm.request → TTS
```

implemented as the `plugins/io/voice/` transport (Phase 4 of #91). The
voice IO plugin is a portable design — when realtime providers are
wired, the same agent configs keep working; only the inner LLM call is
replaced with a streaming session.

## What's needed when these land

For each provider:

- WebSocket client (`gorilla/websocket` or stdlib upgrader on the server
  side; both providers run server-sent WebSockets) honoring the
  documented session-init handshake.
- A new `nexus.io.realtime.<provider>` transport plugin OR a new LLM
  provider variant that consumes `voice.audio.input.chunk` events and
  emits `voice.audio.output.chunk` events. Decision deferred to the
  follow-up issue; both shapes work, but the IO-plugin shape keeps the
  rest of the engine unaware of the streaming wire details.
- Tool-use bridging: realtime APIs surface tool calls inline in the
  audio stream — those need to translate into the existing `tool.call`
  / `tool.result` events with no behavior change for tool plugins.
- Cancel + barge-in plumbing onto `nexus.control.cancel`.

[pr]: https://github.com/frankbardon/nexus/pull/93
[issue]: https://github.com/frankbardon/nexus/issues/91
