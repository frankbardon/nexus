# IO Transport Plugins: `nexus.io.browser` ↔ `nexus.io.wails`

Two sibling IO transport plugins project engine event bus onto UI. Share Nexus web UI code via `pkg/ui` adapter contract, but differ deliberately in scope and lifetime.

## Scope and lifetime

- `nexus.io.browser` = **session-scoped**. One browser session, one engine session, then plugin shuts down. No multi-session mgmt, no session switching, no recall UI in plugin. CLI `main` (or user closing tab + emitting `io.session.end`) owns "what session am I in."
- `nexus.io.wails` = **process-scoped**, acts as desktop shell wrapper. Can start new session, recall old sessions, switch between them, surface OS-native file dialogs, menus, notifications, drag-and-drop. Owns long-lived webview process + session lifecycle *within* that process.

## Parity rule (enforced — do not violate)

When extending either plugin, every change classified into one of two buckets:

1. **In-session UX feature** — something user does *inside* active session: rendering new event type, showing status indicator, surfacing new approval flow, adding keyboard shortcut, improving streaming display, etc. **Belongs in both plugins.** Add to `nexus.io.browser`, must also port to `nexus.io.wails`, and vice versa. Should live in shared code under `pkg/ui/` when practical so port is mechanical not rewrite.

2. **Shell/wrapper feature** — only makes sense at desktop-app or multi-session boundary: session list UI, recall-from-history, OS file dialogs, native menus, system tray, window mgmt, notifications, drag-and-drop, auto-update UI, app-level settings beyond `ANTHROPIC_API_KEY`. **Belongs only in `nexus.io.wails`.** Must not back-port to `nexus.io.browser` — `browser` intentionally thin session-scoped transport. Adding wrapper features makes it second desktop shell by accident and destroys simplicity that makes it useful as dev-mode + headless sibling.

**When in doubt, ask:** "Would this feature make sense if user already picked session and plugin's only job was rendering that one session's events?" If yes, in-session, goes in both. If feature implies user choosing *between* sessions, talking to OS, or living past single session's lifetime, wrapper feature, only `nexus.io.wails`.

**Shared code vs forked code.** Anything generic (event serialization, message envelope format, `UIAdapter` interface in `pkg/ui/adapter.go`, UI-side rendering logic) lives in shared packages so both plugins consume. Only transport layer differs: HTTP/WS server in `browser`, Wails runtime bindings in `wails`. If duplicating logic across both plugins that isn't transport-specific, stop and factor into `pkg/ui/` first.

## Config-driven event bridging (`nexus.io.wails`)

Wails IO plugin runs in two modes:

- **Legacy mode** (no config keys): hardcoded chat-event subs with typed handlers.
- **Config-driven mode** (`subscribe`/`accept` lists in YAML): generic passthrough bridging for arbitrary domain events. Developer controls exactly which events cross bus↔frontend boundary.

```yaml
plugins:
  nexus.io.wails:
    subscribe:              # bus → frontend
      - "match.result"
      - "ui.state.restore"
    accept:                 # frontend → bus
      - "match.request"
      - "ui.state.save"
```

Config-driven mode required for desktop shell example agents (hello-world, staffing-match). All domain comms flow through bus bridge, not Wails-bound methods.

## Multi-agent scoping

When desktop shell hosts multiple agents, each gets scoped `Runtime` adapter that namespaces event channels by agent ID:

- Outbound: `"{agentID}:nexus"` instead of `"nexus"`
- Inbound: `"{agentID}:nexus.input"` instead of `"nexus.input"`

Wails IO plugin itself unaware of scoping — talks to its `Runtime`, scoped wrapper handles namespace. No plugin changes needed for multi-agent.
