# Long-Term Memory

**Plugin ID:** `nexus.memory.longterm`

Cross-session memory persistence. Stores memories as individual markdown files with YAML frontmatter. Injects a lightweight index into the system prompt on boot and provides LLM tools for CRUD operations.

## Configuration

```yaml
nexus.memory.longterm:
  # Where memory files live.
  # Default: ~/.nexus/memory/ (CLI), ~/.nexus/agents/<agentID>/memory/ (desktop shell)
  path: "~/.nexus/memory/"

  # Memory scope: "agent" (per-agent isolated), "global" (shared across agents), or "both".
  # "both" merges global + agent-scoped memories, with agent-scoped taking precedence on key conflicts.
  scope: "agent"

  # Inject memory index (titles + keys + tags) into system prompt on session start.
  auto_load: true

  # Instructions injected into system prompt telling the LLM when to
  # proactively save memories mid-session. Empty string = only save when
  # user explicitly asks (tools still available, just no prompting to use them).
  auto_save_instructions: ""

  # Agent ID for agent-scoped storage (injected automatically by desktop shell).
  agent_id: ""
```

### Scope Resolution

| `scope` value | Paths searched | Write target |
|---|---|---|
| `agent` | `~/.nexus/agents/<agentID>/memory/` | agent path |
| `global` | `~/.nexus/memory/` | global path |
| `both` | global path + agent path (agent wins conflicts) | agent path |

For CLI mode (no agent ID), `agent` and `global` behave identically --- both use the configured `path`.

## Storage Format

One file per memory entry at `<memory_path>/<key>.md`:

```markdown
---
key: user_prefers_dark_mode
tags:
  category: preference
  domain: ui
created: 2026-04-10T14:30:00Z
updated: 2026-04-15T09:00:00Z
source_session: 20260410-143022
---

User strongly prefers dark mode across all interfaces.
They mentioned eye strain with light themes during long sessions.
```

Keys are sanitized to lowercase alphanumeric with hyphens and underscores, truncated to 128 characters.

## LLM Tools

| Tool | Parameters | Description |
|---|---|---|
| `memory_write` | `key` (string), `content` (string), `tags` (map, optional) | Create or update a memory entry |
| `memory_read` | `key` (string) | Read full content of a memory entry |
| `memory_list` | `tags` (map, optional) | List all memories, optionally filtered by tags (AND semantics) |
| `memory_delete` | `key` (string) | Delete a memory entry |

## System Prompt Injection

When `auto_load: true`, the plugin injects a section listing all available memories with key, tags, and a one-line preview (first line of content). Full content is retrieved on demand via `memory_read`.

## Events

| Event | Direction | Payload |
|---|---|---|
| `memory.longterm.loaded` | Plugin -> bus | `LongTermMemoryLoaded` |
| `memory.longterm.store` | Any -> plugin | `LongTermMemoryStoreRequest` |
| `memory.longterm.stored` | Plugin -> bus | `LongTermMemoryStored` |
| `memory.longterm.read` | Any -> plugin | `LongTermMemoryReadRequest` |
| `memory.longterm.result` | Plugin -> bus | `LongTermMemoryReadResult` |
| `memory.longterm.delete` | Any -> plugin | `LongTermMemoryDeleteRequest` |
| `memory.longterm.deleted` | Plugin -> bus | `LongTermMemoryDeleted` |
| `memory.longterm.list` | Any -> plugin | `LongTermMemoryQuery` |
| `memory.longterm.list.result` | Plugin -> bus | `LongTermMemoryListResult` |

## Desktop Shell Integration

The desktop shell automatically injects `agent_id` into the plugin config before boot, enabling per-agent memory isolation without manual configuration.
