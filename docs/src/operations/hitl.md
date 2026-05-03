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

- Async out-of-band response channels (Slack, webhook, CLI `nexus hitl
  respond`) are not yet wired. The hitl plugin currently routes
  responses synchronously via the active IO plugin.
- The prompt-synthesizer capability is reserved in the event payload
  but not yet implemented; literal prompts are the only rendered shape.
- Approval-policy gate (`gates/approval_policy/`) and per-plugin
  `require_approval` configs (memory longterm/vector/compaction) are
  not yet shipped; today, only the `ask_user` tool emits
  `hitl.requested`.

These follow-ups are tracked off this PR.
