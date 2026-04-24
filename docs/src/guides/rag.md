# Retrieval-Augmented Generation (RAG)

Nexus has first-class RAG support built from two primitive capabilities (`embeddings.provider`, `vector.store`) and three consumer plugins (`nexus.rag.ingest`, `nexus.tool.knowledge_search`, `nexus.memory.vector`). Each layer is swappable via the standard capability/adapter system, so adding a new vector backend or embedding provider doesn't ripple through any other code.

This guide walks through the common path: stand up a knowledge base, point an agent at it, and let the LLM cite sources. Reference docs for each plugin live under [Plugin Reference](../plugins/overview.md) — the goal here is to give you a working setup in ten minutes.

## What you get

- **Knowledge-base search**: `knowledge_search` tool the LLM calls to pull supporting passages from configured namespaces. Returns top-k chunks with similarity scores and source paths for citation.
- **Per-agent semantic memory**: automatic recall of relevant past content (compaction summaries, explicit stores) on every user turn, injected into the system prompt.
- **Bulk-ingest CLI**: `nexus ingest` boots a minimal engine to chunk, embed, and store files without a running agent.
- **Watch-mode ingestion**: `fsnotify`-backed directory watchers re-ingest files on write and drop chunks on delete.

## Architecture at a glance

```
                        ┌──────────────────────────────────┐
                        │        embeddings.provider       │  ← capability
                        │  (nexus.embeddings.openai/...)   │
                        └──────────────────────────────────┘
                                       ▲
                                       │ embeddings.request
            ┌──────────────────────────┼──────────────────────────┐
            │                          │                          │
┌─────────────────────┐   ┌──────────────────────┐   ┌──────────────────────┐
│  nexus.rag.ingest   │   │ nexus.tool.          │   │ nexus.memory.vector  │
│  (chunk + cache +   │   │ knowledge_search     │   │ (per-agent recall)   │
│   fsnotify watcher) │   │ (LLM-facing tool)    │   │                      │
└─────────────────────┘   └──────────────────────┘   └──────────────────────┘
            │                          │                          │
            │       vector.upsert      │      vector.query        │
            └──────────────────────────┼──────────────────────────┘
                                       ▼
                        ┌──────────────────────────────────┐
                        │           vector.store           │  ← capability
                        │  (nexus.vectorstore.chromem/...) │
                        └──────────────────────────────────┘
```

Everything flows through the bus. No plugin imports another. Adding a new backend means writing one plugin that advertises a capability and subscribes to its events.

## Quickstart

### 1. Provide an OpenAI API key

The default embedding adapter calls OpenAI's `text-embedding-3-small` model. Set the key in env or `.env`:

```bash
export OPENAI_API_KEY=sk-...
```

You can also keep your existing Anthropic key for the LLM — embeddings and chat use different keys.

### 2. Ingest some content

Bulk-load a directory using the CLI subcommand:

```bash
bin/nexus ingest --namespace=kb --glob="*.md" ./docs
```

Output looks like:

```
OK   docs/getting-started/installation.md  (3 chunks, 0 cached)
OK   docs/architecture/overview.md         (5 chunks, 0 cached)
...
ingested 24 file(s), 0 failed; 87 chunks total (0 from cache)
```

Files persist to `~/.nexus/vectors/<namespace>/`. Re-running the same command shows `87 chunks total (87 from cache)` — the embedding cache shortcuts unchanged content.

### 3. Run an agent that knows about it

The `configs/rag.yaml` profile wires everything up:

```bash
bin/nexus -config configs/rag.yaml
```

You can ask the agent questions about the ingested content:

```
> What does the docs say about the plugin lifecycle?

[The agent calls knowledge_search → top hits from architecture/plugin-system.md]

The plugin lifecycle has three phases: Init, Ready, and Shutdown.
During Init, plugins receive a PluginContext with config, bus, logger,
and registry handles. Ready runs after every plugin's Init has
completed... (source: docs/architecture/plugin-system.md, chunk 2)
```

That's the full happy path. The rest of this guide covers the moving parts.

