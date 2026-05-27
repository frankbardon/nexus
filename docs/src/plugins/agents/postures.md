# Posture Registry (`nexus.agent.postures`)

Loads `AgentPosture` YAML files from disk and exposes the `posture.registry`
capability that `nexus.agent.delegate` consumes. fsnotify watches every
configured directory for live edits; active sub-sessions keep their old
posture, new invocations resolve the new one.

See the [Postures architecture page](../../architecture/postures.md) for the
full conceptual model and [delegation](../../architecture/delegate.md) for
how the registry is consumed.

## Details

| | |
|---|---|
| **ID** | `nexus.agent.postures` |
| **Capability** | `posture.registry` |
| **Dependencies** | *(none)* |
| **Requires** | *(none)* |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `scan_dirs` | []string | `[]` | Directories scanned for `*.yaml` / `*.yml` posture files. Paths run through `engine.ExpandPath` (supports `~`). |
| `debounce_ms` | int | `250` | fsnotify reload debounce in milliseconds. |

If `scan_dirs` is empty, no postures load; the plugin still advertises the
capability so `nexus.agent.delegate` boots cleanly (delegate calls will
fail with `posture: not found`).

## Posture YAML

Each file in a scan dir is parsed as a single `AgentPosture`. The filename
(minus extension) supplies the `name` if the YAML omits one.

```yaml
name: analyst
description: deep reader; quotes sources verbatim
system_prompt: |
  You are a careful analyst. Cite sources by URL. Be concise.
allowed_tools:
  - web_search
  - web_fetch
  - read_pdf
model:
  model_role: reasoning
  max_tokens: 4000
default_budget:
  timeout: 60s
  max_tokens: 50000
  max_tool_calls: 20
max_recursion_depth: 2
```

A 16-character content hash lands on `Version`, included automatically in
delegate cache keys so any edit invalidates stale entries.

## Events

### Emits

| Event | When |
|-------|------|
| `posture.registered` | A posture loads (initial scan) or reloads (watcher fire). |
| `posture.removed` | A posture file is deleted or fails to parse after an edit. |

See [events reference](../../events/reference.md#posture-events) for
payload shape.

### Subscribes To

None.

## Example

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.agent.postures
    - nexus.agent.delegate
    - nexus.memory.capped

  nexus.agent.postures:
    scan_dirs:
      - ~/.nexus/postures
      - ./configs/postures
    debounce_ms: 250
```
