# Memory Plugins

Memory plugins manage conversation history and context window size. They persist messages across turns and can compress older context to stay within token limits.

## Available Memory Plugins

| Plugin | ID | Purpose |
|--------|----|---------|
| [Simple History](./simple.md) | `nexus.memory.simple` | Unbounded append-only history; reference/test impl |
| [Capped History](./capped.md) | `nexus.memory.capped` | Sliding window with JSONL persistence (default `memory.history` provider) |
| [Summary-Buffer History](./summary_buffer.md) | `nexus.memory.summary_buffer` | Keeps recent N verbatim, LLM-summarizes older inline |
| [Context Compaction](./compaction.md) | `nexus.memory.compaction` | External coordinator that summarizes old messages and emits `memory.compacted` |
| [Long-Term Memory](./longterm.md) | `nexus.memory.longterm` | Cross-session memory persistence via file-per-entry storage |

## How Memory Works

Memory plugins listen to I/O and tool events, building up a message buffer. When agents need conversation history (for `llm.request`), they query the memory plugin via `memory.query`.

The compaction plugin monitors context size and automatically summarizes older messages when thresholds are exceeded, replacing them with a condensed summary.

The long-term memory plugin operates independently from conversation memory. It persists knowledge across sessions as individual markdown files with YAML frontmatter, providing LLM tools for CRUD and injecting a lightweight memory index into the system prompt.