## Plugins

| Plugin ID | Role |
|-----------|------|
| `nexus.embeddings.openai` | `embeddings.provider` — OpenAI embeddings API |
| `nexus.embeddings.mock` | `embeddings.provider` — deterministic hash-based vectors for tests, no network |
| `nexus.vectorstore.chromem` | `vector.store` — chromem-go backend, pure Go, JSON on-disk persistence |
| `nexus.rag.ingest` | Ingestion: chunker + embedding cache + fsnotify watcher; backs `nexus ingest` |
| `nexus.tool.knowledge_search` | LLM-facing search tool; mirrors `web_search` for configured knowledge bases |
| `nexus.memory.vector` | Per-agent semantic recall; advertises `memory.vector` |

## Common configurations

### Knowledge base only

The agent has a `knowledge_search` tool but no automatic recall. Good when you want full LLM control over when retrieval happens.

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.embeddings.openai
    - nexus.vectorstore.chromem
    - nexus.rag.ingest             # only needed if you ingest at runtime
    - nexus.tool.knowledge_search

  nexus.tool.knowledge_search:
    namespaces: [kb]
    default_namespaces: [kb]
    top_k: 5
```

### Per-agent semantic memory only

No tool — the agent just gets relevant past context auto-injected on every turn.

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.embeddings.openai
    - nexus.vectorstore.chromem
    - nexus.memory.vector

  nexus.memory.vector:
    top_k: 5
    auto_store_compaction: true
```

The plugin auto-stores compaction summaries (so trimmed context stays recallable) and on every `io.input` queries the agent's namespace and renders hits as a `<recalled_memory>` XML block in the next system prompt. Conservative by default: it does *not* auto-store every user message — `auto_store_user_input` is `false` until you opt in.

### Watch-mode ingestion

For a docs site or knowledge wiki, point a watcher at a directory:

```yaml
nexus.rag.ingest:
  chunker:
    size: 1000
    overlap: 200
  watch:
    - path: ./docs
      glob: "*.md"
      namespace: project-docs
    - path: ./knowledge-base
      namespace: kb
```

`fsnotify` re-ingests on write (debounced 250ms to coalesce save bursts) and drops the file's chunks on delete. Chunk IDs are deterministic (`<sha256-prefix-of-abspath>-<chunk-idx>`) so the upsert path is idempotent and deletes don't need a preceding query.

### Both: tool + memory + watcher

