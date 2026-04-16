# I/O Interface Plugins

I/O plugins handle user interaction — displaying agent output and collecting user input. You need exactly one active I/O plugin.

## Available I/O Plugins

| Plugin | ID | Interface |
|--------|----|-----------|
| [Terminal UI](./tui.md) | `nexus.io.tui` | BubbleTea-based terminal interface |
| [Browser UI](./browser.md) | `nexus.io.browser` | HTTP/WebSocket web interface |
| [Wails Desktop](./wails.md) | `nexus.io.wails` | Wails webview transport for desktop apps |
| [Oneshot](./oneshot.md) | `nexus.io.oneshot` | Non-interactive single-turn JSON transcript (scripting / CI) |

## I/O Event Flow

Both I/O plugins follow the same event pattern:

- **Input**: Collect user text → emit `io.input`
- **Output**: Receive `io.output` → display to user
- **Streaming**: Receive `io.output.stream` chunks → render incrementally
- **Approvals**: Receive `io.approval.request` → show dialog → emit `io.approval.response`
- **Questions**: Receive `io.ask` → show prompt → emit `io.ask.response`
- **Status**: Receive `io.status` → update status indicator
