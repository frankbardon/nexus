# Topic-Aware Pruner

Detects topic boundaries in user input and emits
`memory.topic_shift_detected`. The pruner does not itself rewrite
history; it surfaces the shift so other plugins (summary buffer,
compaction) can react.

## Details

| | |
|---|---|
| **ID** | `nexus.memory.topic_pruner` |
| **Capabilities** | _none_ |
| **Dependencies** | _none_; uses `embeddings.provider` opportunistically when one is registered. |

Two signals are combined:

- **Explicit phrase** — substring match against a configurable list
  (`"different question"`, `"new topic"`, `"let's move on"`, etc.).
  Cheap, deterministic. Lead-anchored phrases (`"unrelated:"`,
  `"separately,"`) are tagged `user_explicit`; substring matches are
  tagged `phrase`.
- **Embedding similarity** — cosine similarity between the latest user
  input and the rolling centroid of the current topic's user inputs.
  Below the configured threshold flags a shift. Runs only when an
  `embeddings.provider` is active. Tagged `embedding`.

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `true` | Toggle the pruner. |
| `similarity_threshold` | float | `0.55` | Cosine similarity below which a new user input flags a topic shift. Used only when an `embeddings.provider` is active. |
| `keep_last_topic_full` | bool | `true` | Reserved for downstream consumers. |
| `explicit_phrases` | []string | _see code_ | Lowercase substrings that signal a topic shift. Replacing the list disables the defaults. |

Defaults for `explicit_phrases`:

```
"different question", "different topic", "new topic", "new question",
"let's move on", "moving on", "change of subject", "switching gears",
"unrelated:", "separately,", "on a different note"
```

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `io.input` | 60 | Run the classifier on every user input |
| `agent.turn.end` | 60 | Track the current turn for debouncing |

### Emits

| Event | When |
|-------|------|
| `memory.topic_shift_detected` | Per detected shift (carries `from_turn`, `to_turn`, `similarity`, `signal`) |
| `memory.curated` | Envelope event (`Layer: "topic_pruner"`, `CacheInvalidates: false`) |
| `embeddings.request` | Pointer-payload request when an `embeddings.provider` is active |

Same-turn duplicate signals are debounced — at most one shift event
per turn boundary.

## Replay Determinism

Topic-shift decisions are non-deterministic (heuristic + embeddings).
Each decision is journalled as a `memory.topic_shift_detected` event
(Idea 01) so replay reproduces the same boundaries.

## Example Configuration

```yaml
plugins:
  active:
    - nexus.agent.react
    - nexus.memory.topic_pruner
    - nexus.embeddings.openai   # optional; enables the embedding signal

  nexus.memory.topic_pruner:
    enabled: true
    similarity_threshold: 0.55
    explicit_phrases:
      - "different question"
      - "new topic"
      - "moving on"
```
