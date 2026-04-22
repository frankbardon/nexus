# Oneshot I/O

A non-interactive I/O plugin for scripting, batch jobs, and CI. It runs the agent for a single turn, auto-approves every approval request, and writes a JSON transcript of the run to stdout (and optionally a file).

The name reflects its semantics: one prompt in, one transcript out, then the process exits. (A future `nexus.io.headless` plugin will keep a long-running process alive so external systems can drive it — this is not that.)

## Details

| | |
|---|---|
| **ID** | `nexus.io.oneshot` |
| **Dependencies** | None |

Use this plugin **instead of** `nexus.io.tui` or `nexus.io.browser` — exactly one I/O plugin should be active at a time.

## Prompt resolution

The plugin resolves the prompt to feed into the agent from the first of these sources that is non-empty:

1. `NEXUS_ONESHOT_PROMPT` environment variable
2. `input` field in the plugin config
3. `input_file` field in the plugin config (path to a text file)
4. Piped stdin (only when stdin is not a terminal)

If none of these yield a prompt, the run fails fast and still emits a JSON document containing the error so callers get something actionable.

## Configuration

```yaml
plugins:
  active:
    - nexus.io.oneshot
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.memory.capped
    - nexus.observe.logger

  nexus.io.oneshot:
    input: ""             # inline prompt, or leave empty to use another source
    input_file: ""        # path to a file containing the prompt
    output_file: ""       # optional: also write the JSON transcript to this path
    pretty: true          # pretty-print the JSON (default true)
    read_stdin: true      # allow reading a piped stdin as a fallback (default true)
```

All fields are optional.

## Usage

```bash
# Pipe a prompt through stdin
echo "What is 2+2?" | bin/nexus -config configs/oneshot.yaml

# Supply the prompt via environment variable
NEXUS_ONESHOT_PROMPT="Summarize octopus intelligence" \
  bin/nexus -config configs/oneshot.yaml

# Read the prompt from a file (via config)
bin/nexus -config configs/oneshot.yaml   # with input_file: ./prompt.txt set

# Feed the JSON transcript into jq for post-processing
echo "List three interesting facts about octopuses" \
  | bin/nexus -config configs/oneshot-planned.yaml \
  | jq '.final_output'
```

## Auto-approval

The oneshot plugin auto-approves **every** approval request so agents with planners and protected tools can run unattended:

- `io.approval.request` (tool call approval) → responds `Approved: true`
- `plan.approval.request` (planner approval) → responds `Approved: true`
- `io.ask` (free-form question to the user) → responds with an empty string

Every auto-approval is recorded in the `approvals` array of the transcript so callers can audit what happened.

> ⚠️ Because every approval is granted, be deliberate about which tools you make available under this profile. Avoid enabling destructive shell commands or filesystem writes outside a sandbox unless you trust the prompt source.

## JSON transcript schema

The root document is tagged with `schema: "nexus.oneshot.transcript/v1"`.

| Field | Type | Description |
|-------|------|-------------|
| `schema` | string | Always `nexus.oneshot.transcript/v1` |
| `session_id` | string | Nexus session ID (matches `~/.nexus/sessions/<id>/`) |
| `started_at` / `ended_at` | RFC3339 timestamp | Lifetime of the run |
| `duration_ms` | number | Wall-clock duration |
| `final_output` | string | Final assistant message text |
| `plans` | array | `plan.created` events (full plan snapshots) |
| `plan_updates` | array | `agent.plan` events (step status transitions) |
| `thinking` | array | `thinking.step` events (reasoning trace) |
| `approvals` | array | Auto-approved tool / plan / ask requests |
| `errors` | array | `core.error` events and error-role `io.output` messages |

Every array field is omitted from the JSON when empty.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.output` | 50 | Capture final assistant text and error messages |
| `io.approval.request` | 10 | Auto-approve tool call approvals |
| `plan.approval.request` | 10 | Auto-approve plan approvals |
| `io.ask` | 10 | Auto-respond with an empty answer |
| `plan.created` | 50 | Record generated plans in the transcript |
| `agent.plan` | 50 | Record plan status updates |
| `thinking.step` | 50 | Record reasoning trace |
| `agent.turn.start` / `agent.turn.end` | 50 | Detect when the single turn has completed |
| `core.error` | 50 | Record errors in the transcript |

The approval / ask handlers subscribe at priority `10` so they run before any other plugin's handlers and respond immediately.

### Emits

| Event | When |
|-------|------|
| `io.input` | Once during `Ready`, with the resolved prompt |
| `io.approval.response` | Auto-approval for a tool call |
| `plan.approval.response` | Auto-approval for a plan |
| `io.ask.response` | Auto-response for an ask prompt |
| `io.session.start` | On `Ready` |
| `io.session.end` | After the final JSON transcript has been flushed |

## Lifecycle

1. `Init` wires subscriptions and reads config.
2. `Ready` emits `io.session.start`, resolves the prompt, then emits `io.input` on a new goroutine so the engine's main loop can install its signal + session-end handlers.
3. The agent runs its turn. Any approval or ask prompts are auto-handled.
4. When `agent.turn.end` brings the turn depth back to zero, the plugin builds the JSON transcript, writes it to stdout (and `output_file` if set), then emits `io.session.end` to trigger engine shutdown.
5. `Shutdown` is idempotent — if the run was terminated by a signal before the turn completed, it still flushes whatever was captured.

## Sample profiles

Two profiles ship with Nexus:

- [`configs/oneshot.yaml`](https://github.com/frankbardon/nexus/blob/main/configs/oneshot.yaml) — minimal ReAct loop with conversation memory.
- [`configs/oneshot-planned.yaml`](https://github.com/frankbardon/nexus/blob/main/configs/oneshot-planned.yaml) — adds the dynamic planner with `approval: always` to exercise the auto-approval path end-to-end.
