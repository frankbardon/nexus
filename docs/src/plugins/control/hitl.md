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
| `hitl.cancel` | 50 | Requester-side retraction; emits a synthetic `hitl.responded{cancelled: true}`. |
| `hitl.requested` | 50 | Records requests that name a tool call (`CallID`) so a `tool.timeout` can cancel them; when `registry.enabled`, also mirrors the request to disk for out-of-band response. |
| `tool.timeout` | 50 | Cancels every open request whose `CallID` is the timed-out call (see below). |

### Emits

| Event | When |
|-------|------|
| `before:hitl.requested` | Canonical vetoable entry point with `*HITLRequest` payload. Subscribers (e.g. the prompt synthesizer) can mutate `Prompt` or veto. |
| `hitl.requested` | After a non-veto result, value-shape emission consumed by IO plugins. |
| `tool.register` | Registers the `ask_user` tool at boot. |
| `tool.result` | After the operator responds (carries the structured JSON). |
| `hitl.responded` | Synthetic `{cancelled: true}` response on `hitl.cancel` or on a `tool.timeout` for the request's call. |

## Action Kinds

`HITLRequest.ActionKind` discriminates what is awaiting human input.
Today the registry treats it as opaque metadata; downstream policy code
will consume it. Conventional values:

| Kind | Source |
|------|--------|
| `free_text` | `ask_user` tool, mode = `free_text` |
| `ask_user.choices` | `ask_user` tool, mode = `choices` or `both` |
| `tool.invoke` | `gates/approval_policy/` rule on a tool call; `TurnID` is the gated call's `BindingTurnID()` — its `ParentTurnID` inside a subagent/delegate loop, else its `TurnID` |
| `llm.request` | `gates/approval_policy/` rule on an LLM request; `TurnID` comes from `Metadata["turn_id"]` |
| `memory.longterm.write` | `memory/longterm` `require_approval`; from the `memory_write` tool, `TurnID` is the call's `BindingTurnID()` (empty for a bus-driven store) |
| `memory.vector.write` | `memory/vector` `require_approval` |
| `memory.compaction.commit` | `memory/compaction` `require_approval` |

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

## Requests for a timed-out tool call

`HITLRequest.CallID` names the tool call (`ToolCall.ID`) a request was
raised on or for. The shipped producers stamp it: `ask_user` (its own
call), `gates/approval_policy` on `tool.invoke`, `memory/longterm`'s
`memory_write`, and `agents/a2aremote` when a remote parks at
`INPUT_REQUIRED`. It is empty for requests that belong to no call.

When a `tool.timeout` fires (emitted by `nexus.gate.tool_timeout`), the
plugin cancels every still-open request whose `CallID` matches — whoever
raised it — through the same path as `hitl.cancel`: the registry file is
removed and a synthetic `hitl.responded{cancelled: true, cancel_reason:
"tool call <id> (<tool>) timed out after <d>"}` is emitted. That response
unblocks the requester's waiter and is what IO transports dismiss the
prompt on. Without it, a prompt for a call that is already over stayed open
until shutdown, which then waited up to `engine.shutdown.drain_timeout`
for it. Requests for other calls are untouched. A plugin raising a HITL
request inside a tool call should set `CallID` to get this behaviour.

The memory approval gates (`memory/longterm`, `memory/vector`,
`memory/compaction` `require_approval`) retract their own request the same
way when their `require_approval.timeout` expires: they emit `hitl.cancel`
for it, so the registry entry is removed and the synthetic response dismisses
the prompt. All three default that timeout to `30m` (`"0"` disables it).

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
