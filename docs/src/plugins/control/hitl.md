# Human-in-the-Loop (HITL)

The unified human-interaction primitive: any plugin that needs an
operator's input — clarifying a question, approving a destructive
action, picking among curated choices, signing off on a memory write —
emits a single event type and waits for the structured response.

## Details

| | |
|---|---|
| **ID** | `nexus.control.hitl` |
| **Tool Name** | `ask_user` (LLM-facing) |
| **Dependencies** | None |

## Configuration

| Key                  | Type   | Default          | Description |
|----------------------|--------|------------------|-------------|
| `registry.enabled`   | bool   | `false`          | Mirror every `hitl.requested` to disk and watch for response files written by `nexus hitl respond`, webhook handlers, etc. |
| `registry.dir`       | string | `~/.nexus/hitl`  | Filesystem directory for the request/response YAML pairs. Tilde-expanded; created at boot if missing. |

The registry is the async out-of-band response channel — see the
[HITL operations guide](../../operations/hitl.md#async-response-out-of-band-channels-via-the-filesystem-registry)
for the wire format and CLI surface (`nexus hitl list / respond /
cancel`, plus `nexus approve` and `nexus reject` shorthands).

Future iterations will add a `require_approval` style policy for
non-tool action kinds and a prompt-synthesizer capability binding.

## Tool Parameters

The `ask_user` tool's schema lets the LLM present a freeform prompt, a
multi-choice prompt, or both:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `prompt` | string | Yes | Question or approval text shown to the user. |
| `mode` | enum (`free_text`, `choices`, `both`) | No | Response shape. Defaults to `free_text`. |
| `choices` | array of `{id, label}` | When `mode != free_text` | Multi-choice options the operator picks from. |
| `default_choice_id` | string | No | Choice picked on deadline expiry. Must reference an entry in `choices`. |
| `deadline_seconds` | integer | No | Auto-resolution timeout. (Currently advisory; deadline enforcement lands in a follow-up.) |

Tool result is structured JSON, not a bare string:

```json
{ "choice_id": "approve" }
{ "free_text": "let's go with cautious" }
{ "choice_id": "edit", "free_text": "trim batch to 50" }
```

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handles `ask_user` invocations from the agent. |
| `hitl.responded` | 50 | Routes operator answers to the awaiting requester. |
| `hitl.requested` | 50 | (Only when `registry.enabled`) Mirrors the request to disk for out-of-band response. |

### Emits

| Event | When |
|-------|------|
| `before:hitl.requested` | Canonical vetoable entry point with `*HITLRequest` payload. Subscribers (e.g. the prompt synthesizer) can mutate `Prompt` or veto. |
| `hitl.requested` | After a non-veto result, value-shape emission consumed by IO plugins. |
| `tool.register` | Registers the `ask_user` tool at boot. |
| `tool.result` | After the operator responds (carries the structured JSON). |

## Action Kinds

`HITLRequest.ActionKind` discriminates what is awaiting human input.
Today the registry treats it as opaque metadata; downstream policy code
will consume it. Conventional values:

| Kind | Source |
|------|--------|
| `free_text` | `ask_user` tool, mode = `free_text` |
| `ask_user.choices` | `ask_user` tool, mode = `choices` or `both` |
| `tool.invoke` | (planned) `gates/approval_policy/` |
| `memory.longterm.write` | (planned) memory plugin require_approval |
| `memory.vector.write` | (planned) memory plugin require_approval |
| `llm.request` | (planned) `gates/approval_policy/` |

## How It Works

1. Plugin calls `bus.EmitVetoable("before:hitl.requested", &req)` so
   `before:hitl.requested` subscribers (notably the prompt synthesizer)
   can mutate `req.Prompt` or veto. On veto, the request resolves as
   rejected/cancelled without reaching IO. On non-veto, the plugin
   follows up with `bus.Emit("hitl.requested", req)`.
2. The active IO plugin (TUI, Browser, Wails, oneshot, test) renders
   the prompt and collects the operator's answer.
3. IO plugin emits `hitl.responded` with `{request_id, choice_id?, free_text?}`.
4. The hitl plugin routes the response to the requester's channel.
5. For `ask_user` tool calls, the plugin emits a `tool.result` with the
   response encoded as JSON.

The plugin replays cleanly: during deterministic replay, the journaled
`tool.result` short-circuits the live ask, so the operator is never
re-prompted with stale questions.

## Example Configuration

```yaml
plugins:
  active:
    - nexus.control.hitl
```

## Multi-Choice Example (LLM Side)

The agent can present curated choices directly:

```json
{
  "name": "ask_user",
  "arguments": {
    "prompt": "I see three migration paths. Which should I take?",
    "mode": "choices",
    "choices": [
      { "id": "a", "label": "Cautious — rename column, dual-write 1 week" },
      { "id": "b", "label": "Balanced — rename + immediate cutover" },
      { "id": "c", "label": "Aggressive — drop and re-create" }
    ],
    "default_choice_id": "a"
  }
}
```

The operator's answer arrives as `{"choice_id": "b"}` (or `{"choice_id": "edit", "free_text": "..."}` in `mode: both`).
