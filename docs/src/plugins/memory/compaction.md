# Context Compaction

Monitors conversation size and automatically summarizes older messages using an LLM when thresholds are exceeded. This prevents the context window from growing unbounded.

## Details

| | |
|---|---|
| **ID** | `nexus.memory.compaction` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `strategy` | string | `message_count` | Trigger strategy: `message_count`, `token_estimate`, or `turn_count` |
| `message_threshold` | int | `50` | Trigger when message count exceeds this (for `message_count` strategy) |
| `token_threshold` | int | `30000` | Trigger when estimated tokens exceed this (for `token_estimate` strategy) |
| `turn_threshold` | int | `10` | Trigger when turn count exceeds this (for `turn_count` strategy) |
| `chars_per_token` | float | `4.0` | Characters per token estimate (for `token_estimate` strategy) |
| `model_role` | string | `quick` | Model role for the compaction LLM call |
| `protect_recent` | int | `4` | Number of most recent messages to keep verbatim (not summarized) |
| `compaction_prompt` | string | *(built-in)* | Custom inline prompt for the summarization |
| `prompt_file` | string | *(none)* | Path to a custom compaction prompt file |
| `persist` | bool | `true` | Persist the live tracked log and archive snapshots to the session workspace |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.input` / `io.output` | 5 | Track messages for threshold checks |
| `tool.invoke` / `tool.result` | 5 | Track tool messages |
| `agent.turn.end` | 30 | Check thresholds after each turn |
| `llm.response` | 5 | Track token usage |

### Emits

| Event | When |
|-------|------|
| `llm.request` | Sends summarization request to LLM |
| `memory.compaction.triggered` | Compaction started |
| `memory.compacted` | Compaction complete — new message set |
| `thinking.step` | Records the compaction reasoning |
| `io.status` | Status updates during compaction |

## Strategies

### `message_count`
Triggers compaction when the total number of tracked messages exceeds `message_threshold`.

### `token_estimate`
Estimates token count using `chars_per_token` and triggers when `token_threshold` is exceeded. This is approximate but avoids counting actual tokens.

### `turn_count`
Triggers when the number of completed turns exceeds `turn_threshold`.

## How Compaction Works

1. Threshold is exceeded after a turn ends
2. Plugin emits `memory.compaction.triggered`
3. Archives the pre-compaction transcript to the session workspace
4. Sends older messages (excluding the `protect_recent` most recent) to the LLM for summarization
5. Writes the returned summary as a sidecar next to the archive snapshot
6. Rotates the live log so it now holds `[summary, ...protected]`
7. Emits `memory.compacted` with the new message set
8. The active `memory.history` provider replaces its buffer with the compacted version

## Persisted Artifacts

When `persist` is enabled (the default) the plugin mirrors its tracked
state into the session workspace so every compaction cycle is fully
auditable:

```
plugins/nexus.memory.compaction/
├── current.jsonl                         # live log — mirrors in-memory state
└── archive/
    ├── 001-20260410-142301.jsonl         # pre-compaction snapshot
    ├── 001-20260410-142301.meta.json     # reason, strategy, counts
    ├── 001-20260410-142301.summary.md    # LLM-produced summary
    ├── 002-20260410-151855.jsonl
    ├── 002-20260410-151855.meta.json
    └── 002-20260410-151855.summary.md
```

- `current.jsonl` is appended to on every tracked message and **rewritten
  in place** at the end of each compaction. After rotation it contains
  the summary system message followed by the protected recent messages,
  which is exactly what the plugin holds in memory.
- Each `archive/NNN-*` cycle is a three-file record: the raw transcript
  that was compacted, the metadata describing why, and the summary that
  replaced it. The numeric counter is recovered from the archive
  directory on startup so numbering survives session resumes.
- On `Ready()` the plugin preloads `current.jsonl` so a resumed session
  continues from the exact state it left, with any prior compaction
  summaries already in place.

## Example Configuration

```yaml
nexus.memory.compaction:
  strategy: token_estimate
  token_threshold: 30000
  model_role: quick
  protect_recent: 6
```

### With Custom Prompt

```yaml
nexus.memory.compaction:
  strategy: message_count
  message_threshold: 40
  model_role: quick
  protect_recent: 4
  prompt: |
    Summarize the conversation so far in 200 words or less, preserving any
    decisions, code changes, and unresolved questions.
```
