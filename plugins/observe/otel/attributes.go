package otel

import (
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"

	"go.opentelemetry.io/otel/attribute"
)

// extractAttributes returns OTel span attributes for known event payload types.
func extractAttributes(payload any) []attribute.KeyValue {
	// Unwrap vetoable payloads.
	if vp, ok := payload.(*engine.VetoablePayload); ok {
		attrs := extractAttributes(vp.Original)
		if vp.Veto.Vetoed {
			attrs = append(attrs,
				attribute.Bool("nexus.veto.vetoed", true),
				attribute.String("nexus.veto.reason", vp.Veto.Reason),
			)
		}
		return attrs
	}

	switch p := payload.(type) {
	case *events.LLMRequest:
		return llmRequestAttrs(p)
	case *events.LLMResponse:
		return llmResponseAttrs(p)
	case *events.StreamEnd:
		return streamEndAttrs(p)
	case *events.ToolCall:
		return toolCallAttrs(p)
	case *events.ToolResult:
		return toolResultAttrs(p)
	case *events.TurnInfo:
		return turnInfoAttrs(p)
	case *events.SubagentSpawn:
		return subagentSpawnAttrs(p)
	case *events.SubagentComplete:
		return subagentCompleteAttrs(p)
	case *events.ErrorInfo:
		return errorInfoAttrs(p)
	default:
		return nil
	}
}

func llmRequestAttrs(r *events.LLMRequest) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("nexus.llm.role", r.Role),
		attribute.String("nexus.llm.model", r.Model),
		attribute.Int("nexus.llm.max_tokens", r.MaxTokens),
		attribute.Bool("nexus.llm.stream", r.Stream),
		attribute.Int("nexus.llm.message_count", len(r.Messages)),
		attribute.Int("nexus.llm.tool_count", len(r.Tools)),
	}
	if r.Temperature != nil {
		attrs = append(attrs, attribute.Float64("nexus.llm.temperature", *r.Temperature))
	}
	return attrs
}

func llmResponseAttrs(r *events.LLMResponse) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("nexus.llm.model", r.Model),
		attribute.String("nexus.llm.finish_reason", r.FinishReason),
		attribute.Int("nexus.llm.usage.prompt_tokens", r.Usage.PromptTokens),
		attribute.Int("nexus.llm.usage.completion_tokens", r.Usage.CompletionTokens),
		attribute.Int("nexus.llm.usage.total_tokens", r.Usage.TotalTokens),
		attribute.Int("nexus.llm.tool_call_count", len(r.ToolCalls)),
	}
}

func streamEndAttrs(s *events.StreamEnd) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("nexus.llm.turn_id", s.TurnID),
		attribute.String("nexus.llm.finish_reason", s.FinishReason),
		attribute.Int("nexus.llm.usage.prompt_tokens", s.Usage.PromptTokens),
		attribute.Int("nexus.llm.usage.completion_tokens", s.Usage.CompletionTokens),
		attribute.Int("nexus.llm.usage.total_tokens", s.Usage.TotalTokens),
	}
}

func toolCallAttrs(t *events.ToolCall) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("nexus.tool.id", t.ID),
		attribute.String("nexus.tool.name", t.Name),
		attribute.String("nexus.tool.turn_id", t.TurnID),
	}
}

func toolResultAttrs(t *events.ToolResult) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("nexus.tool.id", t.ID),
		attribute.String("nexus.tool.name", t.Name),
		attribute.String("nexus.tool.turn_id", t.TurnID),
		attribute.Bool("nexus.tool.has_error", t.Error != ""),
	}
	if t.Error != "" {
		attrs = append(attrs, attribute.String("nexus.tool.error", t.Error))
	}
	return attrs
}

func turnInfoAttrs(t *events.TurnInfo) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("nexus.agent.turn_id", t.TurnID),
		attribute.Int("nexus.agent.iteration", t.Iteration),
		attribute.String("nexus.agent.session_id", t.SessionID),
	}
}

func subagentSpawnAttrs(s *events.SubagentSpawn) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("nexus.subagent.spawn_id", s.SpawnID),
		attribute.String("nexus.subagent.task", s.Task),
		attribute.String("nexus.subagent.model_role", s.ModelRole),
		attribute.String("nexus.subagent.parent_turn_id", s.ParentTurnID),
	}
}

func subagentCompleteAttrs(s *events.SubagentComplete) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("nexus.subagent.spawn_id", s.SpawnID),
		attribute.Int("nexus.subagent.iterations", s.Iterations),
		attribute.Int("nexus.subagent.usage.total_tokens", s.TokensUsed.TotalTokens),
		attribute.String("nexus.subagent.parent_turn_id", s.ParentTurnID),
		attribute.Bool("nexus.subagent.has_error", s.Error != ""),
	}
	if s.Error != "" {
		attrs = append(attrs, attribute.String("nexus.subagent.error", s.Error))
	}
	return attrs
}

func errorInfoAttrs(e *events.ErrorInfo) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("nexus.error.source", e.Source),
		attribute.Bool("nexus.error.fatal", e.Fatal),
	}
	if e.Err != nil {
		attrs = append(attrs, attribute.String("nexus.error.message", e.Err.Error()))
	}
	return attrs
}
