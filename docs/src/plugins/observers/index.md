# Observer Plugins

Observers watch system activity without affecting behavior. They're useful for debugging, auditing, and persisting reasoning traces.

## Available Observers

| Plugin | ID | Purpose |
|--------|----|---------|
| [Thinking Persistence](./thinking.md) | `nexus.observe.thinking` | Persists thinking steps and plan progress |
| [OpenTelemetry](./otel.md) | `nexus.observe.otel` | Exports events as OTel traces via OTLP |
| [Online Sampler](./sampler.md) | `nexus.observe.sampler` | Snapshots a fraction of session journals for eval |
| [Event Bus Tap](./tap.md) | `nexus.observe.tap` | Streams every bus event to external consumers over SSE (read-only) |

> The legacy `nexus.observe.logger` plugin was removed in #66 / Phase 3. Its
> `events.jsonl` role is now subsumed by the always-on engine journal at
> `<session>/journal/events.jsonl`. External tooling that previously tailed
> the logger's events.jsonl can tail the journal instead, or connect to `nexus.observe.tap` for a live
> stream of the same envelopes.
