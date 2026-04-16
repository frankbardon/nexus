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
| `io.output` | 50 | Display agent responses |
| `io.output.stream` / `io.output.stream.end` | 50 | Streaming response rendering |
| `io.status` | 50 | Update status bar (thinking, tool_running, etc.) |
| `io.approval.request` | 50 | Show tool approval dialogs |
| `io.ask` | 50 | Show question prompts |
| `thinking.step` | 50 | Display thinking indicators |
| `plan.approval.request` | 50 | Show plan approval dialogs |
| `plan.created` | 50 | Display generated plans |
| `agent.plan` | 50 | Display plan progress |
| `session.file.created` / `session.file.updated` | 50 | Show file activity notifications |
| `io.history.replay` | 50 | Replay conversation on session resume |
| `cancel.complete` | 50 | Handle cancellation UI |

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
