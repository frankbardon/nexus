# Vector Memory

**Plugin ID:** `nexus.memory.vector`
**Capability:** `memory.vector`

Per-agent semantic recall backed by the `vector.store` capability. On every user turn, the plugin embeds the input, queries the agent's namespace for relevant past content, and renders the hits as a `<recalled_memory>` block in the next system prompt. Compaction summaries are auto-stored so trimmed context stays recallable.

Fully independent of [`nexus.memory.longterm`](./longterm.md) — separate code, capability, and storage. They coexist and complement each other:

| | `nexus.memory.longterm` | `nexus.memory.vector` |
|---|---|---|
| Address by | key (LLM-managed) | embedding (semantic similarity) |
| Storage | one markdown file per entry, YAML frontmatter | vector store namespace |
| LLM tools | `memory_read` / `memory_write` / `memory_list` / `memory_delete` | none — automatic |
| Best for | structured notes, preferences, exact facts | fuzzy recall of summaries, topics, prior turns |

Long-term is the agent's deliberate filing cabinet; vector memory is the agent's automatic associative recall.

## Capabilities

- **Provides:** `memory.vector`
- **Requires:** `embeddings.provider`, `vector.store`

Both required capabilities auto-activate via `Requires()` if missing.

## Configuration

```yaml
nexus.memory.vector:
  # namespace: memory-default               # default: "memory-{InstanceID}"
  top_k: 5
  min_similarity: 0.3
  embedding_model: ""                       # optional pin
  auto_store_compaction: true               # auto-write summaries on memory.compacted
  auto_store_user_input: false              # off by default; turning on makes every turn a memory
  section_priority: 45                      # PromptRegistry priority for the recalled-memory block
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `namespace` | string | `memory-{InstanceID}` | Vector store namespace for this agent. The default sanitizes the InstanceID for filesystem safety (`/` → `-`, `:` → `-`). Multi-agent desktop shells isolate automatically. |
| `top_k` | int | `5` | Max hits queried per turn. |
| `min_similarity` | float | `0.0` | Hits below this similarity are dropped from the prompt. `0` disables filtering. |
| `embedding_model` | string | *(provider default)* | Pin a specific embedding model. Should match the model used to populate the namespace. |
| `auto_store_compaction` | bool | `true` | Write the compaction summary on `memory.compacted`. |
| `auto_store_user_input` | bool | `false` | Write every user message. Off by default — usually too noisy. |
| `section_priority` | int | `45` | `PromptRegistry` priority. Higher numbers append later. |

## Behavior

### On `io.input` (priority 10)

Subscribed at priority 10 — earlier than the agent's handler at 50. Sequence:

1. Embed the user message via `embeddings.provider`.
2. Query `vector.store` for top-k hits in the configured namespace.
3. Filter by `min_similarity`.
4. Stash the hits in plugin state.
5. Optionally also store the input itself (`auto_store_user_input`).

When the agent builds the next `llm.request` (priority 50), the `PromptRegistry` calls `buildPromptSection` which renders the stashed hits as XML.

### On `memory.compacted`

When [`nexus.memory.compaction`](./compaction.md) or summary-buffer compaction fires, the plugin extracts the system-role summary message from `CompactionComplete.Messages` and stores it. Past context stays recallable even after the active history buffer trims it.

### On `memory.vector.store` (explicit)

Plugins or tools can write a piece of content explicitly:

```go
req := &events.VectorMemoryStore{
    Content: "User confirmed they prefer dark mode in long sessions",
    Source:  "agent",
    Metadata: map[string]string{"category": "preference"},
}
bus.Emit("memory.vector.store", req)
// req.Provider and req.Error filled in place
```

`Source` is recorded as a metadata tag on the chunk and shown in the rendered prompt block.

## Prompt section

When at least one hit clears `min_similarity`, the plugin renders:

```xml
<recalled_memory>
These items were recalled from your vector memory based on the current user message.
Use them if relevant; ignore them if not.

  <item rank="1" similarity="0.842" source="compaction">
    Summary of prior turns: user is debugging the websocket reconnect logic...
  </item>
  <item rank="2" similarity="0.711" source="explicit">
    User mentioned the bug was specific to the staging environment.
  </item>
</recalled_memory>
```

When no hits clear the filter, the section returns an empty string and `PromptRegistry` skips it — turns with no recall add nothing to the prompt.

## Salience

Salience policy is intentionally conservative:

- **Compaction summaries** — high signal, low volume, written automatically.
- **Explicit stores** — caller-decided.
- **User input** — off by default. Auto-storing every message floods the namespace with low-value content, hurts recall quality, and racks up embedding costs.

If you want richer auto-storage, the recommended path is a separate plugin that subscribes to `agent.turn.complete` (or similar) and emits `memory.vector.store` with whatever salience heuristic fits your domain — e.g., "store assistant messages but not user messages", or "store anything tagged `important: true` in metadata".

## ID scheme

Each stored doc gets a 16-hex-char ID derived from `sha256(content || source || RFC3339Nano-timestamp)`. Time in the hash means re-storing the same content twice produces two entries — vector memory accumulates rather than dedupes. If you want exact-content dedupe, use [`nexus.memory.longterm`](./longterm.md) instead.

## Stored metadata

Every recall doc lands with:

| Key | Description |
|-----|-------------|
| `source` | One of `user`, `compaction`, `explicit`, or whatever caller supplied. |
| `stored` | RFC3339 UTC timestamp of when the doc was written. |
| `session` | Session ID (only on `user` source). |
| `backup_path` | Compaction archive path (only on `compaction` source). |

Plus any extra string metadata supplied on the `VectorMemoryStore` payload.

## Multi-agent shells

In a desktop shell hosting multiple agents, each plugin instance receives a distinct `InstanceID` (see [Plugin System → Instance IDs](../../architecture/plugin-system.md)). The default namespace `memory-{InstanceID}` keeps each agent's recall isolated automatically — no extra configuration needed.

To deliberately share memory across agents, override `namespace` to a common value.

## Events

| Event | Direction | Payload | When |
|-------|-----------|---------|------|
| `io.input` | Bus → plugin | `events.UserInput` | Triggers query + stash. |
| `memory.compacted` | Bus → plugin | `events.CompactionComplete` | Triggers auto-store of summary. |
| `memory.vector.store` | Any → plugin | `*events.VectorMemoryStore` | Explicit store. |
| `embeddings.request` | Plugin → bus | `*events.EmbeddingsRequest` | Used during query and store. |
| `vector.query` | Plugin → bus | `*events.VectorQuery` | Used during turn-start retrieval. |
| `vector.upsert` | Plugin → bus | `*events.VectorUpsert` | Used during store. |

## Limits

- **One namespace per agent instance.** Multi-namespace recall (e.g., separate "facts" and "summaries" namespaces) isn't supported in v1. Run two plugin instances if you need it.
- **No re-ranking.** Hits are returned in cosine-similarity order. A future `rag.reranker` capability will allow LLM-based or cross-encoder rerank passes.
- **No filter.** The query doesn't pass a metadata filter. Adding one is a small change if you have a use case — file an issue.

## Errors

- Errors during query, embed, or store are **logged** as warnings rather than surfaced in the prompt. Recall is best-effort: a missing prompt section is far better than blocking the user's turn on a transient embedding-API blip. Watch the engine log for `vector memory: ...` lines if you suspect recall is silently failing.
