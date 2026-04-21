# File I/O Tool

Provides file read, write, and listing capabilities with path traversal protection.

## Details

| | |
|---|---|
| **ID** | `nexus.tool.file` |
| **Tool Names** | `read_file`, `write_file`, `list_files` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `base_dir` | string | *(session files dir)* | Root directory for file operations. All paths are resolved relative to this. |
| `allow_external_writes` | bool | `false` | When `false`, `write_file` always writes to the session files directory regardless of `base_dir`. When `true`, `write_file` can write anywhere within `base_dir`. |

## Tools

### `read_file`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `path` | string | Yes | Path to the file to read |
| `offset` | number | No | Byte offset to start reading from (default `0`) |
| `length` | number | No | Maximum bytes to read (default `4096`) |

Reads up to `length` bytes starting at `offset`. Returns a JSON object with `content`, `bytes_read`, `offset`, and `total_size` so callers can page through files larger than the chunk size.

### `write_file`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `path` | string | Yes | Path to write to |
| `content` | string | Yes | Content to write |

Creates or overwrites the file. Emits `session.file.created` or `session.file.updated`.

### `list_files`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `path` | string | Yes | Directory to list |
| `pattern` | string | No | Glob pattern to filter results |

Returns a listing with file names and sizes.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handles file operations |

### Emits

| Event | When |
|-------|------|
| `tool.result` | Operation result |
| `tool.register` | Registers all three tools at boot |
| `core.error` | File operation errors |
| `session.file.created` | New file written |
| `session.file.updated` | Existing file overwritten |

## Security

All paths are resolved relative to `base_dir`. Path traversal attempts (e.g., `../../etc/passwd`) are blocked.

By default, `write_file` is restricted to the session files directory even when a custom `base_dir` is configured. This prevents the agent from modifying files in the working directory unless explicitly opted in via `allow_external_writes: true`.

## Example Configuration

```yaml
# Use session files directory (default)
nexus.tool.file: {}

# Use a specific directory (reads from workspace, writes to session files)
nexus.tool.file:
  base_dir: /home/user/workspace

# Allow writes to the workspace directory
nexus.tool.file:
  base_dir: /home/user/workspace
  allow_external_writes: true
```
