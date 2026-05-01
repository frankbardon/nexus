# Dynamic Variables Plugin

Injects dynamic system information (date, time, OS, working directory) into the agent's system prompt.

## Details

| | |
|---|---|
| **ID** | `nexus.system.dynvars` |
| **Dependencies** | None |

## Configuration

Each variable is **opt-in** — defaults to `false` and must be explicitly
enabled:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `date` | bool | `false` | Include current date |
| `time` | bool | `false` | Include current time |
| `timezone` | bool | `false` | Include timezone |
| `cwd` | bool | `false` | Include current working directory |
| `session_dir` | bool | `false` | Include session directory path |
| `os` | bool | `false` | Include operating system |

## Events

This plugin does not subscribe to or emit any events. It registers a prompt section via the Prompt Registry during initialization.

## System Prompt Output

The plugin appends a section like this to the system prompt:

```
## System Info
- Date: 2026-04-08
- Time: 10:30:00
- Timezone: America/New_York
- OS: darwin
- CWD: /Users/frank/projects/myapp
- Session: ~/.nexus/sessions/abc123
```

## Example Configuration

```yaml
# Empty config → no variables emitted (every flag defaults to false).
nexus.system.dynvars: {}

# Enable only the variables you want.
nexus.system.dynvars:
  date: true
  cwd: true
```
