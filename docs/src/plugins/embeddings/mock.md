# Mock Embeddings

**Plugin ID:** `nexus.embeddings.mock`

A deterministic, zero-I/O `embeddings.provider` for tests and offline development. Same text in → same vector out. No network, no API key, sub-millisecond per call.

The vectors are derived from the SHA-256 hash of the input text. Two different inputs map to two different (but stable) vectors; the same input always maps to the same vector. The output is unit-normalized so backends like chromem-go that require normalized inputs accept it directly.

This plugin ships in the main binary alongside production providers — same pattern as `nexus.io.test`. It is opt-in; to use it just include it in `plugins.active`.

## Configuration

```yaml
nexus.embeddings.mock:
  # dimensions: 128            # default
  # model: mock-embedding      # default; echoed back in EmbeddingsRequest.Model
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dimensions` | int | `128` | Output vector dimensionality. Per-call override via `EmbeddingsRequest.Dimensions`. |
| `model` | string | `mock-embedding` | Echoed back as `EmbeddingsRequest.Model`. |

## When to use it

- **Integration tests.** The RAG integration tests under `tests/integration/rag_test.go` use this plugin so the suite runs without an API key and without spending money. See [Integration Testing](../../guides/integration-testing.md).
- **Adapter and storage development.** When iterating on a new vector store backend, you don't want a real embedding API in the loop. Wire up `nexus.embeddings.mock` to keep the focus on the storage side.
- **Pipeline smoke tests.** Boot a CLI ingest with mock embeddings to confirm chunking, persistence paths, and watch-mode wiring all work, then swap in a real provider.

## When not to use it

The mock has **no semantic structure** — "cat" and "kitten" produce vectors as different as "cat" and "transistor radio". Tests that depend on similarity rankings for *meaning* will give wrong-shaped signals. The included integration tests work around this by querying with the exact stored content (which has bit-for-bit-identical vectors and produces similarity ≈ 1.0).

Don't use this plugin in production. Don't use it for anything where retrieval quality matters.

## Events

Same as any `embeddings.provider`: subscribes to `embeddings.request`, fills the result in place. See [Embedding Providers](./index.md#bus-contract) for the full payload shape.
