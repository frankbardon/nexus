# Observer Plugins

Observers watch system activity without affecting behavior. They're useful for debugging, auditing, and persisting reasoning traces.

## Available Observers

| Plugin | ID | Purpose |
|--------|----|---------|
| [Thinking Persistence](./thinking.md) | `nexus.observe.thinking` | Persists thinking steps and plan progress |
| [OpenTelemetry](./otel.md) | `nexus.observe.otel` | Exports events as OTel traces via OTLP |

> The legacy `nexus.observe.logger` plugin was removed in #66 / Phase 3. Its
> `events.jsonl` role is now subsumed by the always-on engine journal at
> `<session>/journal/events.jsonl`. External tooling that previously tailed
> the logger's events.jsonl can tail the journal instead.
