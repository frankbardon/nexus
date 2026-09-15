package engine

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// collectResponses subscribes to llm.response and returns a pointer to the
// slice of everything published during the test.
func collectResponses(bus EventBus) *[]events.LLMResponse {
	got := &[]events.LLMResponse{}
	bus.Subscribe("llm.response", func(e Event[any]) {
		resp, ok := e.Payload.(events.LLMResponse)
		if !ok {
			return
		}
		*got = append(*got, resp)
	})
	return got
}

// beforeResponse subscribes a handler to the vetoable hook, handing it the
// unwrapped *events.LLMResponse.
func beforeResponse(bus EventBus, fn func(*events.LLMResponse, *VetoablePayload), opts ...SubscribeOption) {
	bus.Subscribe("before:llm.response", func(e Event[any]) {
		vp, ok := e.Payload.(*VetoablePayload)
		if !ok {
			return
		}
		resp, ok := vp.Original.(*events.LLMResponse)
		if !ok {
			return
		}
		fn(resp, vp)
	}, opts...)
}

func TestPublishLLMResponse_NoHandlers_PassesThrough(t *testing.T) {
	bus := NewEventBus()
	got := collectResponses(bus)

	in := events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		RequestID:     "req-1",
		Content:       "the model's answer",
		Model:         "test-model",
	}

	out, substituted := PublishLLMResponse(bus, in)
	if substituted {
		t.Fatal("no handlers registered, response must not be substituted")
	}
	if out.Content != "the model's answer" {
		t.Fatalf("content = %q, want the model's answer", out.Content)
	}
	if len(*got) != 1 {
		t.Fatalf("published %d llm.response events, want exactly 1", len(*got))
	}
	if (*got)[0].Content != "the model's answer" {
		t.Fatalf("published content = %q", (*got)[0].Content)
	}
}

// A handler that rewrites the payload without vetoing has its edits published
// as the model's response — the mutate-to-replace path.
func TestPublishLLMResponse_MutateWithoutVeto_Replaces(t *testing.T) {
	bus := NewEventBus()
	got := collectResponses(bus)

	beforeResponse(bus, func(resp *events.LLMResponse, _ *VetoablePayload) {
		resp.Content = "rewritten by the gate"
	})

	out, substituted := PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "original",
	})

	if substituted {
		t.Fatal("mutation without veto must not count as a substitution")
	}
	if out.Content != "rewritten by the gate" {
		t.Fatalf("returned content = %q, want the mutated text", out.Content)
	}
	if len(*got) != 1 || (*got)[0].Content != "rewritten by the gate" {
		t.Fatalf("published = %+v, want the mutated text exactly once", *got)
	}
	if (*got)[0].FinishReason == FinishReasonVetoed {
		t.Fatal("a non-vetoed response must not be marked vetoed")
	}
}

// A veto with no replacement content publishes the veto reason, so the agent
// loop can see why it was blocked.
func TestPublishLLMResponse_VetoWithoutContent_UsesReason(t *testing.T) {
	bus := NewEventBus()
	got := collectResponses(bus)

	beforeResponse(bus, func(_ *events.LLMResponse, vp *VetoablePayload) {
		vp.Veto = VetoResult{Vetoed: true, Reason: "contains a banned term"}
	})

	out, substituted := PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "the banned answer",
	})

	if !substituted {
		t.Fatal("veto must report a substitution")
	}
	if out.Content != "contains a banned term" {
		t.Fatalf("content = %q, want the veto reason", out.Content)
	}
	if out.FinishReason != FinishReasonVetoed {
		t.Fatalf("finish reason = %q, want %q", out.FinishReason, FinishReasonVetoed)
	}
	if vetoed, _ := out.Metadata[MetaVetoed].(bool); !vetoed {
		t.Fatal("substitute must be stamped with the vetoed metadata flag")
	}
	if reason, _ := out.Metadata[MetaVetoReason].(string); reason != "contains a banned term" {
		t.Fatalf("veto reason metadata = %q", reason)
	}
	if len(*got) != 1 {
		t.Fatalf("published %d llm.response events, want exactly 1", len(*got))
	}
	if (*got)[0].Content != "contains a banned term" {
		t.Fatalf("published content = %q, want the substitute", (*got)[0].Content)
	}
}

// A handler that rewrites Content and then vetoes is dictating the
// replacement; the reason must not overwrite it.
func TestPublishLLMResponse_VetoWithContent_KeepsHandlerText(t *testing.T) {
	bus := NewEventBus()

	beforeResponse(bus, func(resp *events.LLMResponse, vp *VetoablePayload) {
		resp.Content = "I can't help with that request."
		vp.Veto = VetoResult{Vetoed: true, Reason: "policy: self-harm"}
	})

	out, substituted := PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "the unsafe answer",
	})

	if !substituted {
		t.Fatal("veto must report a substitution")
	}
	if out.Content != "I can't help with that request." {
		t.Fatalf("content = %q, want the handler's replacement text", out.Content)
	}
	if reason, _ := out.Metadata[MetaVetoReason].(string); reason != "policy: self-harm" {
		t.Fatalf("veto reason metadata = %q, want the reason even when content was dictated", reason)
	}
}

