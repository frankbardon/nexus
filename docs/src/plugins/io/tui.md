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

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.output` | 0 | Display agent responses |
| `io.output.stream` / `io.output.stream.end` | 0 | Streaming response rendering |
| `io.status` | 0 | Update status bar (thinking, tool_running, etc.) |
| `io.approval.request` | 0 | Show tool approval dialogs |
| `io.ask` | 50 | Show question prompts |
| `thinking.step` | 0 | Display thinking indicators |
| `plan.approval.request` | 0 | Show plan approval dialogs |
| `plan.created` | 0 | Display generated plans |
| `agent.plan` | 0 | Display plan progress |
| `session.file.created` / `session.file.updated` | 0 | Show file activity notifications |
| `io.history.replay` | 0 | Replay conversation on session resume |
| `cancel.complete` | 0 | Handle cancellation UI |

### Emits

| Event | When |
|-------|------|
| `io.input` | User submits a message |
| `io.approval.response` | User responds to approval dialog |
| `io.ask.response` | User answers a question |
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
