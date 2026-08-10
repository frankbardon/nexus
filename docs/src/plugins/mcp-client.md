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

`stdio` (default) launches a subprocess and speaks JSON-RPC over its stdin/stdout. The official `modelcontextprotocol/go-sdk` handles framing and lifecycle.

`http` uses the streamable HTTP transport. The SDK negotiates the session header; configure auth headers via `headers` (injected on every request through a wrapping `http.RoundTripper`). The legacy SSE transport is deliberately not exposed.

`inprocess` wires an in-memory transport pair (`mcp.NewInMemoryTransports()`) to an `*mcp.Server` the embedding host built and handed to the plugin. No subprocess is launched and no socket is dialled. This is the transport for hosts that embed Nexus and already have MCP tools implemented in the same binary. See [In-process servers](#in-process-servers) below.

## In-process servers

`transport: inprocess` connects to a live `*mcp.Server` owned by the host process instead of launching one. The wiring has two halves that must agree: a Go call that registers the server under an opaque key, and a YAML `server:` value naming that same key.

### The `server` key

| | |
|---|---|
| **Required for** | `transport: inprocess` (the config schema rejects the boot without it) |
| **Type** | non-empty string |
| **Meaning** | An opaque, host-chosen key. It is never parsed, matched against a pattern, or derived from anything — it only has to be byte-identical to the key passed to `client.RegisterInProcessServer`. |

If the key is absent, boot fails during schema validation naming the key. If the key is *present but unregistered*, boot succeeds — a broken MCP server never blocks the engine — and the connect fails with `no host-injected server registered under key "…"` logged at error. The symptom is a missing `mcp__<server>__*` namespace, not a crash.

### Wiring order: register before `engine.Boot`

> **`RegisterInProcessServer(key, srv)` must be called before `engine.Boot(ctx)`.**

The plugin resolves the key while connecting, and for the default `lifecycle: engine` that connect happens *during* boot. A registration made after `Boot` returns is too late: the connect has already failed and the server's tools are absent for the rest of the engine's life. (With `lifecycle: session` the lookup happens at each `io.session.start` instead, but registering before `Boot` is correct for both and is the rule to follow.)

### Worked example

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	mcpclient "github.com/frankbardon/nexus/plugins/mcp/client"
)

// echoInput is the typed argument for the host's tool; the SDK derives the
// tool's JSON schema from this struct.
type echoInput struct {
	Text string `json:"text"`
}

func main() {
	ctx := context.Background()

	// 1. Build the MCP server in this process with the official SDK.
	srv := mcp.NewServer(&mcp.Implementation{Name: "host-tools", Version: "v0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "echo",
		Description: "Echo the input text back.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}},
		}, nil, nil
	})

	// 2. Register it under a key BEFORE Boot. The YAML `server:` value must
	//    be exactly this string.
	mcpclient.RegisterInProcessServer("host-tools", srv)

	// 3. Boot as usual. The plugin resolves "host-tools" during Boot and
	//    connects over the in-memory transport.
	eng, err := engine.New("nexus.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "engine: %v\n", err)
		os.Exit(1)
	}
	allplugins.RegisterAll(eng.Registry)

	if err := eng.Boot(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "boot: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = eng.Stop(context.Background()) }()

	// An embedding host owns the lifecycle and blocks however it likes —
	// here, until a plugin ends the session. Don't call eng.Run: that is the
	// stock CLI wrapper, it calls Boot itself and it owns signal handling.
	<-eng.SessionEnded()
}
```

The matching `nexus.yaml` block:

```yaml
plugins:
  active:
    - nexus.mcp.client

  nexus.mcp.client:
    servers:
      - name: host
        transport: inprocess
        server: host-tools   # must equal the RegisterInProcessServer key
        lifecycle: engine
```

The `echo` tool then reaches the agent as `mcp__host__echo`, exactly as if it had come from a subprocess server.

### The registry is process-wide

`RegisterInProcessServer` writes into a **package-level, process-wide** map. It is not scoped to an engine, an agent, or a session. Two engines booted in the same process share one key namespace, and a later registration under an existing key silently replaces the earlier one.

The concrete failure in a multi-tenant host: tenant A registers its server under `host-tools`, then tenant B registers *its* server under `host-tools` too. The map now holds B's server, and tenant A's YAML — which still says `server: host-tools` — connects to **tenant B's** MCP server. Tenant A's agent then calls tools bound to tenant B's data. Nothing errors; the tools are present and answer normally.

Scope the key per tenant or per agent to avoid this, and derive the YAML value from the same identifier rather than hard-coding it — for a per-tenant engine built with `engine.NewFromBytes`, render the `server:` value into the config bytes from the same variable used for the registration key:

```go
key := "tenant-" + tenantID + "/host-tools"
mcpclient.RegisterInProcessServer(key, srv)
// ...render `server: <key>` into the per-tenant config bytes, then
// engine.NewFromBytes(cfg) → RegisterAll → Boot.
```

### Cleaning up: `UnregisterInProcessServer`

```go
mcpclient.UnregisterInProcessServer(key)
```

Removes the registration. Because the map is process-wide, **tests must unregister in cleanup** or one test's server stays visible to every later test in the package:

```go
mcpclient.RegisterInProcessServer(key, srv)
t.Cleanup(func() { mcpclient.UnregisterInProcessServer(key) })
```

Hosts whose servers live for the whole process lifetime do not need to call it. Long-lived hosts that tear down a tenant should, so the key does not linger for the next tenant that reuses it.

### Why the registry lives in the plugin package

The natural home for a host-injection seam is `pkg/engine`, next to `eng.Registry.Register(id, factory)` — the existing precedent for handing host-constructed objects to the engine before `Boot`. This one cannot live there: the injected object is an `*mcp.Server`, and **the engine core deliberately does not import the MCP SDK**. Only this plugin does. Putting the registry on `pkg/engine` would drag `github.com/modelcontextprotocol/go-sdk` across the engine boundary and into every binary that links the engine, MCP or not. So the registry stays in `plugins/mcp/client` (`injected.go`) and hosts import the plugin package directly.

## Sampling

MCP sampling (server-asks-host-to-call-an-LLM) is deferred to Phase 2. Tracked in [issue #98](https://github.com/frankbardon/nexus/issues/98).

## Testing

The integration tests in `tests/integration/mcp_client_test.go` build the fake MCP server at `tests/integration/mcp_fake/` and exercise the plugin end-to-end over stdio. Run with:

```bash
go test -tags integration ./tests/integration/ -run TestMCPClient -v
```

No LLM provider key is required — the tests drive the bus directly and observe the plugin's catalog, resource, and prompt projections.

The `inprocess` transport is covered by unit tests in `plugins/mcp/client/inprocess_test.go` (`go test ./plugins/mcp/client/`), which build a one-tool, one-resource `*mcp.Server`, register it, and drive `tool.invoke` through the in-memory transport. They are the shortest runnable reference for the wiring described in [In-process servers](#in-process-servers).
