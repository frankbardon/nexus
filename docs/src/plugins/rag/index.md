# RAG

Plugins that consume `embeddings.provider` and `vector.store` to build a retrieval pipeline.

For an end-to-end walkthrough — wiring a profile, ingesting docs, and getting an agent to cite sources — see the [RAG guide](../../guides/rag.md).

## Plugins

| Plugin ID | Role |
|-----------|------|
| [`nexus.rag.ingest`](./ingest.md) | Reads files, chunks them, embeds via `embeddings.provider`, upserts via `vector.store`. Two entry points (event + watch mode) sharing one codepath. Backs the `nexus ingest` CLI subcommand. |

Two more RAG consumers live elsewhere in the plugin tree:

- [`nexus.tool.knowledge_search`](../tools/knowledge_search.md) — LLM-facing tool the agent calls explicitly.
- [`nexus.memory.vector`](../memory/vector.md) — automatic per-agent semantic recall on every turn.

They're under `plugins/tools/` and `plugins/memory/` because they fit those existing categories — the RAG namespace is reserved for ingestion machinery and any future RAG-specific glue.
