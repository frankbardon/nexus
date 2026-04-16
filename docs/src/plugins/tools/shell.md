# Shell Tool

Executes shell commands in a controlled environment with optional command allowlisting and sandboxing.

## Details

| | |
|---|---|
| **ID** | `nexus.tool.shell` |
| **Tool Name** | `shell` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `allowed_commands` | string[] | *(none)* | Whitelist of allowed command names. If empty, all commands are allowed. |
| `timeout` | duration | `30s` | Maximum execution time per command |
| `sandbox` | bool | `false` | Restrict environment variables when enabled |

## Tool Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `command` | string | Yes | The shell command to execute |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handles shell command execution |

### Emits

| Event | When |
|-------|------|
| `tool.result` | Command output (stdout + stderr) |
| `tool.register` | Registers the `shell` tool at boot |
| `core.error` | Execution errors |

## Security Features

### Command Allowlist

When `allowed_commands` is set, only commands whose base name matches the list are permitted:

```yaml
nexus.tool.shell:
  allowed_commands: ["ls", "cat", "grep", "find", "git", "make"]
```

Attempting to run a command not in the list returns an error to the agent.

### Sandbox Mode

When `sandbox: true`, the shell environment is restricted. This provides a basic layer of isolation.

### Command History

All executed commands are logged to `plugins/nexus.tool.shell/history.txt` in the session directory.

## Example Configurations

### Minimal (full access)
```yaml
nexus.tool.shell:
  timeout: 30s
```

### Coding assistant
```yaml
nexus.tool.shell:
  allowed_commands: ["go", "git", "ls", "cat", "grep", "find", "mkdir", "rm", "cp", "mv", "make", "docker", "npm", "cargo", "python"]
  timeout: 30s
  sandbox: true
```

### Read-only exploration
```yaml
nexus.tool.shell:
  allowed_commands: ["ls", "cat", "grep", "find", "head", "tail", "wc"]
  timeout: 10s
  sandbox: true
```
