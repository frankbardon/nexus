# Chromem-go Vector Store

**Plugin ID:** `nexus.vectorstore.chromem`

Backed by [`philippgille/chromem-go`](https://github.com/philippgille/chromem-go) — a pure-Go, in-memory vector database with JSON on-disk persistence. No CGO, no separate service. Each namespace becomes a chromem-go *collection* under the configured path.

Suitable up to roughly **low millions of chunks** depending on dimensionality and host RAM. Beyond that, migrate to a dedicated backend (`sqlite-vec`, `pgvector`, Qdrant) — the `vector.store` event surface is identical, so consumers don't change.

## Configuration

```yaml
nexus.vectorstore.chromem:
  path: ~/.nexus/vectors    # default
  compress: false           # gzip-compress collection files on disk
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `path` | string | `~/.nexus/vectors` | Directory for the chromem DB. One subdirectory per namespace. Created if missing. |
| `compress` | bool | `false` | Gzip-compress collection files. Saves disk at the cost of a little CPU on read/write. |

`~` is expanded relative to `$HOME`.

## On-disk layout

```
~/.nexus/vectors/
├── kb/                          # one chromem collection per namespace
│   ├── ...documents
│   └── metadata
├── project-docs/
│   └── ...
└── memory-react/
    └── ...
```

Plus the embedding cache (managed by `nexus.rag.ingest`, not this plugin) at `~/.nexus/vectors/_cache/`. Keeping cache and store under the same root makes them easy to back up and easy to wipe together when switching embedding models.

## Behavior

### Upsert

chromem-go has no native upsert. The plugin implements it as `Delete(IDs)` followed by `Add(...)`. Stable IDs (which `nexus.rag.ingest` produces from `<sha256-prefix-of-abspath>-<chunk-idx>`) make re-ingest idempotent.

### Query

Cosine similarity over normalized vectors. The plugin clamps `K` to the namespace size to avoid chromem panicking when asked for more results than exist, and short-circuits empty namespaces with a zero-results response (no error).

### Delete

`VectorDelete` ignores unknown IDs. Watch-mode deletes in `nexus.rag.ingest` rely on this — they delete a fixed upper bound of IDs (`0..4095`) under the file's path-hash without first querying how many actually exist.

### Drop namespace

`VectorNamespaceDrop` removes the entire collection — both in-memory and the on-disk directory. Idempotent: dropping an unknown namespace returns success.

## Concurrency

The plugin holds a mutex around `GetOrCreateCollection` and `GetCollection`/`DeleteCollection`. Per-collection chromem operations (`Add`, `QueryEmbedding`, `Delete`) are themselves thread-safe, so reads and writes against an existing namespace fan out without serialization.

## Events

Subscribes to all four `vector.*` events. See [Vector Stores](./index.md#bus-contract) for payload shapes.

| Event | Direction | Payload |
|-------|-----------|---------|
| `vector.upsert` | Any → plugin | `*VectorUpsert` |
| `vector.query` | Any → plugin | `*VectorQuery` |
| `vector.delete` | Any → plugin | `*VectorDelete` |
| `vector.namespace.drop` | Any → plugin | `*VectorNamespaceDrop` |

## Limits and caveats

- **In-memory at runtime.** chromem-go loads the full collection into RAM on first access. A namespace with 100k chunks at 1536 dims is ~600 MB resident. Watch the process RSS when scaling up.
- **Single-process.** Two engines pointed at the same `path` will race on writes. Use one process per `path`, or pick a backend with proper transactional guarantees.
- **No native upsert / no atomic transactions.** The delete-then-add pattern is exposed as a single call from the consumer's perspective but isn't transactional under the hood. A crash between delete and add can leave a chunk's slot empty until the next ingest. Re-running ingest fixes it; the embedding cache makes that cheap.
- **No range or full-text filter.** `VectorQuery.Filter` is exact-match string equality only. If you need richer filters, that's a v2 backend feature.
