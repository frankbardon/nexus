# IO Transport Plugins

IO transport plugins project the engine event bus onto a user or an external client. Plugins never call each other; every transport only subscribes to and emits bus events.

## First-party UI: `nexus.io.browser`

- **Session-scoped.** One browser session, one engine session, then the plugin shuts down. No multi-session management, session switching or recall UI in the plugin. CLI `main` (or the user closing the tab and emitting `io.session.end`) owns "what session am I in."
- Serves the Nexus web UI over HTTP/WS. Wire format is the `pkg/ui` Envelope; the `UIAdapter` interface lives in `pkg/ui/adapter.go`. Keep generic logic (event serialization, envelope format, UI-side rendering) in `pkg/ui/`, not in the transport.
- Stays a thin transport: no app-shell features (session list, OS dialogs, menus, tray).

## External-interop and specialized transports

`nexus.io.agui`, `nexus.io.a2a`, `nexus.io.realtime` and `nexus.io.broker` each speak their own wire and are not Nexus-authored UIs:

- `agui` serves the open [AG-UI protocol](https://docs.ag-ui.com): `POST /agui` accepts a `RunAgentInput`, bus events become a canonical AG-UI SSE stream (`RunStarted` -> text/tool/reasoning -> `RunFinished`). Wire is `pkg/agui`, not the `pkg/ui` Envelope.
- `a2a` serves the A2A protocol (wire in `pkg/a2a`).
- `realtime` and `broker` are specialized transports (voice/realtime, broker dial-back) with their own framing.

They are translation layers bus<->protocol; the renderer is somebody else's client. Changes to one family do not require a mirror in another, and nothing protocol-specific is back-ported into `browser`.

**Superset for Nexus-specific events.** Bus events with no canonical AG-UI equivalent (`workflow.progress`, `subagent.*`, `code.exec.stdout`) ride the AG-UI `Custom` event (name = bus event type, value = JSON payload); stock clients ignore them. Full mapping table and config in `docs/src/plugins/io-agui.md` and `docs/src/configuration/reference.md#nexusioagui`.
