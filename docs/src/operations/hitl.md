# Human-in-the-Loop and Session Rewind

This page covers the operator surface for two related features: the
unified human-in-the-loop primitive (`nexus.control.hitl`) and the
journal-backed session rewind primitive.

## HITL: one event, many shapes

Every interaction that needs an operator's input — clarification
questions, approvals, plan picks, memory-write sign-offs — flows
through one event family:

- **`hitl.requested`** — emitted by the requesting plugin, payload is a
  `events.HITLRequest` carrying prompt, mode, optional choices,
  default-on-deadline, and an opaque `ActionRef` for context.
- **`hitl.responded`** — emitted by the IO plugin (or, eventually, an
  out-of-band channel), payload is a `events.HITLResponse` carrying the
  picked choice and/or freeform answer.

The `ask_user` tool is the LLM-facing entry point to the same
machinery. Its schema lets the model present:

| `mode` | Required fields | Result shape |
|--------|-----------------|--------------|
| `free_text` (default) | `prompt` | `{"free_text": "..."}` |
| `choices` | `prompt`, `choices` | `{"choice_id": "..."}` |
| `both` | `prompt`, `choices` | `{"choice_id": "...", "free_text": "..."}` (either or both) |

The result is always a JSON object — agents that previously consumed a
bare string from the old `ask_user` tool need a schema update.

## Async response: out-of-band channels via the filesystem registry

By default the hitl plugin routes responses synchronously through the
active IO plugin (TUI, browser, Wails). For long-running sessions where
the operator is in another room, on Slack, or behind a webhook, the
plugin can additionally mirror every `hitl.requested` to a filesystem
registry that any external tool can answer.

### Enable

```yaml
plugins:
  active:
    - nexus.control.hitl
  nexus.control.hitl:
    registry:
      enabled: true
      dir: ~/.nexus/hitl
```

When enabled, the plugin:

1. Persists each `hitl.requested` as
   `<dir>/<request-id>.request.yaml` before blocking on a response.
2. Watches `<dir>` (fsnotify) for files matching
   `<request-id>.response.yaml`.
3. On match, parses the response YAML, emits the typed `hitl.responded`
   on the bus, and deletes both files so the directory does not
   accumulate.

The synchronous IO-driven path stays — IO plugins continue to emit
`hitl.responded` directly. The fsnotify watcher is an additional source.
First response wins; later responses for the same request are no-ops
because the pending channel is already drained.

### CLI

```bash
# List pending requests in the registry directory.
nexus hitl list

# Respond with a multi-choice answer.
nexus hitl respond --choice allow <request-id>

# Respond with freeform text (free_text or both modes).
nexus hitl respond --free-text "trim batch to 50" <request-id>

# Combine: a choice plus an edited payload from a JSON or YAML file.
nexus hitl respond --choice edit --edit ./override.json <request-id>

# Cancel a pending request with an operator reason.
nexus hitl cancel --reason "operator override" <request-id>

# Boolean shorthands (canonical "allow" / "reject" choice IDs).
nexus approve <request-id>
nexus reject  <request-id>
```

Each command resolves the registry directory from the same
`registry.dir` value the running engine uses, writes the response
YAML there, and exits. The engine's fsnotify watcher picks it up on
the next event tick.

If `registry.enabled` is `false` (or the plugin is unconfigured), every
CLI command exits non-zero with a clear "registry disabled in config"
error so a misconfigured operator workflow fails loudly rather than
silently producing orphaned files.

### Wire format

Request file (`<id>.request.yaml`) — written by the plugin:

```yaml
request_id: hitl-turn-3-call-7
session_id: 2026-05-03-001
turn_id: turn-3
requester_plugin: nexus.control.hitl
action_kind: tool.invoke
action_ref:
  tool: shell
  args:
    command: rm -rf /tmp/junk
mode: choices
choices:
  - id: allow
    label: Approve
    kind: allow
  - id: reject
    label: Reject
    kind: reject
default_choice_id: reject
prompt: "Run shell: rm -rf /tmp/junk?"
deadline: 2026-05-03T15:30:00Z
created_at: 2026-05-03T15:25:00Z
```

Response file (`<id>.response.yaml`) — written by the CLI, webhook, etc:

