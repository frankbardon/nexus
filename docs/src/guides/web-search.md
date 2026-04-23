# Web Search & Fetch

Nexus gives agents two tools for doing research on the live web:

- **`web_search`** — returns a ranked list of URLs + titles + snippets for a query.
- **`web_fetch`** — downloads one URL and returns its main article text (or raw text for non-article pages).

They are intentionally split so the agent can triage first and only pay for full-page reads on the hits it actually cares about.

## Architecture at a glance

```
LLM turn
   │
   ▼
web_search tool  ──►  search.request event  ──►  search.provider plugin  ──►  HTTP API
                                                                                │
                                           bus fills SearchRequest.Results  ◄──┘
   ▼
LLM picks a URL, calls web_fetch
   │
   ▼
web_fetch tool  ──►  http.Client  ──►  go-readability / x/net/html  ──►  tool.result
```

The web tool plugin (`nexus.tool.web`) is the only plugin that registers tools with the catalog. Search providers are separate plugins that advertise the abstract **`search.provider`** capability. This mirrors how the LLM provider system works: one consumer, pluggable backends, resolved by capability name at boot.

## Plugins

| Plugin ID | Role | Notes |
|-----------|------|-------|
| `nexus.tool.web` | Registers `web_search` and `web_fetch`. Emits `search.request`. | Requires a `search.provider` to be active. Holds the fetch cache. |
| `nexus.search.brave` | `search.provider` via the Brave Search API. | Needs `BRAVE_API_KEY`. Free tier: 2k queries/month. |
| `nexus.search.anthropic_native` | `search.provider` via Anthropic's built-in `web_search` tool. | Needs `ANTHROPIC_API_KEY`. Bills to your Anthropic account at the native web-search rate. Works even when your LLM provider is OpenAI. |
| `nexus.search.openai_native` | `search.provider` via OpenAI's Responses API with the built-in `web_search` tool. | Needs `OPENAI_API_KEY`. Works even when your LLM provider is Anthropic. |

Adding a new adapter later (Tavily, Exa, Serper, Kagi, a private Searx instance) is a matter of writing a plugin that:

1. Advertises `Capabilities() = [{Name: "search.provider"}]`
2. Subscribes to `search.request` and fills the result in place

