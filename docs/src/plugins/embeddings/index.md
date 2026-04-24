# Embedding Providers

Plugins that advertise the **`embeddings.provider`** capability. Convert text to dense vectors so the rest of the RAG stack — vector stores, ingest, search — can find passages by semantic similarity.

## Why a capability

Embedding APIs are ergonomically similar (text in, vectors out) but the providers and prices vary a lot. Splitting `embeddings.provider` into its own capability lets the same `nexus.rag.ingest`, `nexus.tool.knowledge_search`, and `nexus.memory.vector` plugins work against any backend — OpenAI today, Ollama or a self-hosted model tomorrow — without code changes elsewhere.

This mirrors how `search.provider` and `llm.provider` work. Pin one explicitly with a top-level `capabilities:` block when more than one is active.

## Built-in adapters

| Plugin ID | Backend | Notes |
|-----------|---------|-------|
| [`nexus.embeddings.openai`](./openai.md) | OpenAI embeddings API | `text-embedding-3-*` models. Needs `OPENAI_API_KEY`. Supports `base_url` override for Azure / OpenAI-compatible proxies. |
| [`nexus.embeddings.mock`](./mock.md) | Deterministic hash-based | Zero I/O, no API key. For tests and offline development. |

## Bus contract

```go
// pkg/events/embeddings.go
type EmbeddingsRequest struct {
    Texts      []string
    Model      string
    Dimensions int          // optional truncation hint

    // Filled by the provider:
    Vectors  [][]float32
    Provider string
    Usage    EmbeddingsUsage
    Error    string
}
```

Emitted as a **pointer payload** on `embeddings.request`. The capability-resolved provider fills the result fields in place before `Emit` returns. Adapter handlers must:

1. Ignore the event if `req.Provider != ""` (someone else already answered).
2. Set `req.Provider = pluginID` whether the call succeeded or failed.
3. Set either `req.Error` **or** `req.Vectors`, not both.
4. Echo back the actual model used in `req.Model` so consumers can record what produced the vectors.

See the [RAG guide](../../guides/rag.md#writing-a-new-adapter) for a full adapter skeleton.