```yaml
request_id: hitl-turn-3-call-7
choice_id: allow
free_text: ""
```

Atomic same-directory rename keeps fsnotify from observing partial
files. Webhook receivers should match the same shape.

### Follow-ups

- An HTTP endpoint that lets a Slack / Discord / ntfy callback POST a
  response directly (instead of writing to disk) is the next iteration
  of this surface. Until then, webhook handlers can write a response
  YAML to the registry directory.
- Windows file-watcher semantics are untested for this path; the
  underlying `fsnotify/fsnotify` library handles macOS and Linux
  natively.

## Session rewind: archive, truncate, replay forward

The journal already records every event. Rewind is the offline
operation that:

1. Moves the live journal (`<sessions.root>/<id>/journal/`) into a
   timestamped archive directory under `journal/archive/`.
2. Writes a truncated copy as the new live journal, ending at a chosen
   `seq` inclusive.
3. Leaves the session in a state where the next boot replays the
   truncated prefix, then resumes live execution.

The rewind is reversible: the archive is preserved verbatim, and
`nexus session restore` swaps it back in (rotating the current live
journal to its own archive first).

### CLI

```bash
# Inspect — print the journal as a timeline (seq, ts, type).
nexus session inspect <session-id> [--limit=100]

# Rewind — archive current journal, keep events seq <= 42.
nexus session rewind --to-seq=42 --yes <session-id>

# List archives for a session.
nexus session archives <session-id>

# Restore a previous archive (the current live journal is itself archived first).
nexus session restore --from-archive=20260503T141500Z --yes <session-id>
```

`--yes` is required for `rewind` and `restore` because both rewrite the
on-disk journal.

### Session lock

A running engine writes `<sessions.root>/<id>/session.lock` on `Boot`
(JSON: `{pid, started_at, transport}`) and removes it on `Stop`.
`rewind` and `restore` refuse to operate when the lock is present and
its PID is alive on the host:

```
session is already running, pid=4242 — use a different session ID or stop the running process
```

A stale lock — PID is gone — is treated as absent. The next `Boot`
overwrites it with a warning, and rewind/restore proceed without
complaint.

For the rare case where the lock is held by a wedged process that
cannot be killed cleanly, both subcommands accept `--force`:

```bash
nexus session rewind --to-seq=42 --yes --force <session-id>
nexus session restore --from-archive=20260503T141500Z --yes --force <session-id>
```

`--force` prints a warning to stderr. Concurrent writes against a
journal being rewound produce undefined state, so use this flag only
when you are certain the holder of the lock is not actually writing.

Liveness probing currently works on Linux and macOS. On other
platforms (Windows in particular), every lock is treated as live —
operators must use `--force` to recover. This is deliberate: silently
overwriting a real run's lock is worse than asking the operator to
opt in.

### When to use it

- **Recovering from a bad turn.** The agent took a wrong path on
  turn 12; rewind to the seq just before turn 12 started, edit the
  preceding event payload (or the system prompt) on disk, and let the
  next boot replay forward.
- **Investigating a regression.** Truncate to the seq before a failure
  and re-run with a different model or config snapshot. Archive
  preserves the original.
- **Pruning sensitive data.** Rewind past a journaled secret that
  should never have been logged (the archive holds it; delete the
  archive directory if even that is too sensitive).

### Limitations (foundation PR)

- An HTTP endpoint for direct webhook callbacks (Slack, Discord, ntfy)
  is not yet wired. The filesystem registry described above is the
  current async response path; webhook handlers can drop response
  YAMLs into the registry directory.
- Multi-operator RBAC: any process with write access to the registry
  directory can answer requests. Hardening (per-operator API keys,
  audit log) is a follow-up.
- Windows fsnotify semantics for the registry are untested; macOS and
  Linux are covered by the underlying `fsnotify/fsnotify` library.
- The prompt-synthesizer capability is reserved in the event payload
  but not yet implemented; literal prompts are the only rendered shape.
- Approval-policy gate (`gates/approval_policy/`) and per-plugin
  `require_approval` configs (memory longterm/vector/compaction) are
  not yet shipped; today, only the `ask_user` tool emits
  `hitl.requested`.

These follow-ups are tracked off this PR.
