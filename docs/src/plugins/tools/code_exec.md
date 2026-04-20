# Code Exec Tool (Programmatic Tool Calling)

Lets the LLM orchestrate multiple tool calls in a single turn by writing a short Go script. The script runs in an embedded [Yaegi](https://github.com/traefik/yaegi) interpreter and dispatches inner tool calls through the real event bus, so every existing gate still fires.

## Details

| | |
|---|---|
| **ID** | `nexus.tool.code_exec` |
| **Tool Name** | `run_code` |
| **Dependencies** | None (uses current tool registry at invocation time) |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `timeout_seconds` | int | `30` | Wall-clock limit for a script, propagated via `context.Context` |
| `max_output_bytes` | int | `65536` | Stdout/stderr cap per script; excess is silently dropped and `truncated=true` is returned |
| `allowed_packages` | string[] | see below | Go stdlib whitelist. Default covers pure compute: `fmt`, `strings`, `strconv`, `bytes`, `regexp`, `unicode*`, `encoding/*` (json/base64/hex/csv/xml/pem/binary), `crypto/*` (sha/md5/hmac/rand/subtle), `math`, `math/big`, `math/rand`, `math/rand/v2`, `math/bits`, `sort`, `container/*` (heap/list/ring), `hash/*` (crc32/crc64/fnv/adler32), `errors`, `time`, `context`, `io`, `bufio`. Omitted: anything touching filesystem, network, OS processes, reflection, unsafe memory. Also omitted: `slices`/`maps` (Yaegi lacks full generics support). |
| `persist_scripts` | bool | `true` | Write `script.go`, `stdout.txt`, `result.json`, `error.txt` to the session workspace |
| `reject_goroutines` | bool | `true` | Reject scripts containing `go` statements at the AST layer |

## Script Contract

The LLM passes a `script` argument containing a complete Go source file:

```go
package main

import (
    "context"
    "fmt"
    "tools"
)

func Run(ctx context.Context) (any, error) {
    r, err := tools.Shell(tools.ShellArgs{Command: "ls"})
    if err != nil {
        return nil, err
    }
    fmt.Println("found:", r.Output)
    return map[string]string{"listing": r.Output}, nil
}
```

Hard rules enforced before Yaegi ever sees the source:

1. Package must be `main`.
2. Must declare `func Run(ctx context.Context) (any, error)` exactly.
3. No `go` statements (phase 1).
4. Imports restricted to `allowed_packages` plus `tools` plus `skills/<name>` for each currently-active skill.

Violations surface as a structured error in the tool result. The script never executes.

## Typed Tool Bindings

At every `run_code` invocation the plugin snapshots the current tool registry and builds a fresh `tools` package for Yaegi:

- JSON Schema types map to Go: `string→string`, `integer→int64`, `number→float64`, `boolean→bool`, `array→[]T`, `object→struct`.
- Each tool `foo_bar` becomes `tools.FooBar(args tools.FooBarArgs) (<Result>, error)`.
- **Return type depends on whether the tool declared `OutputSchema`:**
  - With schema → `tools.FooBarResult`, a struct generated from the schema. Fields are populated from `ToolResult.OutputStructured` (preferred) or parsed from JSON in `Output` as a fallback.
  - Without schema → the fixed `tools.Result` struct (`{Output, Error, OutputFile string}`). Scripts parse `Output` themselves.
- `tools.Result` is always exported so helper functions can handle both shapes.
- Gate vetoes (`before:tool.invoke`) surface as a Go `error` on the `tools.*` call.
- Outer `run_code` call is excluded from the binding — scripts cannot recursively invoke themselves.

### Declaring an OutputSchema

Tool plugins opt in by adding `OutputSchema` to their `ToolDef` and populating `ToolResult.OutputStructured`:

```go
_ = p.bus.Emit("tool.register", events.ToolDef{
    Name: "shell",
    // ...
    OutputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "stdout":    map[string]any{"type": "string"},
            "stderr":    map[string]any{"type": "string"},
            "exit_code": map[string]any{"type": "integer"},
        },
        "required": []string{"stdout", "stderr", "exit_code"},
    },
})

// ...then in the tool's handler:
result := events.ToolResult{
    ID:     tc.ID,
    Name:   tc.Name,
    Output: humanReadableSummary,
    OutputStructured: map[string]any{
        "stdout":    stdoutStr,
        "stderr":    stderrStr,
        "exit_code": exitCode,
    },
    // ...
}
```

Scripts then see the typed shape:

```go
r, err := tools.Shell(tools.ShellArgs{Command: "ls"})
if err != nil {
    return nil, err
}
fmt.Println(r.Stdout, r.Stderr, r.ExitCode)
```

The existing `Output` string still flows through the bus unchanged — non-script consumers (LLM conversation history, logging, etc.) see the same human-readable text as before. Tools without schemas continue to work as they always did.

## Skill Helpers

Skills may ship `.go` files alongside `SKILL.md`. On `skill.loaded` the plugin reads every non-test `.go` in the skill dir, rewrites the `package` declaration to a sanitised name, and stages the result into a per-invocation GOPATH. Scripts import the package as `skills/<skill_name>`:

```go
import helpers "skills/math-helpers"

func Run(ctx context.Context) (any, error) {
    return helpers.Double(21), nil
}
```

Skills are loaded on `skill.loaded` and removed on `skill.deactivate`. Cross-skill imports are not supported in phase 1.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handles the outer `run_code` call |
| `tool.result` | 50 | Routes inner tool results back to the waiting script |
| `tool.register` | 50 | Builds the type catalogue used to generate `tools.*` bindings |
| `skill.loaded` | 50 | Scans and stages skill helper source files |
| `skill.deactivate` | 50 | Removes skill helpers from the active set |

### Emits

| Event | When |
|-------|------|
| `tool.register` | Registers the `run_code` tool at boot |
| `before:tool.invoke` / `tool.invoke` | For every inner tool call dispatched by a script |
| `before:tool.result` / `tool.result` | For the outer `run_code` call |
| `code.exec.request` | Just before script execution (carries the raw script + imports + active skills) |
| `code.exec.stdout` | For every flushed stdout chunk while the script runs; the final chunk has `Final=true` and carries the truncation flag if applicable |
| `code.exec.result` | When the script has finished, errored out, or timed out — always arrives after the final `code.exec.stdout` chunk for the same `CallID` |

## Stdout Streaming

The interpreter's stdout and stderr are wired to a chunking writer that emits `code.exec.stdout` events while the script is still running. Flush triggers:

1. Any newline in the pending buffer — everything up to the newline flushes.
2. Pending buffer crosses a 512-byte threshold — forces out long lines that would otherwise wait for a newline.
3. Script finish — any residual tail is flushed as the `Final` chunk.

The aggregated `Output` field on the terminal `code.exec.result` still contains the full stdout (capped at `max_output_bytes`), so non-streaming consumers keep working without changes. IO plugins that want live output subscribe to `code.exec.stdout` instead.

## Sandboxing Layers

Defense in depth; each layer enforces a different concern:

1. **Import allowlist** — AST-level rejection of any import not on `allowed_packages` ∪ `{tools}` ∪ active `skills/<name>`.
2. **AST `go`-stmt rejection** — scripts cannot spawn goroutines.
3. **Wall-clock timeout** — script runs under `context.WithTimeout`; `tools.*` shims observe cancellation.
4. **Stdout byte cap** — capped writer drops excess and flags truncation.
5. **No heap/CPU cap** — documented limitation; operators rely on OS-level process limits if a stronger guarantee is required.

## Session Persistence

When `persist_scripts` is enabled each call writes to `plugins/nexus.tool.code_exec/<callID>/`:

```
plugins/nexus.tool.code_exec/<callID>/
  script.go     # exact source the LLM submitted
  stdout.txt    # captured stdout (capped)
  result.json   # JSON-marshaled Run() return value
  error.txt     # only present if the script failed
```

## Example Config

```yaml
plugins:
  active:
    - nexus.tool.shell
    - nexus.tool.file
    - nexus.tool.code_exec

  nexus.tool.code_exec:
    timeout_seconds: 30
    max_output_bytes: 65536
    persist_scripts: true
    reject_goroutines: true
```

## Non-Goals (phase 1)

- Goroutines / `go` keyword
- Provider-native programmatic tool calling (Anthropic `allowed_callers`)
- Cross-skill imports
- CPU / memory resource limits beyond the wall-clock timeout
- Script REPL or debugger
