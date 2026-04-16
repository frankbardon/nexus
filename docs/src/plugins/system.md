# Dynamic Variables Plugin

Injects dynamic system information (date, time, OS, working directory) into the agent's system prompt.

## Details

| | |
|---|---|
| **ID** | `nexus.system.dynvars` |
| **Dependencies** | None |

## Configuration

Each variable can be individually enabled or disabled:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `date` | bool | `true` | Include current date |
| `time` | bool | `true` | Include current time |
| `timezone` | bool | `true` | Include timezone |
| `cwd` | bool | `true` | Include current working directory |
| `session_dir` | bool | `true` | Include session directory path |
| `os` | bool | `true` | Include operating system |

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
# All variables enabled (default)
nexus.system.dynvars: {}

# Only date and CWD
nexus.system.dynvars:
  date: true
  time: false
  timezone: false
  cwd: true
  session_dir: false
  os: false
```
