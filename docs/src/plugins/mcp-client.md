# MCP client

Bridges one or more [Model Context Protocol](https://modelcontextprotocol.io/) servers into Nexus. Each configured server contributes its tools, resources, and prompts to the running agent through the existing event bus surfaces — agents and IO plugins don't need any MCP awareness.

## Details

| | |
|---|---|
| **ID** | `nexus.mcp.client` |
| **Source** | `plugins/mcp/client/` |
| **Capability** | `mcp.client` |
| **Phase** | 1 (no sampling; see [GitHub #98](https://github.com/frankbardon/nexus/issues/98)) |

The plugin is developer-configured: end users never see "MCP" in the UI. Tools land in the catalog under the namespace `mcp__<server>__<tool>`, prompts surface as slash commands of the form `/mcp.<server>.<prompt>`, and resources show up as catalog tools (one generic browse/read pair per server plus auto-registered statics and templates).

## Quick start

```yaml
plugins:
  active:
    - nexus.mcp.client
    # ...your usual agent + provider + IO plugins

  nexus.mcp.client:
    servers:
      - name: fs
        transport: stdio
        command: npx
        args: ["-y", "@modelcontextprotocol/server-filesystem", "~/projects"]
        env:
          NODE_ENV: production

      - name: gh
        transport: http
        url: http://localhost:3001/mcp
        headers:
          Authorization: "Bearer ${GITHUB_MCP_TOKEN}"
        timeout: 60s
        tools:
          allow: ["search_issues", "get_pr", "review_pr"]
```

Boot order matters only for capabilities — MCP tools are emitted via `tool.register` after `Ready()`, so any plugin that depends on the catalog being populated should subscribe rather than reading it once during init.

## Tools

Every MCP tool returned from `tools/list` is registered into the Nexus catalog as `mcp__<server>__<raw_name>`. The tool's MCP input schema becomes the catalog `Parameters` map verbatim, so the LLM sees the exact schema the server published.

Three generic tools are also registered per server, regardless of what the server returns:

- `mcp__<server>__list_resources()` — returns the JSON catalog of currently available resources.
- `mcp__<server>__read_resource(uri)` — reads a resource by URI.
- For each static resource (up to `auto_register_max`) — a no-arg `mcp__<server>__resource__<slug>` tool that reads that specific URI.
- For each resource template — `mcp__<server>__template__<slug>` whose input schema mirrors the template variables.

Filter what the catalog sees with `tools.allow` / `tools.deny`. Both lists match the raw MCP tool name (no `mcp__` prefix), and deny always wins.

## Resources

Resources surface as catalog tools rather than a separate event family. This keeps the LLM-callable surface uniform: it can `list_resources()` to discover, `read_resource(uri)` to fetch, or call an auto-registered slug for a single resource.

Static-resource auto-registration is capped at `resources.auto_register_max` (default 50). Above the cap the plugin skips per-resource registration and falls back to the generic `list_resources`/`read_resource` pair so the catalog doesn't bloat.

Slugs are deterministic: `slug(title|name|URI) + "_" + sha1(uri)[:8]`. They stay stable across server restarts as long as the server returns the same URI.

When `resources.subscribe_updates` is true (default), the plugin subscribes to every auto-registered static. Each `notifications/resources/updated` from the server emits an `mcp.resource.updated` event onto the Nexus bus. No core consumer reads this in Phase 1 — it's plumbed for future RAG ingest / memory plugins.

## Prompts

Prompts surface as slash commands. The command shape is `/<command_prefix>.<server>.<prompt>`, lowercase, underscores. With `command_prefix: mcp` (default) and a server `gh` exposing a prompt `review_pr`, the slash command is `/mcp.gh.review_pr`.

Arguments use a hybrid positional + `k=v` syntax:

```text
/mcp.gh.review_pr 123 verbose=true comment="needs benchmarks"
```

- Positional values map to the prompt's declared arguments in order.
- `k=v` values can appear anywhere; quoting with `"…"` allows spaces.
- Missing required arguments fail before the command is dispatched.
- Unknown keys fail as well, so typos are surfaced.

When the user fires a slash command, the plugin:

1. Vetoes the original `before:io.input` so memory plugins don't record the literal slash text.
2. Calls `prompts/get` on the right server with the parsed arguments.
3. Translates the returned `Message[]` into a `[]events.Message` keeping each role.
4. Emits a fresh `io.input` whose `PreloadMessages` carries those messages. The downstream memory plugins append them in order; the agent runs as if the user had typed normally.

This routing depends on the `UserInput.PreloadMessages` field (schema v2). All in-tree memory plugins (`capped`, `simple`, `summary_buffer`) honour it. Third-party memory plugins that pin to `UserInputVersion = 1` continue to work — `PreloadMessages` is an optional slice on the v2 struct.

### Aliases

```yaml
nexus.mcp.client:
  aliases:
    review: gh.review_pr
```

`/review topic=plan` rewrites to `/mcp.gh.review_pr topic=plan` before dispatch. Aliases are useful when a single MCP prompt is the canonical entry point for a workflow.

### Discovery

IO plugins (and a future `/help` style command) can list the registered slash commands with a synchronous query:

```go
q := &events.MCPPromptsList{SchemaVersion: events.MCPPromptsListVersion}
_ = bus.Emit("mcp.prompts.list", q)
for _, p := range q.Prompts {
    // p.Command, p.Server, p.Prompt, p.Title, p.Description, p.Arguments
}
```

## Lifecycle

`lifecycle: engine` (default) keeps a single connection alive for the engine's lifetime. Tools/resources/prompts are registered once at boot. Best for almost every developer scenario.

`lifecycle: session` connects on `io.session.start` and disconnects on `io.session.end`. Use when the MCP server holds per-session state that can't be expressed via MCP `roots` (rare today, but legal).

Failures during boot are logged at error but do not block the rest of the engine — a single broken server doesn't take down a Nexus session.

## Transports

`stdio` (default) launches a subprocess and speaks JSON-RPC over its stdin/stdout. The `mark3labs/mcp-go` SDK handles framing and lifecycle.

`http` uses the streamable HTTP transport. The SDK negotiates the session header; configure auth headers via `headers`. The legacy SSE transport is deliberately not exposed.

## Sampling

MCP sampling (server-asks-host-to-call-an-LLM) is deferred to Phase 2. Tracked in [issue #98](https://github.com/frankbardon/nexus/issues/98).

## Testing

The integration tests in `tests/integration/mcp_client_test.go` build the fake MCP server at `tests/integration/mcp_fake/` and exercise the plugin end-to-end over stdio. Run with:

```bash
go test -tags integration ./tests/integration/ -run TestMCPClient -v
```

No LLM provider key is required — the tests drive the bus directly and observe the plugin's catalog, resource, and prompt projections.
