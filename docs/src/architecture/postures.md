# Postures

A **posture** is a registered, named, versioned configuration that describes
how a sub-agent should run: which system prompt, which subset of tools, which
model, and what default resource budget. Postures are the contract that the
[delegate runtime](./delegate.md) resolves at invocation time, and the value
operators tune in production to change agent behavior without code changes.

## Schema

```go
type AgentPosture struct {
    Name              string         // Registry key parent agents reference
    Description       string         // Human-facing copy (introspection prompts)
    SystemPrompt      string         // The posture's prompt
    AllowedTools      []string       // Closed list of permitted tool names
    OutputSchema      string         // Named schema validated against final output (optional)
    Model             ModelConfig    // Model tier / explicit Provider+Model override
    DefaultBudget     ResourceBudget // Timeout, MaxTokens, MaxToolCalls
    MaxRecursionDepth int            // Per-posture depth cap (0 falls back to runtime MaxDepth)
    Version           string         // Content hash, assigned by the loader / registry
}

type ResourceBudget struct {
    Timeout      time.Duration
    MaxTokens    int
    MaxToolCalls int
}
```

`pkg/posture` defines the type and an in-memory `Registry`. The
`nexus.agent.postures` plugin loads YAML from disk and exposes the
`posture.registry` capability.

## YAML

Postures live in a directory of `*.yaml` files. The filename (minus
extension) supplies a fallback `name` if the YAML omits one. Example:

```yaml
name: analyst
description: deep reader; quotes sources verbatim
system_prompt: |
  You are a careful analyst. Cite sources by URL. Be concise.
allowed_tools:
  - web_search
  - web_fetch
  - read_pdf
output_schema: analyst_report
model:
  model_role: reasoning
  max_tokens: 4000
default_budget:
  timeout: 60s
  max_tokens: 50000
  max_tool_calls: 20
max_recursion_depth: 2
```

## Versioning

The registry hashes each posture's content (name, system prompt, allowed
tools, output schema, model selectors) into a 16-character `Version` string.
Two postures with the same `Name` but different content are not "different
versions" — the new content replaces the old, but the `Version` change flows
into the [delegate result cache](./delegate.md#caching) key, invalidating
any stale entries automatically.

## Hot reload

`nexus.agent.postures` watches every configured `scan_dirs` entry with
fsnotify. Edits and adds re-load the affected file after a small debounce
(`debounce_ms`, default 250ms); deletes drop the posture from the registry.
Active sub-sessions keep their old configuration; new invocations resolve
the new one. This is how operators tune prompts in production without
restarts.

The watcher swallows individual parse errors with a `WARN` log — a single
malformed file does not block the rest from registering.

## Capability resolution

The plugin advertises:

```go
Capabilities: posture.registry
```

The [delegate plugin](../plugins/agents/delegate.md) requires this
capability, so the lifecycle manager pins the active provider at boot and
the delegate runtime resolves the registry through `LookupPlugin` without
plugin-to-plugin imports or bus handshake races.

## Watching change events

Operators that want to react to posture edits (warm caches, alert on
removals) can subscribe to the `posture.registered` / `posture.removed`
bus events; see the [events reference](../events/reference.md#posture-events).
The in-process `posture.Registry.Watch(ctx)` channel provides the same
notifications inside the process for plugins that need them.

## Configuration

See [`nexus.agent.postures`](../configuration/reference.md#nexusagentpostures)
in the configuration reference.
