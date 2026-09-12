# I/O Interface Plugins

I/O plugins handle user interaction — displaying agent output and collecting user input. You need exactly one active I/O plugin.

## Available I/O Plugins

| Plugin | ID | Interface |
|--------|----|-----------|
| [Terminal UI](./tui.md) | `nexus.io.tui` | BubbleTea-based terminal interface |
| [Browser UI](./browser.md) | `nexus.io.browser` | HTTP/WebSocket web interface |
| [Wails Desktop](./wails.md) | `nexus.io.wails` | Wails webview transport for desktop apps |
| [Oneshot](./oneshot.md) | `nexus.io.oneshot` | Non-interactive single-turn JSON transcript (scripting / CI) |
| [Broker IO](./broker.md) | `nexus.io.broker` | Dial-back transport for instances spawned by the [session broker](../../guides/session-broker.md) |

## Request Headers

Every web-facing transport (`nexus.io.agui`, `nexus.io.a2a`, `nexus.io.browser`,
`nexus.io.realtime`, and `nexus.io.broker` via the gateway) forwards request
headers named `X-Nexus-*` into the engine, where plugins read them from
`events.UserInput.Headers` or `session.RequestHeaders()`. It is how a client
passes per-request context — tenant, locale, timezone, a trace correlator —
without putting it in the prompt. No configuration turns it on. See
[Request Headers](../../guides/request-headers.md).

## I/O Event Flow

Both I/O plugins follow the same event pattern:

- **Input**: Collect user text → emit `io.input`
- **Output**: Receive `io.output` → display to user
- **Streaming**: Receive `io.output.stream` chunks → render incrementally
- **Approvals**: Receive `io.approval.request` → show dialog → emit `io.approval.response`
- **Questions**: Receive `io.ask` → show prompt → emit `io.ask.response`
- **Status**: Receive `io.status` → update status indicator
