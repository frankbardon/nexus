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
| `allowed_packages` | string[] | `[fmt, strings, strconv, encoding/json, math, sort, errors, time, context]` | Go stdlib whitelist |
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
- Each tool `foo_bar` becomes `tools.FooBar(args tools.FooBarArgs) (tools.Result, error)`.
- `tools.Result` is a fixed `{ Output, Error, OutputFile string }` so the script sees a predictable shape across tools.
- Gate vetoes (`before:tool.invoke`) surface as a Go `error` on the `tools.*` call.
- Outer `run_code` call is excluded from the binding — scripts cannot recursively invoke themselves.

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
| `code.exec.result` | When the script has finished, errored out, or timed out |

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
- Live stdout streaming
- CPU / memory resource limits beyond the wall-clock timeout
- Script REPL or debugger
