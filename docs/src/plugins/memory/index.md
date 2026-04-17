# Memory Plugins

Memory plugins manage conversation history and context window size. They persist messages across turns and can compress older context to stay within token limits.

## Available Memory Plugins

| Plugin | ID | Purpose |
|--------|----|---------|
| [Conversation History](./conversation.md) | `nexus.memory.conversation` | Stores and replays conversation messages |
| [Context Compaction](./compaction.md) | `nexus.memory.compaction` | Summarizes old messages to free context space |
| [Long-Term Memory](./longterm.md) | `nexus.memory.longterm` | Cross-session memory persistence via file-per-entry storage |

## How Memory Works

Memory plugins listen to I/O and tool events, building up a message buffer. When agents need conversation history (for `llm.request`), they query the memory plugin via `memory.query`.

The compaction plugin monitors context size and automatically summarizes older messages when thresholds are exceeded, replacing them with a condensed summary.

The long-term memory plugin operates independently from conversation memory. It persists knowledge across sessions as individual markdown files with YAML frontmatter, providing LLM tools for CRUD and injecting a lightweight memory index into the system prompt.
