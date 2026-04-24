# RAG Ingest

**Plugin ID:** `nexus.rag.ingest`

Ingests files into the vector store. One plugin, two entry points sharing one codepath:

- **Event mode** — any plugin emits `*events.RAGIngest` on `"rag.ingest"`. Plugin reads → chunks → embeds uncached chunks → upserts. Sync pointer-fill (returns with `Chunks` / `SkippedCached` / `Error` set), plus a notification event `"rag.ingest.result"` for observers.
- **Watch mode** — `fsnotify` watchers declared in config fire the same code path on writes/deletes, debounced 250ms to coalesce save bursts.

The `nexus ingest` CLI subcommand drives event mode from a minimal engine — useful for bulk pre-loading without a running agent. See [RAG guide → bulk-ingest CLI reference](../../guides/rag.md#bulk-ingest-cli-reference).

## Capabilities

`nexus.rag.ingest` requires both:

- `embeddings.provider` — to produce vectors from chunk content
- `vector.store` — to persist them

Both are auto-activated via `Requires()` if not in the user's active list.

## Configuration

```yaml
nexus.rag.ingest:
  chunker:
    size: 1000        # default
    overlap: 200      # default
  cache_dir: ~/.nexus/vectors/_cache    # default
  watch:
    - path: ./docs
      glob: "*.md"
      namespace: project-docs
    - path: ./knowledge-base
      namespace: kb
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `chunker.size` | int | `1000` | Target chunk size in characters. |
| `chunker.overlap` | int | `200` | Overlap between adjacent chunks. Preserves context across boundaries. |
| `cache_dir` | string | `~/.nexus/vectors/_cache` | Embedding cache directory. Content hash → vector. |
| `watch` | list | *(empty)* | fsnotify watch entries. Each requires `path` and `namespace`; `glob` optional. |

## Chunker

Recursive-character splitter: paragraph (`\n\n`) → line (`\n`) → sentence (`. `) → space → hard-split-by-character. Each step backs off when the next chunk would exceed `size`. Overlap is taken as the trailing N characters of the previous chunk seeded into the next, so retrieval matches that span the boundary still hit.

The chunker is internal to this plugin — there is no shared `pkg/rag/chunker`. A second caller would justify promotion; until then, keep it simple.

## Embedding cache

Content-hash → vector, persisted as one JSON file per entry under `cache_dir`. Two-byte directory shard keeps `ls` fast at scale.

The cache is **not keyed per embedding model**. If you change the embedding model or dimensionality, drop `cache_dir` — mixing vectors from two models in a namespace is incoherent regardless of cache state, so re-ingest is required either way:

```bash
rm -rf ~/.nexus/vectors/_cache/
nexus ingest --namespace=kb ./docs
```

## Chunk IDs

Deterministic: `<sha256-prefix-of-abspath>-<chunk-idx>`. Two consequences:

- **Idempotent re-ingest.** Replaying the same file produces identical IDs, so the upsert path replaces existing chunks rather than accumulating duplicates.
- **Cheap deletes.** `nexus.rag.ingest` doesn't track how many chunks were stored per file. Watch-mode deletes drop a generous upper bound of IDs (4096) under the file's path-hash; the underlying vector store ignores unknown IDs. Files with more than 4096 chunks (rare; that's >4 MB at default chunk size) need a wider limit — file an issue.

## Stored metadata

Every chunk lands in the store with this metadata:

| Key | Description |
|-----|-------------|
| `source` | Absolute path to the source file. |
| `path_hash` | First 16 hex chars of the path's SHA-256. Useful for filter queries. |
| `chunk_idx` | Zero-based chunk index (string). |
| `chunk_size` | Length of the chunk content in characters (string). |

Plus any extra `Metadata` fields supplied on the `RAGIngest` payload by the caller.

## Watch mode

Each `watch:` entry installs an `fsnotify` watcher on `path`. On a `Write` or `Create` event, the file is debounced (250ms) and re-ingested. On `Remove` or `Rename`, the file's chunks are dropped via `rag.ingest.delete`.

Globs are matched both relative-to-root and on basename, so `glob: "*.md"` works for both `docs/getting-started/installation.md` and `notes.md` in the watched root.

The watcher runs only when at least one entry is configured. With no `watch:` block, the plugin runs in event mode only.

## Bus contract

| Event | Direction | Payload | Purpose |
|-------|-----------|---------|---------|
| `rag.ingest` | Any → plugin | `*RAGIngest` | Ingest one file. Sync pointer-fill. |
| `rag.ingest.delete` | Any → plugin | `*RAGIngestDelete` | Drop a file's chunks. Sync pointer-fill. |
| `rag.ingest.result` | Plugin → bus | `*RAGIngest` | Notification after `rag.ingest` completes. |
| `embeddings.request` | Plugin → bus | `*EmbeddingsRequest` | Used during ingest. |
| `vector.upsert` | Plugin → bus | `*VectorUpsert` | Used during ingest. |
| `vector.delete` | Plugin → bus | `*VectorDelete` | Used during file delete. |

### `RAGIngest` payload

| Field | Direction | Description |
|-------|-----------|-------------|
| `Path` | input | Absolute or relative path to the file. |
| `Namespace` | input | Target namespace in the vector store. |
| `Metadata` | input | Optional metadata merged into every chunk. |
| `Provider` | output | `nexus.rag.ingest`. |
| `Chunks` | output | Number of chunks produced and upserted. |
| `SkippedCached` | output | How many of those came from the embedding cache. |
| `Error` | output | Non-empty on failure. |

### `RAGIngestDelete` payload

| Field | Direction | Description |
|-------|-----------|-------------|
| `Path` | input | Absolute or relative path. |
| `Namespace` | input | Target namespace. |
| `Provider` | output | `nexus.rag.ingest`. |
| `Deleted` | output | Currently always 0; the underlying vector store drops by ID without counting. |
| `Error` | output | Non-empty on failure. |

## Errors

- **`namespace required`** — `Namespace` was empty on the request payload.
- **`read PATH: permission denied`** — file unreadable. Watch-mode entries should point at directories whose permissions don't change at runtime.
- **`embed: <provider error>`** — the embeddings provider returned an error. Look at the provider's adjacent log line for the underlying cause.
- **`upsert: <store error>`** — the vector store rejected the batch. Most commonly this is a backend running out of disk; check the store's `path` location.
