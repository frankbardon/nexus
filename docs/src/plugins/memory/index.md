# Memory Plugins

Memory plugins fall into three groups by what they remember and how:

- **Conversation history** — the active message buffer that ships back to the LLM on every turn. One plugin advertising `memory.history` is active at a time.
- **Compaction** — summarizes older messages so the conversation stays within the context window.
- **Cross-session memory** — persists knowledge between sessions. Two flavors: structured (key-addressed notes) and semantic (embedding-addressed recall).

## Available Memory Plugins

| Plugin | ID | Capability | Purpose |
|--------|----|------------|---------|
| [Simple History](./simple.md) | `nexus.memory.simple` | `memory.history` | Unbounded append-only history; reference/test impl |
| [Capped History](./capped.md) | `nexus.memory.capped` | `memory.history` | Sliding window with JSONL persistence (default `memory.history` provider) |
| [Summary-Buffer History](./summary_buffer.md) | `nexus.memory.summary_buffer` | `memory.history` + `memory.compaction` | Keeps recent N verbatim, LLM-summarizes older inline |
| [Context Compaction](./compaction.md) | `nexus.memory.compaction` | `memory.compaction` | External coordinator that summarizes old messages and emits `memory.compacted` |
| [Long-Term Memory](./longterm.md) | `nexus.memory.longterm` | `memory.longterm` | Cross-session **structured notes**: file-per-entry, YAML frontmatter + markdown, key-addressed, LLM tools (`memory_read`, `memory_write`, `memory_list`, `memory_delete`) |
| [Vector Memory](./vector.md) | `nexus.memory.vector` | `memory.vector` | Cross-session **semantic recall**: embedding-addressed, automatic on every turn, auto-stores compaction summaries |

## How Memory Works

Within a session, conversation memory plugins listen to I/O and tool events, building up a message buffer. When agents need history (for `llm.request`), they query the active `memory.history` provider via `memory.history.query`. When the buffer would exceed the context window, the compaction plugin (if active) summarizes older messages and emits `memory.compacted` so any history buffer that supports adoption can swap in the compacted view.

Across sessions, two independent plugins offer different recall styles:

- **`nexus.memory.longterm`** is the agent's filing cabinet. The LLM deliberately writes structured notes by key; on each session start, an index of titles + tags is injected into the system prompt and the agent reads full content on demand.
- **`nexus.memory.vector`** is the agent's associative memory. On every user input, the plugin embeds the message and pulls semantically-similar past content into the prompt automatically. It also auto-stores compaction summaries so trimmed context stays recallable.

The two coexist and don't share storage — pick one, or activate both for complementary recall styles. They're documented side-by-side in [Vector Memory → vs long-term memory](./vector.md). For the full RAG context, see the [RAG guide](../../guides/rag.md).
