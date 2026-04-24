# Vector Stores

Plugins that advertise the **`vector.store`** capability. Persist embedding vectors with their content and metadata, and answer nearest-neighbor queries against a named subset (a *namespace*).

## Why a capability

Vector backends differ wildly — pure-Go in-memory stores, embedded SQLite extensions, Postgres extensions, dedicated services like Qdrant or Weaviate. Splitting `vector.store` into its own capability lets the same `nexus.rag.ingest`, `nexus.tool.knowledge_search`, and `nexus.memory.vector` plugins work against any of them, with the choice driven entirely by config.

## Built-in adapters

| Plugin ID | Backend | Notes |
|-----------|---------|-------|
| [`nexus.vectorstore.chromem`](./chromem.md) | `philippgille/chromem-go` | Pure Go, in-memory with JSON on-disk persistence. No CGO, no service to run. Suitable up to low millions of chunks. |

## Namespaces

Every operation takes a `Namespace` string. Namespaces isolate logically distinct knowledge — a project's docs, a shared knowledge base, per-agent semantic memory. They are cheap to create (chromem-go creates the collection on first upsert) but **do not validate against an allow-list**: the consumer plugin is responsible for namespace policy.

`nexus.tool.knowledge_search` enforces an allow-list at the tool level. `nexus.memory.vector` namespaces by `InstanceID`. The vector store itself just routes by name.

## Bus contract

Four event types cover the interface:

```go
// pkg/events/vector.go
type VectorUpsert         struct { Namespace string; Docs []VectorDoc;
                                   Provider string; Error string }
type VectorQuery          struct { Namespace string; Vector []float32;
                                   K int; Filter map[string]string;
                                   Matches []VectorMatch; Provider string; Error string }
type VectorDelete         struct { Namespace string; IDs []string;
                                   Provider string; Error string }
type VectorNamespaceDrop  struct { Namespace string;
                                   Provider string; Error string }

type VectorDoc   struct { ID string; Vector []float32; Content string; Metadata map[string]string }
type VectorMatch struct { ID string; Content string; Metadata map[string]string; Similarity float32 }
```

Emitted as **pointer payloads** on `vector.upsert`, `vector.query`, `vector.delete`, `vector.namespace.drop`. Adapters mutate in place and set `req.Provider` / `req.Error` (and `req.Matches` on query) before `Emit` returns.

### Upsert semantics

`VectorUpsert` replaces documents with matching IDs. Adapters without native upsert (chromem-go) implement this as `Delete(IDs)` followed by `Add`. Re-ingesting the same path with `nexus.rag.ingest` is therefore safe and idempotent.

### Metadata constraint

`VectorDoc.Metadata` is `map[string]string` — the common denominator across chromem, sqlite-vec, pgvector, Qdrant. Numeric or boolean metadata should be stringified at the producer.

### Query filter

`VectorQuery.Filter` is an exact-match metadata filter. `{"source": "docs/foo.md"}` returns only chunks whose stored metadata's `source` field equals `docs/foo.md`. Empty filter returns over the whole namespace. Range / contains / regex filters are backend-dependent and not exposed in v1.

### Idempotent drop

`VectorNamespaceDrop` succeeds whether the namespace exists or not. Adapters must not error on missing namespaces; the consumer is allowed to call drop defensively at startup or shutdown.
