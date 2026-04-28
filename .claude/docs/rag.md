# RAG

Retrieval-augmented generation decomposes into two primitive capabilities plus consumer plugins that compose them. All four pieces are swappable via the standard capability / adapter system.

## Primitives

**`embeddings.provider`** — `Embed(texts, model, dimensions) → vectors`. Consumers emit `*events.EmbeddingsRequest` on `"embeddings.request"`; the provider fills `Vectors / Provider / Model / Usage / Error` in place. Same sync pointer-fill shape as `search.provider`.

- `nexus.embeddings.openai` — OpenAI embeddings API (text-embedding-3-small default; override `model:`/`dimensions:`/`base_url`).

**`vector.store`** — namespace-aware interface: `Upsert / Query / Delete / DropNamespace`. Four event types carry each op:

- `vector.upsert` — `*events.VectorUpsert{Namespace, Docs}`. Adapters without native upsert do delete-by-id-then-add.
- `vector.query` — `*events.VectorQuery{Namespace, Vector, K, Filter}` → fills `Matches`.
- `vector.delete` — `*events.VectorDelete{Namespace, IDs}`.
- `vector.namespace.drop` — `*events.VectorNamespaceDrop{Namespace}`. Idempotent.

Metadata is `map[string]string` — matches the common denominator across chromem-go, sqlite-vec, pgvector, Qdrant. Namespace defaults to "memory-default" when unset; multi-tenant callers should always pass one.

- `nexus.vectorstore.chromem` — `philippgille/chromem-go` backend. Pure Go, in-memory with JSON on-disk persistence. Default path `~/.nexus/vectors/`, one directory per namespace. Fine up to low millions of chunks.

## Consumers

**Ingestion — `nexus.rag.ingest`.** One plugin, two entry points sharing one codepath:

- **Event mode** — any plugin emits `*events.RAGIngest{Path, Namespace, Metadata}` on `"rag.ingest"`. Plugin reads → chunks → embeds uncached chunks → upserts. Sync pointer-fill (returns with `Chunks / SkippedCached / Error` set) plus a `"rag.ingest.result"` notification event for observers.
- **Watch mode** — `fsnotify` watchers declared in config fire the same code path on writes/deletes, debounced at 250ms to coalesce bursts.

Chunks use deterministic IDs `<sha256-prefix-of-abspath>-<chunk-idx>` so re-ingest replaces cleanly and watch-mode deletes don't need a preceding query. The chunker is recursive-character (paragraph → line → sentence → space → char) with configurable size/overlap. Embedding cache lives alongside the vector store at `~/.nexus/vectors/_cache/`, keyed per embedding model (changing `model:` invalidates automatically).

**CLI — `nexus ingest`.** Subcommand dispatched off `os.Args[1]` before flag parsing; the default `-config` flow is unchanged. Boots a minimal engine (embeddings + vectorstore + ingest only), walks the input path with an optional glob, fans out `rag.ingest` events with bounded concurrency. Useful for offline bulk loads without a running agent.

```
nexus ingest --namespace=kb --glob="*.md" ./knowledge-base
```

**Knowledge-search tool — `nexus.tool.knowledge_search`.** LLM-facing tool: embeds query → fans out `vector.query` across selected namespaces → merges + ranks → returns JSON with rank, namespace, similarity, source path, chunk index, content. Named to parallel `web_search` — the LLM picks it when the user asks about configured knowledge, not the web. Namespace access is config-constrained: the plugin declares an allow-list (non-empty, required) and optionally a `default_namespaces` subset. LLM-supplied `namespaces` args are intersected with the allow list.

**Vector memory — `nexus.memory.vector`.** Advertises `memory.vector`. Runs on every turn:

- **On `io.input`** (priority 10, before the agent at 50): embeds user message → queries the agent's namespace → stashes hits → renders them as a `<recalled_memory>` block via the `PromptRegistry` for the next `llm.request`.
- **On `memory.compacted`**: auto-stores the compaction summary so past context stays recallable after the history buffer trims it.
- **On `memory.vector.store`**: explicit-store entry point for tools/plugins that want to record deliberately.

Salience is conservative by default: no per-message auto-writing (`auto_store_user_input: false`). Namespace defaults to `memory-{InstanceID}` — multi-agent desktop shells isolate automatically; override via `namespace:` config.

**Relationship to `nexus.memory.longterm`.** Fully independent. Longterm is the structured notes store — key-addressed, LLM-managed via `memory_read`/`memory_write`/`memory_list`/`memory_delete` tools, files on disk with YAML frontmatter + markdown bodies. Vector memory is embedding-addressed fuzzy recall — salient content in, semantic nearest-neighbor lookup out. They coexist.

## Out of scope (future issues)

- Automatic retrieval gate on `before:llm.request` without tool involvement (latency-budget + provenance UX).
- Re-ranking (cross-encoder, LLM-based) — future `rag.reranker` capability.
- Additional vector store backends (sqlite-vec, pgvector, Qdrant).
- Additional embedding providers (Anthropic voyage, Ollama, local models).
