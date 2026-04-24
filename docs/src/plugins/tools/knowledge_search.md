# Knowledge Search Tool

**Plugin ID:** `nexus.tool.knowledge_search`

LLM-facing tool that searches configured knowledge-base namespaces using semantic (vector) similarity. Named to parallel `web_search` — agents pick `knowledge_search` for configured knowledge and `web_search` for the open web.

For an end-to-end example wiring this tool into an agent, see the [RAG guide](../../guides/rag.md).

## Capabilities

Requires both:

- `embeddings.provider` — to embed the query
- `vector.store` — to run the similarity search

Both auto-activate via `Requires()` if not in the user's active list.

## Configuration

```yaml
nexus.tool.knowledge_search:
  namespaces: [kb, project-docs]      # required allow-list
  default_namespaces: [kb]            # subset used when LLM omits the arg
  top_k: 5
  include_metadata: true
  # tool_name: knowledge_search       # rename the LLM-visible tool if you want
  # embedding_model: ""               # pin a specific model; empty = provider default
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `namespaces` | list of string | *(required, non-empty)* | Allow-list of namespaces the tool can query. |
| `default_namespaces` | list of string | *(equal to `namespaces`)* | Subset used when the LLM doesn't pass `namespaces` arg. |
| `top_k` | int | `5` | Default max results. LLM may override per call up to `maxTopK = 50`. |
| `include_metadata` | bool | `true` | Whether to return the full metadata map alongside structured fields. |
| `tool_name` | string | `knowledge_search` | LLM-visible tool name. Override if it collides with another tool. |
| `embedding_model` | string | *(empty)* | Pin a specific model; empty = use provider default. Should match the model used to ingest. |

If `namespaces` is empty or unset, **boot fails** — the tool refuses to register without a defined allow-list.

## Tool surface

| Argument | Type | Required | Notes |
|----------|------|----------|-------|
| `query` | string | yes | The semantic query. Phrase it as you would a search. |
| `namespaces` | array of string | no | Subset of allowed namespaces. Filtered through the allow-list; unknown names silently dropped. |
| `k` | int | no | Max results. Defaults to `top_k`, capped at `maxTopK = 50`. |

### Output

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
      "content": "Each plugin goes through three lifecycle phases: Init, Ready, and Shutdown...",
      "metadata": {
        "source": "docs/architecture/plugin-system.md",
        "chunk_idx": "2",
        "chunk_size": "768"
      }
    },
    ...
  ]
}
```

Fields:

| Field | Description |
|-------|-------------|
| `rank` | 1-based ranking by similarity, post-merge across namespaces. |
| `namespace` | Which namespace the hit came from. |
| `similarity` | Cosine similarity in `[-1, 1]`. Higher is better. |
| `source` | Lifted from `metadata.source` for convenient citation. |
| `chunk_idx` | Lifted from `metadata.chunk_idx`. |
| `content` | The original chunk text, suitable for quoting. |
| `metadata` | Full metadata map (only when `include_metadata: true`). |

## Behavior

1. Trim and validate the query. Empty queries return an error result (the LLM sees `Error: "query argument required"`).
2. Resolve the namespace set: LLM-supplied names ∩ allow-list, falling back to `default_namespaces` when the LLM didn't specify.
3. Embed the query once via the `embeddings.provider`.
4. Fan out one `vector.query` per namespace, all with the same vector and `K = k`.
5. Merge all hits, sort by similarity descending, truncate to `k`.
6. Format as JSON and emit `tool.result`.

Per-namespace failures log a warning but don't fail the call — partial results are better than zero.

## Choosing `top_k`

Higher = more context for the LLM, lower = less noise. Typical starting points:

| Use case | `top_k` |
|----------|---------|
| Strict citation, narrow questions | 3 |
| General knowledge base | 5 (default) |
| Open-ended research | 10 |

Don't exceed the LLM's context budget. With chunk size 1000 and `top_k=10`, the tool can return ~10 KB of JSON per call — fine for `claude-sonnet-4`, tight for smaller models.

## Prompting the LLM

By default, the LLM may not realize the tool is the right thing to call for factual questions about your knowledge base. Add a hint to the system prompt — `configs/rag.yaml` does this:

```yaml
nexus.agent.react:
  system_prompt: |
    You are a helpful assistant with access to a knowledge base. When the user
    asks a factual question, use the knowledge_search tool first to pull
    supporting chunks from the knowledge base, then cite sources by file path.
```

Adjust this for your domain. Be specific about *what* the knowledge base contains so the LLM knows when to reach for the tool.

## Events

| Event | Direction | Payload |
|-------|-----------|---------|
| `tool.invoke` | Catalog → plugin | `events.ToolCall` |
| `embeddings.request` | Plugin → bus | `*EmbeddingsRequest` |
| `vector.query` | Plugin → bus | `*VectorQuery` |
| `before:tool.result` | Plugin → bus (vetoable) | `*events.ToolResult` |
| `tool.result` | Plugin → bus | `events.ToolResult` |
| `tool.register` | Plugin → bus (Ready) | `events.ToolDef` |

## Gate interaction

Goes through the standard vetoable `before:tool.result` hook, so existing gates apply unchanged: `nexus.gate.content_safety`, `nexus.gate.output_length`, `nexus.gate.tool_filter`, etc.

To restrict which agents can call this tool, use `nexus.gate.tool_filter` per-profile rather than maintaining multiple plugin instances.

## Errors

- **`at least one namespace must be configured under 'namespaces'`** at boot — the allow-list is required and must be non-empty.
- **`query argument required`** in tool output — the LLM called without a `query`.
- **`embed query: ...`** — the embeddings provider returned an error. Check the provider's log line for the underlying cause.
- **`no valid namespaces selected`** — every namespace the LLM passed was filtered out by the allow-list. Either widen the allow-list or fix the system prompt so the LLM only requests valid names.
