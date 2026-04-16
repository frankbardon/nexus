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
