# Sandboxing

Nexus tools that shell out (`tools/shell`) or execute agent-emitted code
(`tools/code_exec`) route through a single execution-isolation abstraction at
`pkg/engine/sandbox`. Backends slot in via the `Sandbox` interface; the v1
release ships `host` and `wasm`, with `landlock`, `gvisor`, and
`firecracker` deferred.

## Threat model

Agent-emitted code is the load-bearing case. Trail of Bits' Oct 2025
[prompt-injection-to-RCE chain](https://blog.trailofbits.com/2025/10/22/prompt-injection-to-rce-in-ai-agents/)
documented multiple production incidents where a compromised LLM context
yielded host shell access through under-isolated tool execution. For
non-developer end users (the desktop shell ships agents to laptops),
"sandbox in name only" is a regression to those incidents waiting to
happen.

The `wasm` backend closes that risk for `tools/code_exec` by interpreting
agent code inside a wazero-managed Wasm module. The module sees no kernel
syscalls — every escape is a host-side bridge function with explicit
capability gates.

## Backends

### `host` (default for `tools/shell`)

Runs commands directly via `os/exec` against the host kernel. The
configured `allowed_commands` allowlist and `working_dir` apply, but a
permissive allowlist is a host compromise away from arbitrary code
execution. Use this only when commands are trusted (developer machines, CI
runners, allowlists tight enough that escape is implausible).

```yaml
nexus.tool.shell:
  sandbox:
    backend: host
    allowed_commands: [git, go, npm]
    working_dir: ~/.nexus/sessions/${session_id}/files
```

### `wasm` (recommended for `tools/code_exec`)

Embeds a Yaegi-compiled-to-Wasm interpreter into the engine binary at
build time. The wasm module sees no fs / net / syscalls unless the
configured policy explicitly grants them, in which case the calls go
through a host bridge function gated per-operation. Bridge surfaces:

| SDK package | Capability tag | Gate config |
|---|---|---|
| `nexus_sdk/http` | `cap_net_http` | `sandbox.net.allow_hosts` (exact-match hostnames) |
| `nexus_sdk/fs` | `cap_fs_read`, `cap_fs_write` | `sandbox.fs_mounts` (host→guest bindings, ro/rw) |
| `nexus_sdk/exec` | `cap_exec` | `sandbox.exec_allowed` (command allowlist) |
| `nexus_sdk/env` | none | `sandbox.env` (sandbox-scoped key/value map) |

```yaml
nexus.tool.code_exec:
  compiler: yaegi-wasm
  sandbox:
    backend: wasm
    cache_dir: ~/.nexus/sandbox/wasm/cache
    timeout: 30s
    net:
      allow_hosts: ["api.openai.com", "api.anthropic.com"]
    fs_mounts:
      - host: ~/.nexus/sessions/${session_id}/files
        guest: /workspace
        mode: rw
    exec_allowed: [git]
    env:
      WORKSPACE: /workspace
```

Empty `net.allow_hosts` denies all HTTP. Empty `fs_mounts` denies all FS.
Empty `exec_allowed` denies all subprocess invocation.

## Snippet authoring under `wasm`

```go
package main

import (
	"context"
	"fmt"
	"errors"

	nhttp "nexus_sdk/http"
	nfs "nexus_sdk/fs"
)

func Run(ctx context.Context) (any, error) {
	resp, err := nhttp.Get("https://api.example.com/data")
	if err != nil {
		if errors.Is(err, nhttp.ErrCapDenied) {
			return nil, fmt.Errorf("net.allow_hosts denies api.example.com")
		}
		return nil, err
	}
	if err := nfs.WriteFile("/workspace/out.json", resp.Body); err != nil {
		return nil, err
	}
	return resp.Status, nil
}
```

The `nexus_sdk/*` packages mirror the shape of `net/http`, `os`, `os/exec`,
and `os.Getenv` so familiar Go reflexes work. The bridge layer flattens
the ABI: Bodies are `[]byte`, not `io.Reader`. Streaming reads / writes
are not supported in v1.

## What you give up under `wasm`

- Raw TCP / `net.Dial` — not provided. Add a targeted bridge function
  (`nexus_sdk/websocket`, etc.) when a real caller needs it.
- `cgo` — never. Not a Wasm thing.
- Full `database/sql` driver libraries — most native drivers won't run in
  Wasm without their own bridges. Use HTTP-shaped database backends
  (REST, BigQuery) or run database access via `tools/shell` with
  appropriate gating.
- `tools.*` typed bindings, `parallel.*` constructs, skill helpers — v1
  surface forfeits these on the wasm path. They remain available under
  `compiler: yaegi-host` for trusted skill-author workflows.

## Performance

| Operation | Cost |
|---|---|
| WasmBackend cold start (one-time per session) | ~5–9 s wazero AOT compile of the 39 MiB embedded runner |
| With `cache_dir` set, subsequent process startup | ~50–200 ms |
| Per-snippet (warm backend) | ~30–50 ms wall, ~5–30 ms interpretation overhead + bridge round-trips |

Set `cache_dir` to a stable path (e.g., `~/.nexus/sandbox/wasm/cache`) to
amortise the wazero AOT cost across processes.

## Future tiers (not in v1)

- `landlock` — Linux-only hardening below `tools/shell`.
- `gvisor` — `runsc` subprocess for stronger `tools/shell` isolation
  under Linux.
- `firecracker` — Per-snippet microVM for hosted multi-tenant deployments.

The full-Go (`GOOS=wasip1`) compile path with auto-bootstrapped Go SDK is
tracked in [#71](https://github.com/frankbardon/nexus/issues/71) and ships
only on real demand for full Go stdlib semantics (generics, full
`reflect`, `text/template`).