No changes to `nexus.tool.web` or to any agent are needed. See the existing adapters in [`plugins/search/`](https://github.com/frankbardon/nexus/tree/main/plugins/search) for reference.

## Minimum config

```yaml
plugins:
  active:
    - nexus.agent.react
    - nexus.llm.anthropic
    - nexus.tool.web
    - nexus.search.brave   # pick exactly one search provider

  nexus.search.brave:
    api_key_env: BRAVE_API_KEY
    timeout: 15s

  nexus.tool.web:
    search:
      count: 10
      safe_search: moderate
    fetch:
      timeout: 20s
      max_size: 5MB
      extract_mode: readability
```

If more than one plugin advertising `search.provider` is active, the engine picks the first in `plugins.active` order and emits a `WARN`. Pin one explicitly with a top-level `capabilities:` block:

```yaml
capabilities:
  search.provider: nexus.search.anthropic_native

plugins:
  active:
    - nexus.search.brave                 # still active; pin overrides
    - nexus.search.anthropic_native
```

## Configuration reference

### `nexus.tool.web`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `search.count` | int | `10` | Default max results when the LLM omits `count`. |
| `search.safe_search` | string | `moderate` | Provider-dependent safety filter. `off` / `moderate` / `strict`. |
| `search.language` | string | *(empty)* | BCP-47 language tag forwarded to the provider. |
| `fetch.timeout` | duration | `20s` | Per-fetch HTTP timeout. |
| `fetch.max_size` | bytes | `5MB` | Hard cap on response body. Excess truncates and errors. |
| `fetch.user_agent` | string | `Nexus/0.1 ...` | `User-Agent` header. |
| `fetch.extract_mode` | string | `readability` | `readability` or `raw`. Per-call override via the tool's `extract` arg. |
| `fetch.allowed_domains` | list | *(empty)* | When set, only these domains (and subdomains) are fetchable. |
| `fetch.blocked_domains` | list | *(empty)* | Always denied, even if `allowed_domains` includes them. |
| `fetch.follow_redirects` | bool | `true` | Follow 3xx redirects. |
| `fetch.max_redirects` | int | `5` | Redirect chain limit. |

### `nexus.search.brave`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_key` | string | — | Brave API key (direct literal). |
| `api_key_env` | string | `BRAVE_API_KEY` | Env var name to read the key from when `api_key` is unset. |
| `timeout` | duration | `15s` | HTTP timeout. |

### `nexus.search.anthropic_native`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_key` | string | — | Anthropic API key. |
| `api_key_env` | string | `ANTHROPIC_API_KEY` | Fallback env var. |
| `model` | string | `claude-haiku-4-5-20251001` | Model used for the one-shot search call. Haiku keeps it cheap. |
| `timeout` | duration | `30s` | HTTP timeout (search requires a full LLM round trip). |

### `nexus.search.openai_native`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_key` | string | — | OpenAI API key. |
| `api_key_env` | string | `OPENAI_API_KEY` | Fallback env var. |
| `base_url` | string | `https://api.openai.com/v1/responses` | Override for Azure/compatible endpoints. |
| `model` | string | `gpt-4o-mini` | Model used for the Responses call. |
| `timeout` | duration | `30s` | HTTP timeout. |

## Tool surface

### `web_search`

| Argument | Type | Required | Notes |
|----------|------|----------|-------|
| `query` | string | yes | The search query. |
| `count` | int | no | Max results. Defaults to `search.count`. |
| `freshness` | string | no | `day` / `week` / `month`. Adapter best-effort. |
| `language` | string | no | BCP-47 tag. Adapter best-effort. |

Output format: a numbered list of results followed by a JSON payload in the same string. The JSON carries the raw `SearchResult` structs for consumers that want to parse programmatically (e.g. a downstream tool chained via `run_code`).

### `web_fetch`

| Argument | Type | Required | Notes |
|----------|------|----------|-------|
| `url` | string | yes | Absolute `http` or `https` URL. |
| `extract` | string | no | `readability` (default) or `raw`. |

Output is text with a small header block (`URL`, `Title`, `Byline`, `Summary`, `Extract`). Readability failures return an error — the agent should retry with `extract='raw'` rather than silently degrade.

## Session caching

Fetch results are memoized inside the plugin keyed on `(extract-mode, final-URL)` for the duration of the session. This matters because an agent often searches → fetches a URL → reasons → re-fetches the same URL later in the same turn when the conversation loops back to it. The cache clears on `io.session.end`, so recalling a session in a new engine boot starts fresh.

Search results are **not** cached (queries are high cardinality and cheap enough).

## Gate interaction

Both tools emit through the normal vetoable `before:tool.result` hook, so every existing gate applies without extra wiring:

| Gate | Effect on web tools |
|------|---------------------|
| `nexus.gate.content_safety` | PII/secret redaction or blocking on fetched page content and on search snippets. |
| `nexus.gate.output_length` | Truncates oversized page extractions with an LLM retry. |
| `nexus.gate.tool_filter` | Allow- or block-list `web_search` / `web_fetch` per profile without removing the plugin. |
| `nexus.gate.prompt_injection` | Runs on the LLM's input, including anything the agent pastes back from a fetch result. |

No new gate is needed specifically for the web tools. If you want to constrain fetch targets to a fixed set of sites, use `fetch.allowed_domains` — policy at the fetch layer is cheaper and clearer than a custom gate.

## Choosing an adapter

| You want… | Pick |
|-----------|------|
| Cheapest dedicated search API, free tier, LLM-agnostic | `nexus.search.brave` |
| Zero additional API keys, already paying Anthropic | `nexus.search.anthropic_native` |
| Zero additional API keys, already paying OpenAI | `nexus.search.openai_native` |
| Fanout / redundancy across multiple providers | pin one primary, layer a future fallback adapter |

The native adapters make an extra LLM round-trip to Claude/OpenAI to execute the search. This adds latency (roughly one LLM call on top of the downstream search) but means you do not need a second vendor. They are particularly handy during early prototyping, when shipping another API key is more friction than the latency cost.

## Bus contract

Everything flows through one event pair. Knowing the shape is enough to write your own adapter.

```go
// pkg/events/search.go
type SearchRequest struct {
    Query      string
    Count      int
    SafeSearch string
    Language   string
    Freshness  string

    // Filled by the provider:
    Results  []SearchResult
    Provider string
    Error    string
}

type SearchResult struct {
    Title       string
    URL         string
    Snippet     string
    PublishedAt time.Time
    Source      string
}
```

The request is emitted as a **pointer payload** on `search.request`. Handlers mutate it in place before `Emit` returns, so the tool plugin sees the result synchronously. This is the same pattern as `tool.catalog.query` and `memory.history.query`.

Adapter handlers must:

1. Ignore the event if `req.Provider != ""` (someone else already answered).
2. Set `req.Provider = pluginID` whether the call succeeded or failed.
3. Set `req.Error` **or** `req.Results`, not both.

## Writing a new adapter

```go
package tavily

import (
    "github.com/frankbardon/nexus/pkg/engine"
    "github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.search.tavily"

type Plugin struct{ /* … */ }

func (p *Plugin) Capabilities() []engine.Capability {
    return []engine.Capability{{Name: "search.provider"}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
    // … wire config, HTTP client, API key …
    p.bus = ctx.Bus
    p.bus.Subscribe("search.request", p.handleSearch,
        engine.WithPriority(50), engine.WithSource(pluginID))
    return nil
}

func (p *Plugin) handleSearch(e engine.Event[any]) {
    req, ok := e.Payload.(*events.SearchRequest)
    if !ok || req.Provider != "" {
        return
    }
    results, err := p.callTavily(req)
    req.Provider = pluginID
    if err != nil {
        req.Error = err.Error()
        return
    }
    req.Results = results
}
```

Register the factory in `pkg/engine/allplugins/register.go` and add a `search` section to your config. The web tool picks it up automatically via the capability system.

## Troubleshooting

- **`no search provider answered — check that a plugin advertising 'search.provider' is active`**
  The web tool dispatched a search but no adapter handled it. Activate one of the provided adapters or your own.

- **`readability extraction failed … (try extract='raw')`**
  The page is not an article (docs site, table, forum, dashboard). Re-call with `extract: raw`.

- **`host "example.com" is not allowed by policy`**
  Your `fetch.allowed_domains` or `fetch.blocked_domains` rejected the URL. Adjust the list or remove the restriction.

- **HTTP 401/403 from the adapter**
  Check the API key env var. Each adapter logs the variable it looked at during boot.
