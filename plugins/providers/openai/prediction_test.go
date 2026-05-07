package openai

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/pricing"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestBuildRequestBody_PredictionSet verifies that LLMRequest.Prediction is
// serialized as {type: "content", content: <prediction>}.
func TestBuildRequestBody_PredictionSet(t *testing.T) {
	p := &Plugin{}
	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{{Role: "user", Content: "rewrite please"}},
		Prediction: "the known target text",
	}

	body := p.buildRequestBody("gpt-4o", 1024, req)

	pred, ok := body["prediction"].(map[string]any)
	if !ok {
		t.Fatalf("expected prediction to be a map[string]any, got %T", body["prediction"])
	}
	if pred["type"] != "content" {
		t.Errorf("prediction.type: got %v, want \"content\"", pred["type"])
	}
	if pred["content"] != "the known target text" {
		t.Errorf("prediction.content: got %v, want \"the known target text\"", pred["content"])
	}
}

// TestBuildRequestBody_PredictionEmpty verifies no prediction field is added
// when LLMRequest.Prediction is the empty string.
func TestBuildRequestBody_PredictionEmpty(t *testing.T) {
	p := &Plugin{}
	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{{Role: "user", Content: "hi"}}} // Prediction left empty

	body := p.buildRequestBody("gpt-4o", 1024, req)

	if _, ok := body["prediction"]; ok {
		t.Errorf("expected no prediction field on empty Prediction, got %v", body["prediction"])
	}
}

// TestBuildRequestBody_PredictionStrippedOnReasoningModel verifies that
// applyReasoning strips the prediction field for reasoning models that
// reject it (o1, o3, o4, gpt-5*-thinking).
func TestBuildRequestBody_PredictionStrippedOnReasoningModel(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{{Role: "user", Content: "rewrite"}},
		Prediction: "the target",
	}

	body := p.buildRequestBody("o1-mini", 1024, req)

	if _, ok := body["prediction"]; ok {
		t.Errorf("expected prediction stripped on reasoning model o1-mini, got %v", body["prediction"])
	}
}

// TestConvertAPIResponse_PredictionAcceptancePopulated verifies that nonzero
// accepted/rejected counts surface as Metadata["prediction_acceptance"].
func TestConvertAPIResponse_PredictionAcceptancePopulated(t *testing.T) {
	p := &Plugin{pricing: pricing.DefaultsFor(pricing.ProviderOpenAI)}

	content := "edited text"
	apiResp := apiResponse{
		ID:    "chatcmpl-test",
		Model: "gpt-4o",
		Choices: []apiChoice{
			{
				Index:        0,
				Message:      apiMessage{Role: "assistant", Content: &content},
				FinishReason: "stop",
			},
		},
	}
	apiResp.Usage.PromptTokens = 100
	apiResp.Usage.CompletionTokens = 50
	apiResp.Usage.TotalTokens = 150
	apiResp.Usage.CompletionTokensDetails.AcceptedPredictionTokens = 32
	apiResp.Usage.CompletionTokensDetails.RejectedPredictionTokens = 8

	resp := p.convertAPIResponse(apiResp)

	pa, ok := resp.Metadata["prediction_acceptance"].(map[string]any)
	if !ok {
		t.Fatalf("expected prediction_acceptance to be a map[string]any, got %T", resp.Metadata["prediction_acceptance"])
	}
	if pa["accepted"] != 32 {
		t.Errorf("accepted: got %v, want 32", pa["accepted"])
	}
	if pa["rejected"] != 8 {
		t.Errorf("rejected: got %v, want 8", pa["rejected"])
	}
}

// TestConvertAPIResponse_PredictionAcceptanceAbsentOnZero verifies that when
// both counts are zero, the prediction_acceptance key is not set.
func TestConvertAPIResponse_PredictionAcceptanceAbsentOnZero(t *testing.T) {
	p := &Plugin{pricing: pricing.DefaultsFor(pricing.ProviderOpenAI)}

	content := "hi"
	apiResp := apiResponse{
		ID:    "chatcmpl-test",
		Model: "gpt-4o",
		Choices: []apiChoice{
			{
				Index:        0,
				Message:      apiMessage{Role: "assistant", Content: &content},
				FinishReason: "stop",
			},
		},
	}
	apiResp.Usage.PromptTokens = 100
	apiResp.Usage.CompletionTokens = 50
	apiResp.Usage.TotalTokens = 150
	// Both prediction counts are zero.

	resp := p.convertAPIResponse(apiResp)

	if _, ok := resp.Metadata["prediction_acceptance"]; ok {
		t.Errorf("expected no prediction_acceptance key on zero counts, got %v", resp.Metadata["prediction_acceptance"])
	}
}

