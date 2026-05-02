# Inspect-Mode Protocol

`nexus eval --inspect-mode` is the headless JSON-on-stdin/stdout protocol
that external eval harnesses (AISI Inspect AI, Braintrust, custom CI
tooling) use to drive Nexus. The binary reads one JSON object from stdin,
runs a single agent turn (or multi-turn run, capped by `max_turns`) under
the supplied config, and writes one JSON object to stdout.

This page is the **durable wire-format reference**. It is pinned by the
schema-stability snapshot test in
[`pkg/eval/protocol/schema_test.go`](https://github.com/frankbardon/nexus/blob/main/pkg/eval/protocol/schema_test.go).
PRs that change the wire format must update both that snapshot and the
documentation in this file in the same change.

> **Nexus does not ship a Python shim.** A 15-line shim is sketched at
> the bottom of this page so the interop story is documented; the shim
> itself is out-of-tree by design (see plan.md, "No Python in this repo").

## Invoking

```bash
echo '{"schema":1, ...}' | nexus eval --inspect-mode
```

Flags:

- `--inspect-mode` — required. Mutually exclusive with subcommands and
  positional args; combining them returns an `INVALID_REQUEST` error.
- `--timeout=<duration>` — optional. Per-request deadline. When unset,
  the env var `NEXUS_EVAL_INSPECT_TIMEOUT` is honored. Default `60s`.

The deadline applies to the entire request (engine boot, agent run,
shutdown). Crossing it surfaces as a `TIMEOUT` error code.

## Request

Exactly one JSON object on stdin, terminated by EOF:

```json
{
  "schema": 1,
  "config_path": "configs/coding.yaml",
  "config_inline": "<yaml string>",
  "user_input": "explain the build error in main.go",
  "max_turns": 8,
  "metadata": { "case_id": "swe-bench-1234" }
}
```

| Field | Type | Required | Semantics |
|-------|------|----------|-----------|
| `schema` | integer | yes | Wire-format version. Must be `1` today. |
| `config_path` | string | one of | Path to a YAML config. `~` is expanded. Mutually exclusive with `config_inline`. |
| `config_inline` | string | one of | YAML config body inline. Mutually exclusive with `config_path`. |
| `user_input` | string | yes | The single prompt fed into the agent. |
| `max_turns` | integer | no | Hard cap on `agent.turn.end` events observed. `0` (or omitted) = no protocol-level cap; the agent's own iteration gate still bounds the run. |
| `metadata` | object | no | Opaque pass-through. Round-tripped to the response. |

Strict parsing applies: unknown fields are rejected with `INVALID_REQUEST`.
This catches typos at the harness boundary instead of silently defaulting
fields.

The runner overlays your config to make the run hermetic:

- `core.sessions.root` is overridden to a temp directory under
  `os.TempDir()`. The directory is removed on exit unless
  `NEXUS_EVAL_INSPECT_KEEP_SESSIONS=1` is set (useful for post-hoc
  journal forensics).
- `nexus.io.test` is added to `plugins.active` if absent; visual
  transports (`nexus.io.tui`, `nexus.io.browser`, `nexus.io.wails`,
  `nexus.io.oneshot`) are stripped because the run must be headless.
- `plugins.nexus.io.test.inputs` is set to `[user_input]`,
  `approval_mode` to `approve`, `timeout` to `600s`. Any other keys you
  set on `nexus.io.test` (notably `mock_responses`) are preserved.

## Response

One JSON object on stdout, exit `0` on success:

```json
{
  "schema": 1,
  "session_id": "01HK...",
  "final_assistant_message": "the build error is in line 42",
  "tool_calls": [
    {
      "tool": "shell",
      "args": { "cmd": "go build ./..." },
      "result_summary": "main.go:42: undefined: Foo",
      "duration_ms": 412
    }
  ],
  "tokens": { "input": 6213, "output": 1102 },
  "latency_ms": 18733,
  "metadata": { "case_id": "swe-bench-1234" },
  "error": null
}
```

| Field | Type | Semantics |
|-------|------|-----------|
| `schema` | integer | Mirrors the request's wire-format version. |
| `session_id` | string | Engine-assigned session UUID. Empty if the engine never reached session bootstrap. |
| `final_assistant_message` | string | Text of the final assistant `llm.response` (terminal `FinishReason`). Empty when no terminal turn fired. |
| `tool_calls` | array | Ordered `tool.invoke` → `tool.result` pairs in journal order. Always non-null (empty array when no tool calls fired). |
| `tool_calls[].tool` | string | Tool name. |
| `tool_calls[].args` | object | Parsed argument map from the agent's invocation. |
| `tool_calls[].result_summary` | string | Truncated stringification of the tool's output (≤ 2 KB, UTF-8 safe; ellipsized when truncated). Includes any error string. |
| `tool_calls[].duration_ms` | integer | `tool.result.Ts − tool.invoke.Ts` in milliseconds. |
| `tokens` | object | Per-session token totals across every `llm.response`. |
| `latency_ms` | integer | First-to-last journaled envelope wall time, in milliseconds. |
| `metadata` | object | Round-tripped from the request unchanged. |
| `error` | object \| null | Populated on failure; null on success. |

### Errors

When the request fails, the process exits non-zero **and** the response's
`error` field is populated. This redundancy is deliberate: harnesses can
key off either signal.

```json
{
  "schema": 1,
  "tool_calls": [],
  "metadata": { "case_id": "..." },
  "error": {
    "code": "CONFIG_LOAD",
    "message": "config load: read /missing.yaml: no such file or directory"
  }
}
```

| Code | When | Typical recovery |
|------|------|------------------|
| `INVALID_REQUEST` | Wire-format violation: missing/extra field, both config sources set, schema mismatch, unknown field, malformed JSON, or the mode was paired with a subcommand. | Fix the request envelope. |
| `CONFIG_LOAD` | The named `config_path` could not be read, or the YAML failed to parse. | Verify the path / contents. |
| `ENGINE_BOOT` | Plugin initialization or capability resolution failed. | Inspect engine logs; usually a missing required plugin or bad per-plugin config. |
| `RUN_FAILED` | The engine booted and ran, but projecting the journal into the response shape failed (e.g. malformed events, unreachable journal). Mid-run agent errors generally surface via `TIMEOUT` when context expires or as a partial response. | Inspect the response's `tool_calls`, the journal under `NEXUS_EVAL_INSPECT_KEEP_SESSIONS=1`, or rerun under `nexus eval run` with the same config. |
| `TIMEOUT` | The request exceeded the deadline (flag, env, or default 60s) before reaching session-end. | Raise `--timeout`, lower `max_turns`, or simplify the case. |
| `INTERNAL` | Unanticipated error. | Treat as a Nexus bug; file an issue. |

## Schema versioning

The `schema` field is the durable contract. Bumping it is a deliberate
event:

1. Increment `SchemaVersion` in `pkg/eval/protocol/protocol.go`.
2. Update the snapshots in `pkg/eval/protocol/schema_test.go`.
3. Add a migration note to this page describing what changed and how
   external harnesses should adapt.

The schema-stability snapshot test enforces this discipline: a drift in
field names, types, or order fails CI until the snapshot is updated
deliberately.

## External harness integration

Nexus does not ship a Python shim. The protocol is the contract; any
language with `subprocess` and a JSON parser can drive it. Here is a
minimal Python sketch demonstrating the wire format; copy it out-of-tree
into your eval harness as needed.

```python
# example: drive_nexus.py — NOT shipped with Nexus, copy out-of-tree.
import json
import subprocess

def run_nexus(config_path, user_input, *, max_turns=0, metadata=None,
              timeout="60s"):
    req = {
        "schema": 1,
        "config_path": config_path,
        "user_input": user_input,
    }
    if max_turns:
        req["max_turns"] = max_turns
    if metadata:
        req["metadata"] = metadata
    proc = subprocess.run(
        ["nexus", "eval", "--inspect-mode", f"--timeout={timeout}"],
        input=json.dumps(req).encode(),
        capture_output=True,
        check=False,
    )
    resp = json.loads(proc.stdout)
    if resp.get("error"):
        raise RuntimeError(f"{resp['error']['code']}: {resp['error']['message']}")
    return resp
```

For Inspect AI specifically, write a `Solver` whose `__call__` drives
this protocol and emits the final assistant message as the model output;
score on the round-tripped `metadata`.

## See also

- [Eval Harness Overview](./overview.md) — the bigger picture.
- [Case Format](./case-format.md) — the on-disk eval bundle that runs
  through `nexus eval run` rather than the inspect protocol.
- `pkg/eval/protocol/` — Go source of truth for the wire format.
- `pkg/eval/protocol/schema_test.go` — snapshot test pinning the format.