The default `configs/rag.yaml` profile activates everything. Tune namespaces and salience knobs to taste. See [Built-in Profiles](../configuration/profiles.md#ragyaml) for the full file.

## Embedding provider

OpenAI is the only built-in adapter today. To use it:

```yaml
nexus.embeddings.openai:
  api_key_env: OPENAI_API_KEY
  # model: text-embedding-3-small    # default; -large is 3x cost
  # dimensions: 1536                 # provider-default; smaller = cheaper, less accurate
  # base_url: https://api.openai.com/v1/embeddings  # override for Azure / proxies
```

For tests or offline development, swap in the mock:

```yaml
plugins:
  active:
    - nexus.embeddings.mock
    # ...

  nexus.embeddings.mock:
    dimensions: 128   # default; size doesn't matter for tests
```

`nexus.embeddings.mock` produces deterministic hash-based vectors. Same text in → same vector out, no network. Used by the integration test suite and useful for exercising the rest of the stack without burning OpenAI credits.

To add a new provider (Anthropic Voyage, Ollama, local models, etc.) you write one plugin: advertise `embeddings.provider`, subscribe to `embeddings.request`, fill `req.Vectors` / `req.Provider` / `req.Error` in place. See [Bus contract](#bus-contract) below.

## Vector store

`chromem-go` is the only built-in adapter. Pure Go, no CGO, in-memory with JSON on-disk persistence.

```yaml
nexus.vectorstore.chromem:
  path: ~/.nexus/vectors    # default
  compress: false           # gzip-compress collections on disk
```

Namespaces map 1:1 to chromem collections. Each namespace becomes a subdirectory under `path`. Suitable up to **low millions of chunks**; swap for `sqlite-vec` / `pgvector` / Qdrant when you outgrow it. The `vector.store` event surface stays identical.

## Choosing namespaces

Namespaces isolate logically distinct knowledge. Three common shapes:

| Use case | Example namespace | Notes |
|----------|-------------------|-------|
| Project documentation | `project-docs` | One namespace per project; switch profiles to switch context. |
| Shared knowledge base | `kb` | Cross-project FAQ, runbooks, playbooks. |
| Per-agent semantic memory | `memory-{InstanceID}` | Auto-derived by `nexus.memory.vector` — sanitized InstanceID. |

`nexus.tool.knowledge_search` enforces an allow-list: the LLM-supplied `namespaces` arg is intersected with the configured `namespaces` list, so the tool can't reach into stores it shouldn't see. The `memory.vector` namespace is intentionally separate from any knowledge-base namespace — semantic memory is an internal detail you wouldn't expose as a citable source.

## Embedding cache

`nexus.rag.ingest` caches `content-hash → vector` on disk under `~/.nexus/vectors/_cache/` by default. Cache hits skip the embedding API call entirely.

The cache is keyed on content hash only — *not* per embedding model. If you switch embedding models, drop the cache directory. (Mixing vectors from two models in one namespace is incoherent anyway, so re-ingest is required either way.)

```bash
rm -rf ~/.nexus/vectors/_cache/        # after switching embedding model
nexus ingest --namespace=kb ./docs     # rebuild
```

## Knowledge-search tool

The tool registers as `knowledge_search` (mirroring `web_search`), described to the LLM as searching configured knowledge bases. Behavior on invoke:

1. Embed the query.
2. Fan out `vector.query` across the requested-and-allowed namespaces.
3. Merge hits, sort by similarity, truncate to top-k.
4. Return JSON with `rank`, `namespace`, `similarity`, `source`, `chunk_idx`, `content`, and (optional) `metadata` — enough for the LLM to cite.

Configuration knobs:

```yaml
nexus.tool.knowledge_search:
  namespaces: [kb, project-docs]      # required allow-list
  default_namespaces: [kb]            # used when LLM omits the arg
  top_k: 5                            # default; LLM may override per-call up to maxTopK=50
  include_metadata: true              # whether to return raw metadata map
  embedding_model: ""                 # optional pin; empty = provider default
```

The system-prompt example in `configs/rag.yaml` nudges the LLM toward calling `knowledge_search` first on factual questions — adjust this prompt for your domain so the tool gets used at the right times. Without a hint, the LLM may try to answer from training data on questions you wanted grounded in the KB.

## Vector memory

`nexus.memory.vector` runs three behaviors:

1. **On `io.input`** (priority 10, before the agent's handler at 50): embed the user message → query the agent's namespace → stash hits → render them as a `<recalled_memory>` block via the `PromptRegistry` for the next `llm.request`.
2. **On `memory.compacted`**: auto-store the compaction summary so past context stays recallable after the history buffer trims it.
3. **On `memory.vector.store`**: explicit-store entry point for tools or plugins that want to record content deliberately.

```yaml
nexus.memory.vector:
  # namespace: memory-default               # default: "memory-{InstanceID}"
  top_k: 5
  min_similarity: 0.3                       # filter out weak hits from the prompt
  auto_store_compaction: true               # auto-write summaries on memory.compacted
  auto_store_user_input: false              # off by default — you usually don't want this
  embedding_model: ""                       # optional pin
  section_priority: 45                      # prompt ordering vs. other sections
```

### Vector memory vs long-term memory

Both plugins persist across sessions, but they solve different problems and **don't share storage**:

| | `nexus.memory.longterm` | `nexus.memory.vector` |
|---|---|---|
| Address by | key (LLM-managed) | embedding (semantic similarity) |
| Storage | one markdown file per entry, YAML frontmatter | vector store namespace |
| LLM tools | `memory_read` / `memory_write` / `memory_list` / `memory_delete` | none — automatic |
| Best for | structured notes, preferences, facts to remember exactly | fuzzy recall of past content, summaries, topics |

They coexist. Long-term is the agent's deliberate filing cabinet; vector memory is the agent's automatic associative recall. Activate one or both; nothing in either plugin references the other.

## Tool surface

### `knowledge_search`

| Argument | Type | Required | Notes |
|----------|------|----------|-------|
| `query` | string | yes | The semantic query. Phrase as you'd phrase a search. |
| `namespaces` | array of string | no | Subset of allowed namespaces. Defaults to `default_namespaces`. |
| `k` | int | no | Max results. Defaults to plugin's `top_k`, capped at 50. |

Output is a JSON document; the LLM is expected to read it and quote source paths. Example:

```json
{
  "query": "what is the plugin lifecycle?",
  "results": [
    {
      "rank": 1,
      "namespace": "project-docs",
      "similarity": 0.84,
      "source": "docs/architecture/plugin-system.md",
      "chunk_idx": "2",
      "content": "Each plugin goes through three lifecycle phases...",
      "metadata": { "source": "docs/architecture/plugin-system.md", "chunk_idx": "2", "chunk_size": "768" }
    }
  ]
}
```

## Bus contract

Five event types make the whole stack work. Knowing the shape is enough to write your own adapter.

```go
// pkg/events/embeddings.go — primitive
type EmbeddingsRequest struct {
    Texts      []string
    Model      string
    Dimensions int

    Vectors  [][]float32
    Provider string
    Usage    EmbeddingsUsage
    Error    string
}

// pkg/events/vector.go — primitive
type VectorUpsert struct { Namespace string; Docs []VectorDoc; Provider string; Error string }
type VectorQuery  struct { Namespace string; Vector []float32; K int; Filter map[string]string;
                           Matches []VectorMatch; Provider string; Error string }
type VectorDelete struct { Namespace string; IDs []string; Provider string; Error string }
type VectorNamespaceDrop struct { Namespace string; Provider string; Error string }

// pkg/events/rag.go — ingest
type RAGIngest        struct { Path string; Namespace string; Metadata map[string]string;
                               Provider string; Chunks int; SkippedCached int; Error string }
type RAGIngestDelete  struct { Path string; Namespace string;
                               Provider string; Deleted int; Error string }

// pkg/events/memory_vector.go — explicit store
type VectorMemoryStore struct { Content string; Source string; Metadata map[string]string;
                                Provider string; Error string }
```

Every payload is emitted as a **pointer** so providers fill results in place before `Emit` returns. This is the same sync pointer-fill pattern used by `search.provider`, `tool.catalog.query`, and `memory.history.query`.

Adapter handlers must:

1. Ignore the event if `payload.Provider != ""` (someone else already answered).
2. Set `payload.Provider = pluginID` whether the call succeeded or failed.
3. Set either `payload.Error` **or** the result fields, not both.

## Bulk-ingest CLI reference

```
nexus ingest --namespace=NAME [flags] PATH [PATH...]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--namespace` | *(required)* | Target namespace in the vector store. |
| `--glob` | *(empty)* | Filename glob — matched against the path-relative-to-root and the basename. |
| `--concurrency` | `4` | Max files ingested in parallel. |
| `--chunk-size` | `1000` | Chunker target size (chars). |
| `--chunk-overlap` | `200` | Chunker overlap (chars). |
| `--vector-path` | `~/.nexus/vectors` | Vector store directory. |
| `--cache-path` | `~/.nexus/vectors/_cache` | Embedding cache directory. |
| `--model` | `text-embedding-3-small` | Embedding model. |

The subcommand boots a minimal engine with **only** `nexus.embeddings.openai`, `nexus.vectorstore.chromem`, and `nexus.rag.ingest` active — no agent, no LLM, no IO. Useful for offline pre-loading before an agent starts.

## Gate interaction

The retrieve tool emits through the normal vetoable `before:tool.result` hook, so existing gates apply unchanged:

| Gate | Effect on RAG |
|------|---------------|
| `nexus.gate.content_safety` | PII/secret redaction or blocking on retrieved chunk content. |
| `nexus.gate.output_length` | Truncates oversized search outputs with an LLM retry. |
| `nexus.gate.tool_filter` | Allow-/block-list `knowledge_search` per profile. |
| `nexus.gate.prompt_injection` | Runs on the LLM's input, including anything pasted back from search results. |

Per-namespace access policy lives in `nexus.tool.knowledge_search.namespaces`, not a gate. Policy at the tool layer is cheaper and clearer than a custom gate.

## Writing a new adapter

The pattern is identical for embedding providers and vector stores. Here's a minimal `embeddings.provider`:

```go
package myembed

import (
    "github.com/frankbardon/nexus/pkg/engine"
    "github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.embeddings.myembed"

type Plugin struct{ /* ... */ }

func (p *Plugin) Capabilities() []engine.Capability {
    return []engine.Capability{{Name: "embeddings.provider"}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
    p.bus = ctx.Bus
    p.bus.Subscribe("embeddings.request", p.handle,
        engine.WithPriority(50), engine.WithSource(pluginID))
    return nil
}

func (p *Plugin) handle(e engine.Event[any]) {
    req, ok := e.Payload.(*events.EmbeddingsRequest)
    if !ok || req.Provider != "" {
        return
    }
    vectors, err := p.callMyAPI(req.Texts, req.Model)
    req.Provider = pluginID
    if err != nil {
        req.Error = err.Error()
        return
    }
    req.Vectors = vectors
}
```

Register the factory in [`pkg/engine/allplugins/register.go`](https://github.com/frankbardon/nexus/blob/main/pkg/engine/allplugins/register.go) and add a config block. Consumers (ingest, retrieve tool, vector memory) pick it up automatically via the capability system. No changes elsewhere.

A vector-store adapter follows the same shape but subscribes to all four `vector.*` events. See [`plugins/vectorstore/chromem/plugin.go`](https://github.com/frankbardon/nexus/blob/main/plugins/vectorstore/chromem/plugin.go) for the reference implementation — about 240 lines including all four handlers.

## Out of scope (for now)

These weren't in the initial RAG pass and are tracked separately:

- **Auto-retrieve gate** on `before:llm.request` (system injection without LLM tool call). Distinct UX questions around latency budget and provenance — the `knowledge_search` tool path makes the retrieval visible to the LLM and the user, which is usually what you want.
- **Re-ranking** (cross-encoder, LLM-based). Future `rag.reranker` capability.
- **Additional vector store backends** (`sqlite-vec`, `pgvector`, Qdrant). The `vector.store` event surface is designed so they slot in without breaking changes.
- **Additional embedding providers** (Anthropic Voyage, Ollama, local sentence-transformers).

## Troubleshooting

- **`no embeddings provider answered — check that a plugin advertising 'embeddings.provider' is active`**
  Activate `nexus.embeddings.openai` (production) or `nexus.embeddings.mock` (testing).

- **`expected N vectors, got 0`**
  The provider returned an error. Look up at the previous log line for the underlying API failure (HTTP status, key issue, model name typo).

- **`ingest: read FILE: permission denied`**
  The plugin runs as the same user as the engine. Make sure files are readable and that watch-mode `path:` entries point at directories that don't shift permissions.

- **Cache hits never happen**
  Cache lives at `~/.nexus/vectors/_cache/` by default. Check that `cache_dir` (if overridden) is writable and persistent. Also remember the cache is keyed on content hash, so even one trailing newline change invalidates an entry.

- **Search returns empty matches when content was definitely ingested**
  Check the namespace in your `knowledge_search` config matches the namespace you ingested into. Namespace mismatches don't error — they just return zero results.

- **Sliced docs site looks like noise after retrieval**
  The default chunker is generic prose-tuned (1000/200). For code, mixed prose+code, or very short pages, try smaller chunks (`chunk-size: 600`, `chunk-overlap: 120`). Per-content-type chunkers are a future enhancement.
