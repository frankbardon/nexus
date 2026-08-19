# Test IO (`nexus.io.test`)

Non-interactive IO plugin for automated integration testing. Replaces `nexus.io.tui` in test configurations to drive sessions programmatically.

## Purpose

The test IO plugin feeds scripted inputs into the engine, collects all bus events during execution, and handles approval requests automatically. Used alongside `pkg/testharness` for Go integration tests.

## Configuration

```yaml
plugins:
  active:
    - nexus.io.test
    # ... other plugins

  nexus.io.test:
    inputs:                          # scripted user messages (in order)
      - "Hello, who are you?"
      - "List files in current directory."
    input_delay: 500ms               # delay between inputs (default: 500ms)
    approval_mode: approve           # approve | deny | per-prompt (default: approve)
    approval_rules:                  # only when approval_mode: per-prompt
      - match: "shell"               # substring match on tool call or description
        action: approve
      - match: "rm -rf"
        action: deny
    ask_responses:                   # canned answers for io.ask events
      - "yes"
    hitl_responses:                  # canned answers for hitl.requested events
      - "staging"
    hitl_auto_respond: true          # answer hitl.requested at all (default: true)
    mock_responses:                  # synthetic LLM responses (no real API calls)
      - content: "Hello! I'm a test assistant."
      - content: "Here are the files: main.go, go.mod"
    timeout: 60s                     # max session duration (default: 60s)
```

### Approval Modes

| Mode | Behavior |
|------|----------|
| `approve` | Auto-approve all approval requests |
| `deny` | Auto-deny all approval requests |
| `per-prompt` | Match against `approval_rules`, approve if no rule matches |

### Mock Responses

When `mock_responses` is configured, the plugin intercepts `before:llm.request` (at priority 20, after gates at priority 10) and injects synthetic `llm.response` events instead of letting requests reach the real LLM provider. No API key needed, millisecond execution.

Gates still fire first — a stop words gate can veto a request before the mock ever sees it. Responses are consumed in order; the last one repeats for any remaining requests.

Mock responses can include tool calls for testing tool execution flows:

```yaml
mock_responses:
  - content: ""
    tool_calls:
      - name: shell
        arguments: '{"command": "ls"}'
  - content: "Done listing files."
```

### Human-in-the-loop Answers

Every `hitl.requested` — `ask_user`, an approval-policy gate, a remote A2A agent
parking at `INPUT_REQUIRED` — is answered automatically. The answer is the next
`hitl_responses` entry (a bare string is `free_text`; a
`{choice_id: ..., free_text: ...}` map sets either field), else the request's own
`default_choice_id`, else an empty answer. The last entry repeats.

Set `hitl_auto_respond: false` to answer **nothing**. The question is still
collected for assertions, but the plugin that asked it stays blocked, so a test
can hand ownership of the answer to something else: another transport, a
subscription in the test body, or — as in
`tests/integration/a2a_loopback_test.go` — a second engine's human. Without it a
question can never be observed outstanding, because this plugin settles it inside
the same dispatch.

### Session Ending

`io.session.end` is emitted when the last scripted input's turn completes, when
`timeout` elapses, or when a turn is still in flight **three seconds** after the
last input was sent — the stalled-turn detector, which exists so a permanently
vetoed turn does not hang until the global timeout. That three-second cutoff is
not configurable, so a test whose turn legitimately waits longer (a delegation
parked on a human, say) must arrange for the wait to end inside it.

### Input Feeding

Inputs are sent sequentially. The plugin waits for the agent to become idle (turn depth returns to zero) before sending the next input. After all inputs are sent and the final turn completes, the plugin emits `io.session.end`.

### Ask Responses

When the agent emits `io.ask` events, the plugin responds with canned answers from `ask_responses` in order. The last response in the list repeats for any remaining asks. If `ask_responses` is empty, an empty string is returned.

## Event Collection

The plugin subscribes to **all** bus events via `SubscribeAll` and stores them in an ordered slice. After the session ends, collected events are accessible via the `Collected()` method for test assertions.

## Integration with Test Harness

The test IO plugin is designed to work with `pkg/testharness`:

```go
h := testharness.New(t, "configs/test-minimal.yaml")
h.Run()                              // boots engine, feeds inputs, waits for completion
h.AssertEventEmitted("io.output")    // check collected events
h.AssertNoSystemOutput()             // no gate vetoes
```

See the [Integration Testing guide](../../guides/integration-testing.md) for full usage.

## Subscriptions

| Event | Purpose |
|-------|---------|
| `io.approval.request` | Auto-respond per approval config |
| `plan.approval.request` | Auto-respond per approval config |
| `io.ask` | Respond with canned answers |
| `hitl.requested` | Respond per `hitl_responses` / `hitl_auto_respond` |
| `agent.turn.start` | Track turn depth for input pacing |
| `agent.turn.end` | Detect idle state, trigger next input or session end |
| `*` (wildcard) | Collect all events for assertions |

## Emissions

| Event | When |
|-------|------|
| `io.session.start` | On `Ready()` |
| `io.input` | For each scripted input |
| `io.approval.response` | In response to approval requests |
| `plan.approval.response` | In response to plan approval requests |
| `io.ask.response` | In response to ask events |
| `hitl.responded` | In response to `hitl.requested`, unless `hitl_auto_respond: false` |
| `io.session.end` | After all inputs processed or timeout |
