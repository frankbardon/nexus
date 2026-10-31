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
