# Structured Output

This guide covers how to get structured (schema-validated) output from LLM providers in Nexus. The system uses a three-layer design: **schema declaration → request tagging → provider execution**, with the existing `json_schema` gate as a safety net.

## How It Works

1. A schema is registered with the **Schema Registry** (via `schema.register` event or direct API)
2. An LLM request is tagged with `_expects_schema` metadata pointing to the schema name
3. The Schema Registry attaches a `ResponseFormat` to the request
4. The **provider** maps `ResponseFormat` to its native structured output mechanism (or simulates it)
5. The `json_schema` gate optionally validates the response as a safety net

## Scenarios

### 1. Skill with Inline Output Schema

The simplest path — declare the schema directly in your SKILL.md frontmatter.

**SKILL.md:**

```yaml
---
name: extract-entities
description: Extract named entities from text.
output_schema:
  type: object
  required: [entities]
  properties:
    entities:
      type: array
      items:
        type: object
        required: [name, type]
        properties:
          name: { type: string }
          type: { type: string, enum: [person, org, location, date] }
---

# Entity Extraction

Extract all named entities from the user's text. Return only the JSON output.
```

**Config:**

```yaml
plugins:
  active:
    - nexus.skills
    - nexus.llm.openai  # or nexus.llm.anthropic
    - nexus.agent.react
```

No additional config needed — the skills plugin handles registration automatically.

**What happens:**

1. User triggers skill activation → skills plugin loads `output_schema` from frontmatter
2. Skills plugin emits `schema.register` with name `skill.extract-entities.output`
3. On each LLM request while skill is active, skills plugin tags `_expects_schema = "skill.extract-entities.output"` via `before:llm.request`
4. Schema Registry sees the tag, attaches `ResponseFormat{Type: "json_schema", Schema: ...}` to the request
5. Provider sends structured output request to the API
6. On deactivation, skills plugin emits `schema.deregister`

### 2. Skill with File-Referenced Schema

For complex schemas, keep them in a separate JSON file.

**Directory layout:**

```
skills/
  data-analysis/
    SKILL.md
    resources/
      analysis.schema.json
```

**SKILL.md:**

```yaml
---
name: data-analysis
description: Analyze datasets and produce structured findings.
output_schema_file: resources/analysis.schema.json
---

# Data Analysis

Analyze the provided dataset and return structured findings.
```

**resources/analysis.schema.json:**

```json
{
  "type": "object",
  "required": ["summary", "findings", "recommendations"],
  "properties": {
    "summary": { "type": "string" },
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["metric", "value", "trend"],
        "properties": {
          "metric": { "type": "string" },
          "value": { "type": "number" },
          "trend": { "type": "string", "enum": ["up", "down", "stable"] }
        }
      }
    },
    "recommendations": {
      "type": "array",
      "items": { "type": "string" }
    }
  }
}
```

The path is resolved relative to the skill directory. The skills plugin loads and parses the file at activation time.

### 3. Embedder Requesting Structured Output

Embedders can set `ResponseFormat` directly on `LLMRequest`, bypassing the registry entirely.

```go
// In your embedder code:
req := events.LLMRequest{
    Messages: []events.Message{
        {Role: "user", Content: "Analyze this resume..."},
    },
    ResponseFormat: &events.ResponseFormat{
        Type: "json_schema",
        Name: "candidate_score",
        Schema: map[string]any{
            "type":     "object",
            "required": []string{"name", "score", "reasoning"},
            "properties": map[string]any{
                "name":      map[string]any{"type": "string"},
                "score":     map[string]any{"type": "integer", "minimum": 1, "maximum": 10},
                "reasoning": map[string]any{"type": "string"},
            },
        },
        Strict: true,
    },
    MaxTokens: 4096,
    Stream:    true,
}

_ = bus.Emit("llm.request", req)
```

Use direct `ResponseFormat` when:
- The schema is known at compile time and won't change
- You don't need the registry's indirection
- You're building a focused embedder app, not a plugin

### 4. Plugin Injecting Schema via `before:llm.request`

A custom plugin can dynamically select schemas based on conversation state.

```go
func (p *MyPlugin) handleBeforeLLMRequest(event engine.Event[any]) {
    vp, ok := event.Payload.(*engine.VetoablePayload)
    if !ok {
        return
    }
    req, ok := vp.Original.(*events.LLMRequest)
    if !ok {
        return
    }

    // Dynamic schema selection based on request context.
    if p.shouldUseStructuredOutput(req) {
        if req.Metadata == nil {
            req.Metadata = make(map[string]any)
        }
        req.Metadata["_expects_schema"] = "my_plugin.output_schema"
    }
}
```

This pattern works when:
- Schema varies based on conversation state or user input
- Plugin registers multiple schemas and picks one per request
- You want registry-based resolution but custom tagging logic

### 5. Belt-and-Suspenders with `json_schema` Gate

Enable both structured output and the `json_schema` gate for defense-in-depth.

```yaml
plugins:
  active:
    - nexus.skills
    - nexus.gate.json_schema
    - nexus.llm.openai
    - nexus.agent.react

  nexus.gate.json_schema:
    schema:
      type: object
      required: [entities]
      properties:
        entities:
          type: array
    max_retries: 2
```

**How they interact:**

- Schema Registry drives generation — provider sends structured output request
- `json_schema` gate validates output — checks `_structured_output` metadata
- When `_structured_output` is `true` (provider enforced), the gate skips validation
- When `_structured_output` is absent or `false`, the gate validates and retries as usual

This gives you native enforcement where available plus validation fallback everywhere else.

## Provider Behavior

| Provider | `json_object` | `json_schema` | Metadata |
|----------|--------------|---------------|----------|
| **OpenAI** | Native `response_format` | Native `response_format` with strict mode | `_structured_output: true` |
| **Anthropic** | Not supported | Simulated via tool-use-as-schema | `_structured_output: true` |
| **Unknown** | Ignored | Ignored | No flag set |

### Anthropic Simulation Details

Since Anthropic doesn't support `response_format`, the provider simulates `json_schema` mode:

1. Injects a synthetic tool `_structured_output` with the schema as its `input_schema`
2. Forces `tool_choice` to `{"type": "tool", "name": "_structured_output"}`
3. Claude returns structured data as tool call arguments
4. Provider unwraps tool arguments back into `LLMResponse.Content`
5. During streaming, tool input deltas are emitted as content chunks

This overrides any existing `ToolChoice` on the request.
