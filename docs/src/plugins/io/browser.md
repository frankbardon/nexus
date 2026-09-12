# Browser UI

A web-based interface using HTTP and WebSockets. Provides the same functionality as the TUI but accessible through a browser.

## Details

| | |
|---|---|
| **ID** | `nexus.io.browser` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `host` | string | `localhost` | HTTP server bind address |
| `port` | int | `8080` | HTTP server port |
| `open_browser` | bool | `true` | Automatically open the browser on start |

## Request headers

Request headers named `X-Nexus-*` are forwarded into the engine on every turn
this transport starts, keyed by normalized name (`X-Nexus-Tenant-ID` ->
`tenant-id`). Plugins read them from `events.UserInput.Headers` or, outside the
`io.input` path, from `session.RequestHeaders()`. Nothing authenticates them —
they are the caller's own statement about its request — so they are bound to
the reserved, prompt-invisible session-label namespace and surfaced to the
model only when an operator names one in `nexus.system.dynvars.request_headers`.
No configuration turns the forwarding on. See
[Request Headers](../../guides/request-headers.md).

## Events

Subscribes to and emits the same events as the [TUI plugin](./tui.md).

## Architecture

- **HTTP Server** — Serves the web UI static assets
- **WebSocket** — Real-time bidirectional communication
- **Hub** — Coordinates multiple WebSocket connections

Input is emitted asynchronously to avoid deadlocks with the event bus.

## Example Configuration

```yaml
nexus.io.browser:
  host: localhost
  port: 3000
  open_browser: true
```
