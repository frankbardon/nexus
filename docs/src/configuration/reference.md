# Configuration Reference

Complete reference for all YAML configuration options.

## File Structure

```yaml
core:
  # Engine-level settings
  ...

plugins:
  active:
    # List of plugin IDs to load
    - nexus.io.tui
    - nexus.llm.anthropic
    - ...

  # Per-plugin configuration
  nexus.plugin.id:
    key: value
```

## Core Settings

### `core`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `log_level` | string | `info` | Global log level: `debug`, `info`, `warn`, `error` |
| `tick_interval` | duration | `1s` | Interval for `core.tick` heartbeat events |
| `max_concurrent_events` | int | `100` | Maximum concurrent event dispatches |

### `core.sessions`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `root` | string | `~/.nexus/sessions` | Base directory for session storage |
| `retention` | string | `30d` | How long to keep old sessions |
| `id_format` | string | `timestamp` | Session ID format: `timestamp`, `datetime_short` |

### `core.models`

Defines model roles. Each role maps to a provider, model ID, and token limit.

| Key | Type | Description |
|-----|------|-------------|
| `default` | string | Name of the default role (e.g., `balanced`) |
| `<role_name>` | object | Model configuration for this role |

**Role object:**

| Key | Type | Description |
|-----|------|-------------|
| `provider` | string | Plugin ID of the LLM provider |
| `model` | string | Model identifier |
| `max_tokens` | int | Maximum tokens for responses |

```yaml
core:
  models:
    default: balanced
    reasoning:
      provider: nexus.llm.anthropic
      model: claude-opus-4-20250514
      max_tokens: 16384
    balanced:
      provider: nexus.llm.anthropic
      model: claude-sonnet-4-20250514
      max_tokens: 8192
    quick:
      provider: nexus.llm.anthropic
      model: claude-haiku-4-5-20251001
      max_tokens: 4096
```

## Plugin Settings

### `plugins.active`

A list of plugin IDs to activate. Order doesn't matter — dependencies are resolved automatically.

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
```

Multi-instance plugins use a slash suffix:

```yaml
plugins:
  active:
    - nexus.agent.subagent/researcher
    - nexus.agent.subagent/writer
```

### Per-Plugin Configuration

Each plugin ID in the config (other than `active`) provides that plugin's settings:

```yaml
plugins:
  nexus.tool.shell:
    allowed_commands: ["ls", "git"]
    timeout: 30s
```

Plugins with no configuration still need an entry if you want to be explicit:

```yaml
plugins:
  nexus.tool.file: {}
  nexus.observe.thinking: {}
```

## All Plugin Config Options

### Agents

**nexus.agent.react**
| Key | Type | Default |
|-----|------|---------|
| `max_iterations` | int | `25` |
| `planning` | bool | `false` |
| `model_role` | string | *(default)* |
| `system_prompt` | string | *(none)* |
| `system_prompt_file` | string | *(none)* |

**nexus.agent.planexec**
| Key | Type | Default |
|-----|------|---------|
| `max_iterations` | int | `15` |
| `max_steps` | int | `10` |
| `planning_model_role` | string | `reasoning` |
| `execution_model_role` | string | `balanced` |
| `replan_on_failure` | bool | `true` |
| `approval` | string | `always` |
| `system_prompt` | string | *(none)* |
| `system_prompt_file` | string | *(none)* |

**nexus.agent.subagent**
| Key | Type | Default |
|-----|------|---------|
| `max_iterations` | int | `10` |
| `model_role` | string | *(default)* |
| `system_prompt` | string | *(none)* |
| `system_prompt_file` | string | *(none)* |
| `tool_name` | string | `spawn_subagent` |
| `tool_description` | string | *(auto)* |

**nexus.agent.orchestrator**
| Key | Type | Default |
|-----|------|---------|
| `max_workers` | int | `5` |
| `max_subtasks` | int | `8` |
| `worker_max_iterations` | int | `10` |
| `orchestrator_model_role` | string | `reasoning` |
| `worker_model_role` | string | `balanced` |
| `synthesis_model_role` | string | `balanced` |
| `fail_fast` | bool | `false` |
| `system_prompt` | string | *(none)* |
| `system_prompt_file` | string | *(none)* |

### LLM Providers

**nexus.llm.anthropic**
| Key | Type | Default |
|-----|------|---------|
| `api_key_env` | string | `ANTHROPIC_API_KEY` |
| `debug` | bool | `false` |

### Tools

**nexus.tool.shell**
| Key | Type | Default |
|-----|------|---------|
| `allowed_commands` | string[] | *(none — all allowed)* |
| `timeout` | duration | `30s` |
| `sandbox` | bool | `false` |

**nexus.tool.file**
| Key | Type | Default |
|-----|------|---------|
| `base_dir` | string | *(session files dir)* |

**nexus.tool.pdf**
| Key | Type | Default |
|-----|------|---------|
| `timeout` | duration | `30s` |
| `pdftotext_bin` | string | `pdftotext` |
| `pdfinfo_bin` | string | `pdfinfo` |
| `save_to_session` | bool | `false` |
| `save_file_name` | string | *(auto)* |

**nexus.tool.opener**
| Key | Type | Default |
|-----|------|---------|
| `open_cmd` | string | *(auto-detected)* |
| `timeout` | duration | `10s` |

**nexus.tool.ask** — No configuration options.

### Memory

**nexus.memory.conversation**
| Key | Type | Default |
|-----|------|---------|
| `max_messages` | int | `100` |
| `persist` | bool | `true` |

**nexus.memory.compaction**
| Key | Type | Default |
|-----|------|---------|
| `strategy` | string | `message_count` |
| `message_threshold` | int | `50` |
| `token_threshold` | int | `30000` |
| `turn_threshold` | int | `10` |
| `chars_per_token` | float | `4.0` |
| `model_role` | string | `quick` |
| `protect_recent` | int | `4` |
| `compaction_prompt` | string | *(built-in)* |
| `prompt_file` | string | *(none)* |

### I/O

**nexus.io.tui** — No configuration options.

**nexus.io.browser**
| Key | Type | Default |
|-----|------|---------|
| `host` | string | `localhost` |
| `port` | int | `8080` |
| `open_browser` | bool | `true` |

### Observers

**nexus.observe.logger**
| Key | Type | Default |
|-----|------|---------|
| `output` | string | `stdout` |
| `file_path` | string | *(none)* |
| `level` | string | `info` |

**nexus.observe.thinking** — No configuration options.

### Planners

**nexus.planner.dynamic**
| Key | Type | Default |
|-----|------|---------|
| `approval` | string | `always` |
| `model_role` | string | *(default)* |
| `max_steps` | int | `10` |
| `plan_prompt` | string | *(built-in)* |
| `plan_prompt_file` | string | *(none)* |

**nexus.planner.static**
| Key | Type | Default |
|-----|------|---------|
| `approval` | string | `never` |
| `summary` | string | *(none)* |
| `steps` | list | *(required)* |

### Skills

**nexus.skills**
| Key | Type | Default |
|-----|------|---------|
| `scan_paths` | string[] | *(none)* |
| `trust_project` | string | `ask` |
| `max_active_skills` | int | `10` |
| `catalog_in_system_prompt` | bool | `true` |
| `disabled_skills` | string[] | *(none)* |

### System

**nexus.system.dynvars**
| Key | Type | Default |
|-----|------|---------|
| `date` | bool | `true` |
| `time` | bool | `true` |
| `timezone` | bool | `true` |
| `cwd` | bool | `true` |
| `session_dir` | bool | `true` |
| `os` | bool | `true` |

### Control

**nexus.control.cancel** — No configuration options.
