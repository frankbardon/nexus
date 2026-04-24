# OpenAI Embeddings

**Plugin ID:** `nexus.embeddings.openai`

Advertises the `embeddings.provider` capability backed by OpenAI's `/v1/embeddings` API. Supports `text-embedding-3-small` (default), `text-embedding-3-large`, and `text-embedding-ada-002`. Compatible with Azure OpenAI and OpenAI-compatible proxies via `base_url`.

## Configuration

```yaml
nexus.embeddings.openai:
  api_key_env: OPENAI_API_KEY        # default; ignored if api_key is set directly
  # api_key: sk-...                  # direct literal (avoid checking in)
  # model: text-embedding-3-small    # default
  # dimensions: 1536                 # provider default; smaller = cheaper
  # base_url: https://api.openai.com/v1/embeddings
  # timeout: 30s
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_key` | string | — | OpenAI API key (direct literal). |
| `api_key_env` | string | `OPENAI_API_KEY` | Env var name to read the key from when `api_key` is unset. |
| `model` | string | `text-embedding-3-small` | Embedding model. |
| `dimensions` | int | *(provider default)* | Optional truncation. `text-embedding-3-*` accepts arbitrary smaller dims; older models ignore this. |
| `base_url` | string | `https://api.openai.com/v1/embeddings` | Override for Azure / proxies. |
| `timeout` | duration | `30s` | HTTP timeout for the embeddings call. |

## Model picking

| Model | Dim (default) | Notes |
|-------|---------------|-------|
| `text-embedding-3-small` | 1536 | Default. Cheap, good enough for most retrieval. |
| `text-embedding-3-large` | 3072 | ~3× the price; better on hard semantic tasks, more storage. |
| `text-embedding-ada-002` | 1536 | Legacy. Cheaper than `3-large`, less accurate than `3-small`. |

You can request a smaller dimensionality from `text-embedding-3-*` to trade some quality for storage and search latency:

```yaml
nexus.embeddings.openai:
  model: text-embedding-3-large
  dimensions: 1024     # roughly 3-small accuracy at 2/3 the storage
```

## Switching models

The embedding cache (in `nexus.rag.ingest`) is keyed on content hash, not model. Mixing vectors from two models in one namespace produces nonsense rankings, so when you change `model` or `dimensions` you should:

```bash
rm -rf ~/.nexus/vectors/_cache/
nexus ingest --namespace=kb ./docs    # re-embed under the new model
```

## Events

| Event | Direction | Payload |
|-------|-----------|---------|
| `embeddings.request` | Any → plugin | `*EmbeddingsRequest` |

Pointer-fill: the plugin sets `req.Vectors`, `req.Provider`, `req.Model`, `req.Usage` (or `req.Error`) in place.

## Errors

- **`no API key configured (set api_key in config or OPENAI_API_KEY env var)`** at boot — set the key in env or in the config block.
- **`openai returned HTTP 401`** — invalid or expired key. Check the variable name in `api_key_env` is the one actually set in the shell.
- **`expected N embeddings, got M`** — the API returned a different number of vectors than the input text count. The adapter retries the request internally; if you see this surfaced, the API call partially failed mid-stream. Re-running ingest will pick up where the cache stopped.
