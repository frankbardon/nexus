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
| [Session Tags](./session_tags.md) | `nexus.tool.session_tags` | `session_tag_set`, `session_tag_get`, `session_tag_delete`, `session_tag_list` | Opt-in agent read/write access to its own session's general-namespace tags |

## How Tools Work

1. Tool plugin initializes and emits `tool.register` with its tool definition (name, description, JSON Schema parameters)
2. The agent collects registered tools and includes them in `llm.request`
3. When the LLM responds with a tool call, the agent emits `before:tool.invoke` (vetoable for approval)
4. If not vetoed, the agent emits `tool.invoke`
5. The tool plugin handles the invocation and emits `before:tool.result` (vetoable — gates can inspect/block results)
6. If not vetoed, the tool plugin emits `tool.result`
7. The agent feeds the result back to the LLM

### Cancellation

A handler that does real work should run it under a per-call context from the
engine, so the call can be stopped when it is timed out:

```go
ctx, release := p.toolCalls.Begin(context.Background(), tc.ID) // p.toolCalls = pctx.ToolCalls
defer release()
```

The engine cancels that context when a `tool.timeout` names the call's ID
(`nexus.gate.tool_timeout` emits one when a call exceeds its deadline) and when
it stops; `context.Cause(ctx)` is `engine.ErrToolCallTimedOut` or
`engine.ErrToolCallsStopped`. `shell` (process group killed), `run_code` and
`web_fetch` honor it. It is cooperative — a handler that ignores it runs on,
and its late result is vetoed by the gate — and `ToolCalls` is nil-safe, so a
hand-built test `PluginContext` needs no registry. A `Cancel` for an ID with no
live `Begin` does nothing; it does not pre-cancel a later one.

### Unknown tools

If the model calls a tool nobody handles — a hallucinated name, or a client tool
an AG-UI client advertised on an earlier run but not this one — no tool plugin
emits a `tool.result`, which would leave the agent loop waiting forever.
`nexus.tool.catalog` closes that gap. On every `tool.invoke` it checks, first
(priority -100), whether the name is registered or offered by any
`tool.catalog.query` responder; if neither, it waits for the dispatch to finish
(priority 1000) and, unless some subscriber already answered synchronously,
emits an error result through the normal `before:tool.result` → `tool.result`
path:

```json
{"id": "<call id>", "name": "get_weather", "error": "unknown tool: get_weather"}
```

The model sees it on the next iteration like any tool failure. A registered
tool is never touched, even if its handler answers asynchronously (delegate,
remote agents, `ask_user`). Invokes with no `ID` (fire-and-forget seeds) are
skipped. The catalog is auto-activated by ReAct, plan-exec, subagent and
orchestrator, so every agent loop gets this behaviour.

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
