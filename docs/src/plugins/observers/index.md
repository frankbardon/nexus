# Observer Plugins

Observers watch system activity without affecting behavior. They're useful for debugging, auditing, and persisting reasoning traces.

## Available Observers

| Plugin | ID | Purpose |
|--------|----|---------|
| [Event Logger](./logger.md) | `nexus.observe.logger` | Logs all events as structured JSON |
| [Thinking Persistence](./thinking.md) | `nexus.observe.thinking` | Persists thinking steps and plan progress |
| [OpenTelemetry](./otel.md) | `nexus.observe.otel` | Exports events as OTel traces via OTLP |
