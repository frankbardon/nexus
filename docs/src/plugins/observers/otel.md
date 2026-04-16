# OpenTelemetry Observer

Exports all bus events as OpenTelemetry traces via OTLP. Creates one trace per session with individual spans for each event. Rich span attributes are extracted from LLM, tool, agent, and error payloads.

## Details

| | |
|---|---|
| **ID** | `nexus.observe.otel` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `endpoint` | string | `localhost:4317` | OTLP collector endpoint |
| `protocol` | string | `grpc` | Transport protocol: `grpc` or `http` |
| `insecure` | bool | `true` | Skip TLS verification |
| `service_name` | string | `nexus` | OTel service name reported to collector |
| `exclude_events` | list | `[]` | Event types to skip. Supports prefix wildcards (e.g. `llm.stream.*`) |

## Events

### Subscribes To

Uses `SubscribeAll()` — receives every event in the system (minus excluded events).

### Emits

None.

## Trace Structure

Each Nexus session produces one trace:

- **Root span**: `nexus.session` — spans the full session lifetime with session ID attribute.
- **Child spans**: One per bus event, named by event type (e.g. `llm.request`, `tool.invoke`).

All spans include base attributes:
- `nexus.event.id` — unique event ID
- `nexus.event.type` — event type string
- `nexus.event.source` — emitting plugin ID
- `nexus.event.timestamp_unix_ms` — event timestamp

### Rich Attributes by Event Type

**LLM requests** (`llm.request`):
- `nexus.llm.role`, `nexus.llm.model`, `nexus.llm.max_tokens`, `nexus.llm.stream`
- `nexus.llm.message_count`, `nexus.llm.tool_count`, `nexus.llm.temperature`

**LLM responses** (`llm.response`):
- `nexus.llm.model`, `nexus.llm.finish_reason`
- `nexus.llm.usage.prompt_tokens`, `nexus.llm.usage.completion_tokens`, `nexus.llm.usage.total_tokens`
- `nexus.llm.tool_call_count`

**Tool calls** (`tool.invoke`):
- `nexus.tool.id`, `nexus.tool.name`, `nexus.tool.turn_id`

**Tool results** (`tool.result`):
- `nexus.tool.id`, `nexus.tool.name`, `nexus.tool.has_error`, `nexus.tool.error`

**Agent turns** (`agent.turn`):
- `nexus.agent.turn_id`, `nexus.agent.iteration`, `nexus.agent.session_id`

**Subagent events**:
- `nexus.subagent.spawn_id`, `nexus.subagent.task`, `nexus.subagent.iterations`, `nexus.subagent.usage.total_tokens`

**Vetoable events** (`before:*`):
- `nexus.veto.vetoed`, `nexus.veto.reason` (when vetoed)
- Plus attributes from the wrapped original payload

## Example Configuration

```yaml
plugins:
  active:
    - nexus.observe.otel

  # Export to local Jaeger via gRPC
  nexus.observe.otel:
    endpoint: "localhost:4317"
    protocol: grpc
    insecure: true
    service_name: nexus
    exclude_events:
      - llm.stream.chunk
      - core.tick

  # Export to remote collector via HTTP
  nexus.observe.otel:
    endpoint: "otel-collector.example.com:4318"
    protocol: http
    insecure: false
    service_name: my-agent
```

## Backends

Any OTLP-compatible backend works:
- [Jaeger](https://www.jaegertracing.io/) — local development, `docker run -p 4317:4317 -p 16686:16686 jaegertracing/jaeger:latest`
- [Grafana Tempo](https://grafana.com/oss/tempo/) — production tracing
- [Honeycomb](https://www.honeycomb.io/) — managed observability
- [SigNoz](https://signoz.io/) — open source APM
