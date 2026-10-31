# Terminal UI (TUI)

A rich terminal interface built with BubbleTea. Provides markdown rendering, streaming output, approval dialogs, and status indicators.

## Details

| | |
|---|---|
| **ID** | `nexus.io.tui` |
| **Dependencies** | None |

## Configuration

No additional configuration. The TUI plugin uses BubbleTea defaults.

## Events

### Subscribes To

All subscriptions use the default priority `0`.

| Event | Purpose |
|-------|---------|
| `io.output` | Display agent responses |
| `llm.stream.chunk` / `llm.stream.hold` / `llm.stream.retract` / `llm.stream.end` | Streaming response rendering, including held and retracted text |
| `io.output.clear` | Clear the output pane |
| `io.status` | Update status bar (thinking, tool_running, etc.) |
| `io.approval.request` | Show tool approval dialogs |
| `hitl.requested` | Show human-in-the-loop question prompts |
| `thinking.step` | Display thinking indicators |
| `code.exec.stdout` | Show `run_code` output |
| `plan.approval.request` | Show plan approval dialogs |
| `plan.created` / `plan.progress` / `agent.plan` | Display generated plans and progress |
| `provider.fallback` | Surface provider failover |
| `session.file.created` / `session.file.updated` | Show file activity notifications |
| `io.history.replay` | Replay conversation on session resume |
| `cancel.complete` | Handle cancellation UI |
| `icm.*` (`run.started`, `run.completed`, `run.halted`, `stage.started`, `stage.completed`, `stage.failed`, `stage.iteration`, `turn`, `fanout.item`, `predicate.failed`) | Render ICM workflow progress as thinking-step rows |
| `workflow.progress` | Workflow status panel |

### Emits

| Event | When |
|-------|------|
| `io.input` / `before:io.input` | User submits a message |
| `io.approval.response` | User responds to approval dialog |
| `hitl.responded` | User answers a human-in-the-loop question |
| `plan.approval.response` | User approves/rejects a plan |
| `io.session.start` / `io.session.end` | Session lifecycle |
| `cancel.request` | User cancels current operation |
| `cancel.resume` | User resumes after cancellation |

## Features

- Markdown rendering in the terminal
- Streaming response display with incremental rendering
- Approval dialogs for tool execution
- Plan display and approval
- Status bar showing current agent state
- File creation/update notifications
- Session resume with history replay
- Built-in commands: `/quit`, `/exit`

## Interrupt keys

- **Esc** while a turn is running emits `cancel.request`: with `nexus.control.cancel`
  active, the turn is cancelled (resumable with `/resume`) and every tool call it
  has in flight — a running `shell` command's whole process group included — is
  cancelled. The TUI stays open.
- **Ctrl-C** quits. While a turn is running it first emits `cancel.request`, as Esc
  does. Either way the program exit emits `io.session.end`, so the host stops the
  engine, and `Engine.Stop` cancels every tool call still in flight. (The terminal is
  in raw mode, so Ctrl-C reaches the TUI as a key, not as `SIGINT`; and a sandboxed
  shell command runs in its own process group, so it never sees a terminal signal
  either way — the engine's cancellation is what kills it.)