// TestMergeMetadata_RequestMetaAndPredictionBothSurface verifies that when
// both currentRequestMeta (e.g. _structured_output) and prediction metadata
// exist, both surface on the final response — neither overwrites the other.
func TestMergeMetadata_RequestMetaAndPredictionBothSurface(t *testing.T) {
	predMeta := map[string]any{
		"prediction_acceptance": map[string]any{
			"accepted": 10,
			"rejected": 2,
		},
	}
	reqMeta := map[string]any{
		"_structured_output": true,
	}

	merged := mergeMetadata(predMeta, reqMeta)

	if merged["_structured_output"] != true {
		t.Errorf("_structured_output: got %v, want true", merged["_structured_output"])
	}
	pa, ok := merged["prediction_acceptance"].(map[string]any)
	if !ok {
		t.Fatalf("prediction_acceptance: got %T, want map[string]any", merged["prediction_acceptance"])
	}
	if pa["accepted"] != 10 {
		t.Errorf("prediction_acceptance.accepted: got %v, want 10", pa["accepted"])
	}
}

// TestMergeMetadata_NilInputs verifies that two nil inputs yield nil (no
// allocation), and that one nil + one populated yields the populated keys.
func TestMergeMetadata_NilInputs(t *testing.T) {
	if got := mergeMetadata(nil, nil); got != nil {
		t.Errorf("mergeMetadata(nil, nil): got %v, want nil", got)
	}
	from := map[string]any{"k": "v"}
	got := mergeMetadata(nil, from)
	if got["k"] != "v" {
		t.Errorf("mergeMetadata(nil, from): got %v, want {k:v}", got)
	}
	got = mergeMetadata(from, nil)
	if got["k"] != "v" {
		t.Errorf("mergeMetadata(from, nil): got %v, want {k:v}", got)
	}
}

// TestStreamFinalize_PredictionAcceptanceSurfaces verifies that the streaming
// path emits prediction_acceptance metadata when the final usage chunk reports
// accepted/rejected counts AND merges with currentRequestMeta passthrough.
func TestStreamFinalize_PredictionAcceptanceSurfaces(t *testing.T) {
	bus := engine.NewEventBus()

	var (
		mu   sync.Mutex
		resp events.LLMResponse
		got  bool
	)
	unsub := bus.Subscribe("llm.response", func(ev engine.Event[any]) {
		mu.Lock()
		defer mu.Unlock()
		if r, ok := ev.Payload.(events.LLMResponse); ok {
			resp = r
			got = true
		}
	})
	defer unsub()

	p := &Plugin{
		bus:                bus,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		pricing:            pricing.DefaultsFor(pricing.ProviderOpenAI),
		currentRequestMeta: map[string]any{"_structured_output": true},
	}

	// Synthetic SSE stream: content delta, then final chunk with usage,
	// then [DONE]. Usage carries prediction counts.
	stream := strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"completion_tokens_details":{"accepted_prediction_tokens":3,"rejected_prediction_tokens":1}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	p.handleStreamResponse(strings.NewReader(stream))

	mu.Lock()
	defer mu.Unlock()
	if !got {
		t.Fatalf("expected llm.response emission, got none")
	}
	if resp.Metadata == nil {
		t.Fatalf("expected metadata on streamed response, got nil")
	}
	if resp.Metadata["_structured_output"] != true {
		t.Errorf("_structured_output passthrough lost: got %v", resp.Metadata["_structured_output"])
	}
	pa, ok := resp.Metadata["prediction_acceptance"].(map[string]any)
	if !ok {
		t.Fatalf("expected prediction_acceptance on streamed response metadata, got %T", resp.Metadata["prediction_acceptance"])
	}
	if pa["accepted"] != 3 {
		t.Errorf("streamed accepted: got %v, want 3", pa["accepted"])
	}
	if pa["rejected"] != 1 {
		t.Errorf("streamed rejected: got %v, want 1", pa["rejected"])
	}
}
