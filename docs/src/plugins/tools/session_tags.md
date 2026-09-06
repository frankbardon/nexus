# Session Tags Tool

Gives the agent itself read/write access to its own session's tags (key/value labels attached to the session), via four LLM-facing tools. **Off by default.**

## Details

| | |
|---|---|
| **ID** | `nexus.tool.session_tags` |
| **Source** | `plugins/tools/session_tags/plugin.go` |
| **Tool Names** | `session_tag_set`, `session_tag_get`, `session_tag_delete`, `session_tag_list` |
| **Dependencies** | None |
| **Default state** | Not active — opt-in only |

## Opting in

This plugin ships in the main binary but is not part of any stock config's `plugins.active` list. To give the agent this capability, add it explicitly:

```yaml
plugins:
  active:
    - nexus.tool.session_tags
    # ... your other active plugins
```

Same convention as `nexus.embeddings.mock` and other optional plugins: it's compiled in, just not activated unless listed.

## Configuration

```yaml
nexus.tool.session_tags:
  tools:
    session_tag_set: true      # default
    session_tag_get: true      # default
    session_tag_delete: true   # default
    session_tag_list: true     # default
```

| Key                 | Type | Default          | Description |
|---------------------|------|------------------|-------------|
| `tools.<tool_name>` | bool | `true` for each  | Per-tool enable/disable, mirroring `nexus.tool.file`'s `tools.<tool_name>` convention. Recognized names are `session_tag_set`, `session_tag_get`, `session_tag_delete`, `session_tag_list`; an unknown name under `tools` is logged and ignored rather than failing boot. |

## Tools

### `session_tag_set`

| Parameter | Type   | Required | Description |
|-----------|--------|----------|-------------|
| `key`     | string | Yes      | The tag key to set. Must not start with an underscore. |
| `value`   | string | Yes      | The tag value. |

Output (`OutputStructured`): `{"key": string, "value": string}`.

Sets a general-namespace session tag. Rides the same vetoable `before:session.tag.set` bus path any other caller uses — see [Behavior and the reserved namespace](#behavior-and-the-reserved-namespace) below.

### `session_tag_get`

| Parameter | Type   | Required | Description |
|-----------|--------|----------|-------------|
| `key`     | string | Yes      | The tag key to read. |

Output (`OutputStructured`): `{"key": string, "value": string, "found": bool}` (`value` only present when `found` is `true`).

Reads a single general-namespace tag directly off `SessionMetadata().Labels` — this call never touches the bus.

### `session_tag_delete`

| Parameter | Type   | Required | Description |
|-----------|--------|----------|-------------|
| `key`     | string | Yes      | The tag key to delete. |

Output (`OutputStructured`): `{"key": string}`.

Deletes a general-namespace session tag. Rides the vetoable `before:session.tag.delete` bus path.

### `session_tag_list`

No parameters.

Output (`OutputStructured`): `{"tags": {"<key>": "<value>", ...}}` — every general-namespace tag currently set on the session.

## Behavior and the reserved namespace

This plugin can **only ever** touch the general (non-`_`-prefixed) namespace of `SessionMeta.Labels`. It has no special-casing, no pre-check, and no bypass around the engine's reserved-prefix enforcement:

- `session_tag_set` / `session_tag_delete` emit `before:session.tag.set` / `before:session.tag.delete` exactly like any other bus caller. The one enforcement point — `engine.installSessionTagHandlers`, using the shared `engine.IsReservedLabelKey` prefix check — rejects a reserved (`_`-prefixed) key unconditionally. A rejected call surfaces as an ordinary tool error (the veto reason), not a crash or a silent no-op.
- `session_tag_get` reports a reserved key as **not found** (`"found": false`), identical to a key that was never set. It never uses a different error message or code path to reveal that the key exists.
- `session_tag_list` **omits reserved keys entirely** from its result set — not redacted, not marked, simply absent, so the tool result carries no signal that a reserved namespace even exists.

In short: there is no argument, config key, or call sequence through this plugin that reads, writes, or enumerates a reserved-prefixed tag. Reserved tags (for example the identity-derived `_principal_id` binding written by `nexus.io.agui`) are written through a direct Go method not exposed on the bus, entirely outside this plugin's reach.

For the full mechanics of the reserved-prefix mechanism itself — what counts as reserved, who else can write general-namespace tags without going through this plugin, and how `session.tag.set`/`session.tag.deleted` announcements fit into the rest of the system — see [Configuration Reference](../../configuration/reference.md#cost-cli) (the authoritative description lives there today) and [Sessions](../../architecture/sessions.md). A dedicated narrative guide for the tagging system as a whole is planned separately.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handles `session_tag_set`/`get`/`delete`/`list` calls |

### Emits

| Event | When |
|-------|------|
| `before:session.tag.set` | `session_tag_set` call, before applying |
| `before:session.tag.delete` | `session_tag_delete` call, before applying |
| `before:tool.result` | Before publishing any tool result (vetoable — gates can inspect/block) |
| `tool.result` | Tool call result |
| `tool.register` | Registers all four tools at `Ready()` |

Reads (`session_tag_get`, `session_tag_list`) emit no bus events beyond the standard `before:tool.result`/`tool.result` pair — they read `SessionMetadata().Labels` directly and never touch the tag-write bus path.

## Errors

- **`key argument is required`** — `session_tag_set`/`get`/`delete` called without a `key`.
- **`no active session`** — called outside a session context.
- A vetoed `before:session.tag.set`/`before:session.tag.delete` surfaces the veto's `Reason` as the tool's `Error` string (this is how a reserved-key write/delete attempt is reported).

## Example Configuration

```yaml
plugins:
  active:
    - nexus.tool.session_tags
    # ... rest of your active plugins

nexus.tool.session_tags:
  tools:
    session_tag_delete: false   # let the agent set/read/list, but never delete
```
