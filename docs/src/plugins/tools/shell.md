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
| `working_dir` | string | *(session files dir)* | Working directory for executions. |
| `timeout` | duration | `30s` | Maximum execution time per command. |
| `sandbox.backend` | string | `host` | Sandbox tier (`host`; future: `gvisor`, `firecracker`, `landlock`). |
| `sandbox.allowed_commands` | string[] | *(none — all allowed)* | Whitelist of base command names. |
| `sandbox.path_dirs` | string[] | *(none)* | Directories prepended to `PATH`. |
| `sandbox.env_restrict` | bool | `false` | Strip sensitive env vars before execution. |
| `sandbox.timeout` | duration | `30s` | Per-command default; top-level `timeout` wins per-call. |

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

When `sandbox.allowed_commands` is set, only commands whose base name matches the list are permitted:

```yaml
nexus.tool.shell:
  sandbox:
    allowed_commands: ["ls", "cat", "grep", "find", "git", "make"]
```

Attempting to run a command not in the list returns an error to the agent.

### Sandbox Environment Restriction

When `sandbox.env_restrict: true`, sensitive environment variables (AWS, Google, Azure, Anthropic API keys) are stripped before command execution.

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
  timeout: 30s
  sandbox:
    allowed_commands: ["go", "git", "ls", "cat", "grep", "find", "mkdir", "rm", "cp", "mv", "make", "docker", "npm", "cargo", "python"]
    env_restrict: true
```

### Read-only exploration
```yaml
nexus.tool.shell:
  timeout: 10s
  sandbox:
    allowed_commands: ["ls", "cat", "grep", "find", "head", "tail", "wc"]
    env_restrict: true
```
