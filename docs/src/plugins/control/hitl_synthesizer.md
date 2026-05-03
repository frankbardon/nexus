# HITL Prompt Synthesizer

Optional companion to [`nexus.control.hitl`](./hitl.md). Renders
context-aware approval prompts via a small/cheap LLM so reviewers see
"Approve `rm -rf /tmp/build`?" rather than the gate's hand-written
template. Pure opt-in: a request only goes through the synthesizer
when its emitter sets `PromptSynthesizer` to this plugin's capability
ID.

## Details

| | |
|---|---|
| **ID** | `nexus.control.hitl_synthesizer` |
| **Capability** | `hitl.prompt_synthesizer` |
| **Dependencies** | None (uses the active `llm.*` provider chain) |

## Why a Separate Plugin

Some users want zero extra LLM calls in the approval path; others
want the convenience of model-rendered prompts. Splitting the
synthesizer out keeps the core hitl primitive frugal — leave it out
of `plugins.active` and HITL stays as cheap as the emitter's literal
prompt or `text/template`.

## How It Works

1. An emitter (gate, memory plugin, agent) builds a `HITLRequest`
   with `PromptSynthesizer: "hitl.prompt_synthesizer"` and an empty
   `Prompt`. It either:
   - Calls `bus.EmitVetoable("before:hitl.requested", &req)` first,
     then `bus.Emit("hitl.requested", req)` once the synthesizer has
     mutated `Prompt`, or
   - Calls `bus.Emit("hitl.requested", &req)` directly with a
     pointer payload.
2. The synthesizer's handler runs at priority `-100`, ahead of every
   IO plugin (priorities 0/10/50). It checks the opt-in conditions,
   consults the cache, and if there's no hit emits an `llm.request`
   tagged `Metadata["_source"] = "nexus.control.hitl_synthesizer"`.
3. The active LLM provider returns an `llm.response` synchronously;
   the synthesizer extracts the rendered text, writes it back into
   `req.Prompt`, and persists it to disk for next time.
4. Subsequent IO plugins receive the pointer-shared payload (or the
   value re-emitted by the wrapper) with `Prompt` already populated.

If the LLM call fails (provider error, empty response, missing
`llm.response`), the synthesizer never blocks indefinitely: it falls
back to a Go `text/template` rendered against
`{action_kind, action_ref, requester_plugin, request_id}` and lets
the request continue. Failed renders are not cached — only successful
LLM responses are.

## Configuration

```yaml
plugins:
  active:
    - nexus.control.hitl
    - nexus.control.hitl_synthesizer

nexus.control.hitl_synthesizer:
  model: haiku                       # resolved via core.models
  max_action_ref_chars: 1500
  cache_enabled: true
  fallback_prompt: "Approve action: {{.action_kind}}"
```

| Key                    | Type   | Default                              | Description |
|------------------------|--------|--------------------------------------|-------------|
| `model`                | string | `haiku`                              | Model role used for synthesis (resolved via `core.models`). |
| `model_id`             | string | *(none)*                             | Explicit model ID; bypasses role lookup when set. |
| `max_action_ref_chars` | int    | `1500`                               | ActionRef truncation budget (in JSON characters). |
| `cache_enabled`        | bool   | `true`                               | Persist successful synthesis results to `cache.jsonl`. |
| `fallback_prompt`      | string | `Approve action: {{.action_kind}}`   | `text/template` used when synthesis fails. |

## Caching

Successful syntheses persist to
`~/.nexus/sessions/<session>/plugins/nexus.control.hitl_synthesizer/cache.jsonl`,
keyed by `(action_kind, sha256(action_ref))`. The hash uses
`encoding/json`'s sorted map output, so equivalent action references
with different literal key orderings collapse to the same key. The
cache is hydrated at boot and written through on every store.

To force a re-render (e.g. while iterating on the system prompt) set
`cache_enabled: false` or delete `cache.jsonl`.

## Pointer-vs-Value Payloads

The synthesizer mutates the request in place. That requires the
emitter to publish a `*HITLRequest`, not a value. The recommended
pattern is to use the vetoable path:

```go
// Future-proof emitter (gate or memory plugin):
req := &events.HITLRequest{ /* … */ }
if _, err := bus.EmitVetoable("before:hitl.requested", req); err != nil {
    return err
}
_ = bus.Emit("hitl.requested", *req)  // dereference once Prompt is filled
```

When the emitter publishes a value to `hitl.requested` (the legacy
path), the synthesizer logs and skips — it cannot propagate
mutations through a copy. In practice the legacy emitters either set
`Prompt` themselves (the `ask_user` tool) or supply a `PromptTemplate`
the requesting plugin renders inline, so the pointer-only constraint
mostly affects new code that explicitly opts into LLM rendering.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `before:hitl.requested` | -100 | Vetoable opt-in path; mutates `*HITLRequest.Prompt` via `*VetoablePayload`. Never vetoes. |
| `hitl.requested` | -100 | Legacy path; mutates only when the payload is `*HITLRequest`. |
| `llm.response` | 50 | Routes responses tagged with this plugin's `_source` to the awaiting synthesis call. |

### Emits

| Event | When |
|-------|------|
| `llm.request` | Synthesis cache miss; tagged with `Metadata["_source"]` and `Metadata["_synth_id"]` for response correlation. |