// The security-critical invariant: a vetoed response must never drive tool
// execution.
func TestPublishLLMResponse_VetoClearsToolCalls(t *testing.T) {
	bus := NewEventBus()
	got := collectResponses(bus)

	beforeResponse(bus, func(_ *events.LLMResponse, vp *VetoablePayload) {
		vp.Veto = VetoResult{Vetoed: true, Reason: "dangerous command"}
	})

	out, _ := PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "running that now",
		ToolCalls: []events.ToolCallRequest{
			{ID: "call-1", Name: "shell", Arguments: `{"command":"rm -rf /"}`},
		},
	})

	if len(out.ToolCalls) != 0 {
		t.Fatalf("returned response carries %d tool calls, want 0", len(out.ToolCalls))
	}
	if len((*got)[0].ToolCalls) != 0 {
		t.Fatalf("published response carries %d tool calls, want 0", len((*got)[0].ToolCalls))
	}
}

// Correlation and accounting fields must survive substitution: the sync-RPC
// helper matches on RequestID, and the token budget gate commits on Usage.
func TestPublishLLMResponse_VetoPreservesCorrelationAndUsage(t *testing.T) {
	bus := NewEventBus()

	beforeResponse(bus, func(_ *events.LLMResponse, vp *VetoablePayload) {
		vp.Veto = VetoResult{Vetoed: true, Reason: "blocked"}
	})

	out, _ := PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		RequestID:     "req-42",
		Model:         "claude-test",
		Content:       "blocked content",
		Usage:         events.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		CostUSD:       0.0123,
		Tags:          map[string]string{"session_id": "s-1"},
	})

	if out.RequestID != "req-42" {
		t.Fatalf("RequestID = %q, want it preserved for correlation", out.RequestID)
	}
	if out.Model != "claude-test" {
		t.Fatalf("Model = %q, want it preserved", out.Model)
	}
	if out.Usage.TotalTokens != 150 {
		t.Fatalf("Usage.TotalTokens = %d, want 150 — the tokens were spent regardless", out.Usage.TotalTokens)
	}
	if out.CostUSD != 0.0123 {
		t.Fatalf("CostUSD = %v, want it preserved", out.CostUSD)
	}
	if out.Tags["session_id"] != "s-1" {
		t.Fatalf("Tags = %v, want them preserved for cost attribution", out.Tags)
	}
}

// Substituting must not write through to the provider's own metadata map,
// which may be shared with the request's passthrough metadata.
func TestPublishLLMResponse_VetoDoesNotMutateCallerMetadata(t *testing.T) {
	bus := NewEventBus()

	beforeResponse(bus, func(_ *events.LLMResponse, vp *VetoablePayload) {
		vp.Veto = VetoResult{Vetoed: true, Reason: "blocked"}
	})

	shared := map[string]any{"_source": "provider"}
	_, _ = PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "x",
		Metadata:      shared,
	})

	if _, ok := shared[MetaVetoed]; ok {
		t.Fatal("substitution wrote through to the caller's metadata map")
	}
}

// A gate that vetoes and then drives a corrective LLM request produces a
// second response; re-offering an already-substituted response to the hook
// would let it veto its own replacement forever.
func TestPublishLLMResponse_AlreadyVetoed_SkipsHook(t *testing.T) {
	bus := NewEventBus()
	got := collectResponses(bus)

	hookCalls := 0
	beforeResponse(bus, func(_ *events.LLMResponse, vp *VetoablePayload) {
		hookCalls++
		vp.Veto = VetoResult{Vetoed: true, Reason: "always vetoes"}
	})

	out, substituted := PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "already replaced",
		Metadata:      map[string]any{MetaVetoed: true, MetaVetoReason: "earlier veto"},
	})

	if hookCalls != 0 {
		t.Fatalf("hook ran %d times on an already-substituted response, want 0", hookCalls)
	}
	if substituted {
		t.Fatal("an already-substituted response must not be substituted again")
	}
	if out.Content != "already replaced" {
		t.Fatalf("content = %q, want it published unchanged", out.Content)
	}
	if len(*got) != 1 {
		t.Fatalf("published %d llm.response events, want exactly 1", len(*got))
	}
}

// Priority ordering and first-veto-wins apply here as on any before:* event.
func TestPublishLLMResponse_FirstVetoWinsByPriority(t *testing.T) {
	bus := NewEventBus()

	beforeResponse(bus, func(_ *events.LLMResponse, vp *VetoablePayload) {
		vp.Veto = VetoResult{Vetoed: true, Reason: "late gate"}
	}, WithPriority(20))
	beforeResponse(bus, func(_ *events.LLMResponse, vp *VetoablePayload) {
		vp.Veto = VetoResult{Vetoed: true, Reason: "early gate"}
	}, WithPriority(10))

	out, _ := PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "x",
	})

	if out.Content != "early gate" {
		t.Fatalf("content = %q, want the lower-priority gate's reason to win", out.Content)
	}
}

// An earlier handler's mutation must be visible to a later one, so gates can
// compose (redact, then ban-check the redacted text).
func TestPublishLLMResponse_MutationsCompose(t *testing.T) {
	bus := NewEventBus()

	beforeResponse(bus, func(resp *events.LLMResponse, _ *VetoablePayload) {
		resp.Content = "[REDACTED]"
	}, WithPriority(10))

	var seen string
	beforeResponse(bus, func(resp *events.LLMResponse, _ *VetoablePayload) {
		seen = resp.Content
	}, WithPriority(20))

	_, _ = PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "my ssn is 123-45-6789",
	})

	if seen != "[REDACTED]" {
		t.Fatalf("second handler saw %q, want the first handler's mutation", seen)
	}
}

func TestPublishLLMResponse_NilBus_IsSafe(t *testing.T) {
	out, substituted := PublishLLMResponse(nil, events.LLMResponse{Content: "x"})
	if substituted || out.Content != "x" {
		t.Fatal("nil bus must return the response untouched")
	}
}
