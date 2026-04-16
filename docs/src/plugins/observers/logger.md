# Event Logger

Subscribes to all events via a wildcard handler and logs them as structured JSON. Useful for debugging and understanding event flow.

## Details

| | |
|---|---|
| **ID** | `nexus.observe.logger` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `output` | string | `stdout` | Output destination: `stdout`, `stderr`, or `file` |
| `file_path` | string | *(none)* | File path when `output` is `file` |
| `level` | string | `info` | Minimum log level: `debug`, `info`, `warn`, `error` |

## Events

### Subscribes To

Uses `SubscribeAll()` — receives every event in the system.

### Emits

None.

## Example Configuration

```yaml
# Log warnings and errors to stderr
nexus.observe.logger:
  output: stderr
  level: warn

# Log everything to a file
nexus.observe.logger:
  output: file
  file_path: /tmp/nexus-events.log
  level: debug
```
