# Online Eval Sampler

Opt-in observer plugin that snapshots a configurable fraction of live
session journals (plus every failed session, when failure-capture is on)
into a local directory so the eval pipeline can score them later.

The sampler is **off by default**. It does not appear in any of the
shipped configs; activating it is a deliberate two-step opt-in: list the
plugin in `plugins.active`, then set `enabled: true` in its config block.
Omitting either step keeps the plugin inert — `Subscriptions()` returns
empty, no bus traffic, no disk writes.

## Details

| | |
|---|---|
| **ID** | `nexus.observe.sampler` |
| **Source** | `plugins/observe/sampler/plugin.go` |
| **Dependencies** | None — the journal is core, not a plugin |
| **Capabilities** | None |
| **Default state** | Disabled |

## Why it exists

Promotion (`nexus eval promote`, see [Promoting a Session](../../eval/promotion.md))
turns a real session into a deterministic eval case in one command. But
the operator has to *know* a session is interesting before promoting it.
The sampler closes that loop:

- A small fraction of normal sessions land on disk so a sample of the
  workload is always available for offline scoring.
- Every failed session is captured automatically (no "I forgot to keep
  that one"), preserving the journal exactly as it was when it failed.

The captured directory is in the same shape `nexus eval promote`
accepts as input — so a captured session can be promoted to a fully
deterministic case without any intermediate transformation.

## Configuration

Per the [configuration reference](../../configuration/reference.md#nexusobservesampler):

```yaml
plugins:
  active:
    - nexus.observe.sampler

  nexus.observe.sampler:
    enabled: false                    # master switch (default false)
    rate: 0.0                         # fraction of normal sessions captured (0..1)
    failure_capture: true             # always capture status != completed/active
    out_dir: ~/.nexus/eval/samples    # path expanded via engine.ExpandPath
```

`Init` (`plugins/observe/sampler/plugin.go:96`) validates `rate ∈ [0, 1]`
when `enabled: true` and creates `out_dir` ahead of any captures.

## What gets written

For each captured session, the sampler writes:

```
<out_dir>/<session-id>/
  journal/
    header.json              # exact copy of the source journal header
    events.jsonl             # active segment (byte-for-byte under IdentityRedactor)
    events-001.jsonl.zst     # rotated segments, byte-for-byte
    cache/...                # tool result cache, byte-for-byte
  metadata.json              # provenance: captured_at, reason, sampling_rate_at_capture, session_status, engine_version
```

`<session-id>` is the live session's ID — **not** a fresh ID per sample.
Re-running the sampler against the same session is idempotent: the
plugin's in-memory `captured` set short-circuits a duplicate capture in
the same engine lifetime.

The `journal/` subtree is produced by
[`pkg/iocopy.CopyDir`](../../../../pkg/iocopy/copy.go) — the same helper
the promote pipeline uses, so the two paths cannot drift.

## Capture decision

Every `io.session.end` runs through `Plugin.decide`
(`plugins/observe/sampler/plugin.go:194`):

1. If `failure_capture: true` **and** the session's
   `metadata/session.json` `status` is anything other than `active`
   or `completed`, return `("failure_capture", true)` and snapshot.
2. Else, if `rate <= 0`, skip.
3. Else, if `rate >= 1`, return `("sampled", true)` and snapshot.
4. Else, roll a `[0, 1)` float against `rate`. Capture on hit.

Tests inject a deterministic RNG via the package-private
`Plugin.SetRandSource` so the rate path is reproducible:

```go
p := New().(*Plugin)
p.SetRandSource(rand.New(rand.NewSource(42)))
```

## The `eval.candidate` event

Every capture emits one `eval.candidate` envelope:

```go
type EvalCandidate struct {
    SessionID string   `json:"session_id"`
    CaseDir   string   `json:"case_dir"`
    Reason    string   `json:"reason"`        // "sampled" or "failure_capture"
    Warnings  []string `json:"warnings,omitempty"`
}
```

Downstream tooling (a future `nexus eval list-candidates`, an external
ingestion backend, a UI badge) can subscribe to `eval.candidate` and
enumerate captures without scanning `out_dir` itself. Definition:
`plugins/observe/sampler/events.go:1`.

## Redaction

The sampler accepts a pluggable `Redactor`
(`plugins/observe/sampler/redact.go:12`):

```go
type Redactor interface {
    Redact(eventType string, payload []byte) ([]byte, error)
}
```

v1 ships only the `IdentityRedactor` (no-op). When a non-identity
redactor is configured, the sampler walks the active `events.jsonl`
segment line-by-line after the byte copy and rewrites each envelope's
payload through the redactor. A nil return wipes the payload while
preserving envelope metadata (seq, type, ts).

> **Limitation.** Compressed `*.jsonl.zst` rotated segments are
> byte-copied as-is in v1 — round-tripping zstd would expand the
> dependency surface. If a non-identity redactor matters for rotated
> segments, surface the case as a follow-up issue and design a streaming
> rewrite path.

## Integration with `nexus eval promote`

The on-disk shape under `out_dir/<session-id>/journal/` is exactly what
[`pkg/eval/promote`](../../eval/promotion.md) consumes. A future
follow-up will let `nexus eval promote --session ~/.nexus/eval/samples/<id>`
pick up a sampled directory directly. Today the same end can be reached
with one symlink or copy into `~/.nexus/sessions/` and the regular
promote flow.

The `metadata.json` sibling carries the bookkeeping that promote does
not need (captured_at, sampling_rate_at_capture, etc.) — it is
provenance for analytics, not state for replay.

## Privacy posture

- Off by default. No data is captured without an explicit config opt-in.
- `out_dir` is local to the host. The plugin makes zero network calls.
- A `Redactor` hook is in place from day one. The default is identity;
  custom redactors are an API-only contract — no surface area in the
  YAML schema yet.
- Sampled bytes are subject to the same retention rules the operator
  applies to `out_dir` itself; the plugin never deletes its own output.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.session.end` | `0` (low) | Run decide → snapshot → emit `eval.candidate`. Lowest priority so end-of-session writers (journal, memory persisters) finalize first. |

### Emits

| Event | Payload | When |
|-------|---------|------|
| `eval.candidate` | `EvalCandidate` | After a successful snapshot. |
