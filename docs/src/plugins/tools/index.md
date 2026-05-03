# Tool Plugins

Tool plugins give the agent capabilities to interact with the outside world. Each tool registers itself via the event bus — agents discover tools automatically.

## Available Tools

| Plugin | ID | Tool Name | Description |
|--------|----|-----------|-------------|
| [Shell](./shell.md) | `nexus.tool.shell` | `shell` | Execute shell commands |
| [File I/O](./file.md) | `nexus.tool.file` | `read_file`, `write_file`, `list_files` | Read, write, and list files |
| [PDF Reader](./pdf.md) | `nexus.tool.pdf` | `read_pdf` | Extract text from PDF files |
| [File Opener](./opener.md) | `nexus.tool.opener` | `open_path` | Open files in the OS default app |
| [Human-in-the-Loop](../control/hitl.md) | `nexus.control.hitl` | `ask_user` | Ask the user a question or approve an action (multi-choice supported) |
| [Code Exec](./code_exec.md) | `nexus.tool.code_exec` | `run_code` | Run a Go script that orchestrates multiple tool calls in one turn |
| [Knowledge Search](./knowledge_search.md) | `nexus.tool.knowledge_search` | `knowledge_search` | Semantic search over configured RAG namespaces; returns top-k chunks with source paths for citation |

## How Tools Work

1. Tool plugin initializes and emits `tool.register` with its tool definition (name, description, JSON Schema parameters)
2. The agent collects registered tools and includes them in `llm.request`
3. When the LLM responds with a tool call, the agent emits `before:tool.invoke` (vetoable for approval)
4. If not vetoed, the agent emits `tool.invoke`
5. The tool plugin handles the invocation and emits `before:tool.result` (vetoable — gates can inspect/block results)
6. If not vetoed, the tool plugin emits `tool.result`
7. The agent feeds the result back to the LLM

## Tool Registration Event

Each tool emits a `tool.register` event with a `ToolDef`:

```go
type ToolDef struct {
    Name        string // Tool name the LLM will use
    Description string // Description shown to the LLM
    Parameters  string // JSON Schema for parameters
}
```

## Approval Flow

The `before:tool.invoke` event is vetoable. I/O plugins (TUI, Browser) can intercept this to show an approval dialog, especially for high-risk operations like shell commands.
