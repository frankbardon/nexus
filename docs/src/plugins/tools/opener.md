# File Opener Tool

Opens files or URLs using the OS-native default application (e.g., `open` on macOS, `xdg-open` on Linux).

## Details

| | |
|---|---|
| **ID** | `nexus.tool.opener` |
| **Tool Name** | `open_path` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `open_cmd` | string | *(auto-detected)* | Override the open command. Auto-detected: `open` (macOS), `xdg-open` (Linux), `cmd /c start` (Windows) |
| `timeout` | duration | `10s` | Max time to wait for the open command |

## Tool Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `path` | string | Yes | File path or URL to open |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handles open requests |

### Emits

| Event | When |
|-------|------|
| `tool.result` | Confirmation of open action |
| `tool.register` | Registers the `open_path` tool at boot |

## Example Configuration

```yaml
nexus.tool.opener:
  timeout: 10s
```
