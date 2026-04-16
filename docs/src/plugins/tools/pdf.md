# PDF Reader Tool

Extracts text from PDF files using `poppler-utils` (`pdftotext` and `pdfinfo`).

## Details

| | |
|---|---|
| **ID** | `nexus.tool.pdf` |
| **Tool Name** | `read_pdf` |
| **Dependencies** | None |
| **Requires** | `poppler-utils` installed on the system |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `timeout` | duration | `30s` | Max time for PDF processing |
| `pdftotext_bin` | string | `pdftotext` | Path to the `pdftotext` binary |
| `pdfinfo_bin` | string | `pdfinfo` | Path to the `pdfinfo` binary |
| `save_to_session` | bool | `false` | Save extracted text to session files |
| `save_file_name` | string | *(auto)* | Filename for saved text |

## Tool Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `path` | string | Yes | Path to the PDF file |
| `first_page` | int | No | First page to extract (1-based) |
| `last_page` | int | No | Last page to extract |
| `layout` | bool | No | Preserve original layout |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handles PDF read requests |

### Emits

| Event | When |
|-------|------|
| `tool.result` | Extracted text |
| `tool.register` | Registers the `read_pdf` tool at boot |

## Prerequisites

Install `poppler-utils`:

```bash
# macOS
brew install poppler

# Ubuntu/Debian
sudo apt-get install poppler-utils

# Arch Linux
sudo pacman -S poppler
```

## Example Configuration

```yaml
nexus.tool.pdf:
  timeout: 60s
  save_to_session: true
```
