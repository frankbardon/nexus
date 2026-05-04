package anthropic

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/pricing"
	"github.com/frankbardon/nexus/pkg/events"
)

// --- parseThinkingConfig ----------------------------------------------------

// TestParseThinkingConfig_Absent verifies that omitting the `thinking` block
// produces a zero-value config so applyThinking is a no-op and no event is
// emitted on the bus.
func TestParseThinkingConfig_Absent(t *testing.T) {
	tc := parseThinkingConfig(map[string]any{})
	if tc.Enabled || tc.BudgetTokens != 0 || tc.IncludeThoughts {
		t.Fatalf("expected zero config, got %+v", tc)
	}
}

// TestParseThinkingConfig_Defaults verifies that a present-but-minimal block
// fills in the high-leverage defaults: 8192-token budget and IncludeThoughts
// true so callers only need `enabled: true`.
func TestParseThinkingConfig_Defaults(t *testing.T) {
	tc := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{
			"enabled": true,
		},
	})
	if !tc.Enabled {
		t.Fatal("expected enabled=true")
	}
	if tc.BudgetTokens != 8192 {
		t.Errorf("BudgetTokens: got %d, want 8192", tc.BudgetTokens)
	}
	if !tc.IncludeThoughts {
		t.Errorf("IncludeThoughts: got false, want true (default)")
	}
}

// TestParseThinkingConfig_Custom verifies explicit overrides take effect.
func TestParseThinkingConfig_Custom(t *testing.T) {
	tc := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{
			"enabled":          true,
			"budget_tokens":    16384,
			"include_thoughts": false,
		},
	})
	if !tc.Enabled || tc.BudgetTokens != 16384 || tc.IncludeThoughts {
		t.Fatalf("unexpected config: %+v", tc)
	}
}

// TestParseThinkingConfig_DynamicBudget verifies -1 (dynamic budget) is
// preserved as-is so the request body forwards it to Anthropic.
func TestParseThinkingConfig_DynamicBudget(t *testing.T) {
	tc := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{
			"enabled":       true,
			"budget_tokens": -1,
		},
	})
	if tc.BudgetTokens != -1 {
		t.Fatalf("BudgetTokens: got %d, want -1", tc.BudgetTokens)
	}
}

// TestParseThinkingConfig_FloatBudget covers YAML decoders that surface
// integer values as float64 — the parser should still extract the value.
func TestParseThinkingConfig_FloatBudget(t *testing.T) {
	tc := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{
			"enabled":       true,
			"budget_tokens": float64(4096),
		},
	})
	if tc.BudgetTokens != 4096 {
		t.Fatalf("BudgetTokens: got %d, want 4096", tc.BudgetTokens)
	}
}

// --- applyThinking ----------------------------------------------------------

func silentTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestApplyThinking_Disabled is a no-op — body comes back unchanged.
func TestApplyThinking_Disabled(t *testing.T) {
	body := map[string]any{"max_tokens": 1024}
	applyThinking(body, thinkingConfig{}, silentTestLogger())
	if _, ok := body["thinking"]; ok {
		t.Fatal("disabled config should not set thinking")
	}
	if len(body) != 1 {
		t.Fatalf("body mutated unexpectedly: %+v", body)
	}
}

// TestApplyThinking_BudgetZero treats budget=0 as disabled (mirrors gemini).
func TestApplyThinking_BudgetZero(t *testing.T) {
	body := map[string]any{}
	applyThinking(body, thinkingConfig{Enabled: true, BudgetTokens: 0}, silentTestLogger())
	if _, ok := body["thinking"]; ok {
		t.Fatal("budget=0 should not set thinking")
	}
}

// TestApplyThinking_Enabled produces the correct request shape.
func TestApplyThinking_Enabled(t *testing.T) {
	body := map[string]any{}
	applyThinking(body, thinkingConfig{Enabled: true, BudgetTokens: 8192}, silentTestLogger())

	got, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking missing or wrong type: %T", body["thinking"])
	}
	if got["type"] != "enabled" {
		t.Errorf("type: got %v, want enabled", got["type"])
	}
	if got["budget_tokens"] != 8192 {
		t.Errorf("budget_tokens: got %v, want 8192", got["budget_tokens"])
	}
}

// TestApplyThinking_StripsTemperature verifies non-1.0 temperature is removed
// and a warning is logged. Anthropic requires temp=1 (or unset) when thinking
// is enabled.
func TestApplyThinking_StripsTemperature(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	body := map[string]any{"temperature": 0.7}
	applyThinking(body, thinkingConfig{Enabled: true, BudgetTokens: 4096}, logger)

	if _, ok := body["temperature"]; ok {
		t.Fatal("temperature should have been stripped")
	}
	if !strings.Contains(buf.String(), "stripping temperature") {
		t.Errorf("expected warning log, got: %q", buf.String())
	}
}

// TestApplyThinking_TemperatureOneNoWarn — temp=1.0 is silently dropped (it's
// the value Anthropic would have used anyway), no warning emitted.
func TestApplyThinking_TemperatureOneNoWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	body := map[string]any{"temperature": 1.0}
	applyThinking(body, thinkingConfig{Enabled: true, BudgetTokens: 4096}, logger)

	if _, ok := body["temperature"]; ok {
		t.Fatal("temperature should have been removed")
	}
	if buf.Len() != 0 {
		t.Errorf("temp=1.0 should not log a warning, got: %q", buf.String())
	}
}

// TestApplyThinking_NoTemperatureNoWarn — when caller never set temperature,
// applyThinking is silent and leaves body otherwise alone.
func TestApplyThinking_NoTemperatureNoWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	body := map[string]any{"max_tokens": 4096}
	applyThinking(body, thinkingConfig{Enabled: true, BudgetTokens: 4096}, logger)

	if buf.Len() != 0 {
		t.Errorf("no temperature should not log, got: %q", buf.String())
	}
}

// --- prependThinkingBlocks --------------------------------------------------

// TestPrependThinkingBlocks_Native — the native []map[string]any shape (as
// emitted by the provider in-process) round-trips through unchanged.
func TestPrependThinkingBlocks_Native(t *testing.T) {
	meta := map[string]any{
		"thinking_blocks": []map[string]any{
			{"type": "thinking", "thinking": "step 1", "signature": "sig-A"},
		},
	}
	got := prependThinkingBlocks(meta)
	if len(got) != 1 {
		t.Fatalf("len: got %d, want 1", len(got))
	}
	if got[0]["signature"] != "sig-A" {
		t.Errorf("signature lost: %+v", got[0])
	}
}

// TestPrependThinkingBlocks_FromJSON — after a JSONL persistence round-trip
// the slice arrives as []any{map[string]any{...}}; the helper must recover.
func TestPrependThinkingBlocks_FromJSON(t *testing.T) {
	raw := `{"thinking_blocks": [{"type":"thinking","thinking":"x","signature":"sig"}]}`
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}
	got := prependThinkingBlocks(meta)
	if len(got) != 1 || got[0]["signature"] != "sig" {
		t.Fatalf("post-JSON recovery failed: %+v", got)
	}
}

// TestPrependThinkingBlocks_Absent returns nil for missing/nil metadata.
func TestPrependThinkingBlocks_Absent(t *testing.T) {
	if prependThinkingBlocks(nil) != nil {
		t.Error("nil metadata should return nil")
	}
	if prependThinkingBlocks(map[string]any{}) != nil {
		t.Error("missing key should return nil")
	}
	if prependThinkingBlocks(map[string]any{"thinking_blocks": "bad"}) != nil {
		t.Error("wrong type should return nil")
	}
}

// --- convertMessage round-trip ---------------------------------------------

// TestConvertMessage_PrependsThinkingBeforeToolUse verifies the critical
// invariant: when an assistant message has BOTH thinking_blocks (from the
// previous response) AND tool_calls, the request payload places thinking
// blocks FIRST, before any tool_use blocks, with their signatures preserved
// verbatim. Anthropic returns HTTP 400 when this ordering is violated on a
// turn that follows a tool_result.
func TestConvertMessage_PrependsThinkingBeforeToolUse(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}

	msg := events.Message{
		Role:    "assistant",
		Content: "I'll fetch that.",
		ToolCalls: []events.ToolCallRequest{
			{ID: "toolu_1", Name: "fetch", Arguments: `{"url":"x"}`},
		},
		Metadata: map[string]any{
			"thinking_blocks": []map[string]any{
				{"type": "thinking", "thinking": "I should call fetch.", "signature": "sig-XYZ"},
				{"type": "redacted_thinking", "data": "encrypted-blob"},
			},
		},
	}

	api := p.convertMessage(msg)
	content, ok := api["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content wrong type: %T", api["content"])
	}

	// Expected ordering:
	//   [0] thinking
	//   [1] redacted_thinking
	//   [2] text  ("I'll fetch that.")
	//   [3] tool_use
	if len(content) != 4 {
		t.Fatalf("content len: got %d, want 4 (%+v)", len(content), content)
	}
	if content[0]["type"] != "thinking" {
		t.Errorf("[0] type: got %v, want thinking", content[0]["type"])
	}
	if content[0]["signature"] != "sig-XYZ" {
		t.Errorf("[0] signature lost: %+v", content[0])
	}
	if content[1]["type"] != "redacted_thinking" {
		t.Errorf("[1] type: got %v, want redacted_thinking", content[1]["type"])
	}
	if content[1]["data"] != "encrypted-blob" {
		t.Errorf("[1] data lost: %+v", content[1])
	}
	if content[2]["type"] != "text" {
		t.Errorf("[2] type: got %v, want text", content[2]["type"])
	}
	if content[3]["type"] != "tool_use" {
		t.Errorf("[3] type: got %v, want tool_use", content[3]["type"])
	}
}

// TestConvertMessage_NoThinkingNoOp confirms that messages without metadata
// produce the legacy content shape (no leading thinking blocks).
func TestConvertMessage_NoThinkingNoOp(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	msg := events.Message{
		Role:    "assistant",
		Content: "ok",
		ToolCalls: []events.ToolCallRequest{
			{ID: "id1", Name: "t", Arguments: `{}`},
		},
	}
	api := p.convertMessage(msg)
	content := api["content"].([]map[string]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 blocks (text + tool_use), got %d", len(content))
	}
	if content[0]["type"] != "text" {
		t.Errorf("[0] type: got %v, want text", content[0]["type"])
	}
}

// --- SSE round-trip --------------------------------------------------------

// busRecorder collects every event the provider emits during a stream so
// tests can assert on thinking.step + llm.response shape without spinning
// up the full engine.
type busRecorder struct {
	mu     sync.Mutex
	bus    engine.EventBus
	events []recorded
}

type recorded struct {
	Type    string
	Payload any
}

func newBusRecorder() *busRecorder {
	r := &busRecorder{bus: engine.NewEventBus()}
	r.bus.SubscribeAll(func(e engine.Event[any]) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, recorded{Type: e.Type, Payload: e.Payload})
	})
	return r
}

func (r *busRecorder) byType(t string) []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recorded
	for _, ev := range r.events {
		if ev.Type == t {
			out = append(out, ev)
		}
	}
	return out
}

// canned SSE payload mirroring Anthropic's extended-thinking stream:
//
//	message_start
//	  → content_block_start (thinking)
//	    → thinking_delta x2
//	    → signature_delta
//	  → content_block_stop
//	  → content_block_start (text)
//	    → text_delta
//	  → content_block_stop
//	message_delta (stop_reason)
//	message_stop
const canonicalThinkingStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_test","model":"claude-sonnet-4-5-20250514","usage":{"input_tokens":50,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"about this carefully."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-canonical"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Final answer."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

// TestStream_ExtendedThinkingRoundTrip drives the SSE state machine end-to-end
// with a canned thinking-enabled response and asserts:
//
//   - thinking.step events are emitted (one per thinking_delta) when
//     IncludeThoughts is on.
//   - The final llm.response carries Metadata["thinking_blocks"] populated
//     with the block AND its signature.
//   - llm.response.Content is the text portion only (thinking is excluded).
func TestStream_ExtendedThinkingRoundTrip(t *testing.T) {
	rec := newBusRecorder()

	p := &Plugin{
		bus:      rec.bus,
		logger:   silentTestLogger(),
		thinking: thinkingConfig{Enabled: true, BudgetTokens: 8192, IncludeThoughts: true},
		pricing:  pricing.DefaultsFor(pricing.ProviderAnthropic),
	}

	p.handleStreamResponse(strings.NewReader(canonicalThinkingStream))

	// Assert: thinking.step emitted twice (one per thinking_delta).
	steps := rec.byType("thinking.step")
	if len(steps) != 2 {
		t.Fatalf("thinking.step events: got %d, want 2", len(steps))
	}
	step0 := steps[0].Payload.(events.ThinkingStep)
	if step0.Content != "Let me think " {
		t.Errorf("step[0] content: got %q", step0.Content)
	}
	if step0.TurnID != "msg_test" {
		t.Errorf("step[0] turn id: got %q, want msg_test", step0.TurnID)
	}
	if step0.Index != 0 {
		t.Errorf("step[0] index: got %d, want 0", step0.Index)
	}
	step1 := steps[1].Payload.(events.ThinkingStep)
	if step1.Index != 1 {
		t.Errorf("step[1] index: got %d, want 1", step1.Index)
	}

	// Assert: final llm.response shape.
	resps := rec.byType("llm.response")
	if len(resps) != 1 {
		t.Fatalf("llm.response count: got %d, want 1", len(resps))
	}
	resp := resps[0].Payload.(events.LLMResponse)

	if resp.Content != "Final answer." {
		t.Errorf("response content: got %q, want %q", resp.Content, "Final answer.")
	}

	tb, ok := resp.Metadata["thinking_blocks"].([]map[string]any)
	if !ok {
		t.Fatalf("Metadata[thinking_blocks] missing or wrong type: %T", resp.Metadata["thinking_blocks"])
	}
	if len(tb) != 1 {
		t.Fatalf("thinking_blocks len: got %d, want 1", len(tb))
	}
	if tb[0]["type"] != "thinking" {
		t.Errorf("block type: got %v, want thinking", tb[0]["type"])
	}
	if tb[0]["thinking"] != "Let me think about this carefully." {
		t.Errorf("aggregated thinking: got %q", tb[0]["thinking"])
	}
	if tb[0]["signature"] != "sig-canonical" {
		t.Errorf("signature lost: got %q", tb[0]["signature"])
	}
}

// TestStream_IncludeThoughtsFalseSilencesEvents — same canned payload, but
// IncludeThoughts disabled. Thinking blocks must still be captured into
// Metadata (round-trip needs them) but no thinking.step events fire.
func TestStream_IncludeThoughtsFalseSilencesEvents(t *testing.T) {
	rec := newBusRecorder()

	p := &Plugin{
		bus:      rec.bus,
		logger:   silentTestLogger(),
		thinking: thinkingConfig{Enabled: true, BudgetTokens: 8192, IncludeThoughts: false},
		pricing:  pricing.DefaultsFor(pricing.ProviderAnthropic),
	}

	p.handleStreamResponse(strings.NewReader(canonicalThinkingStream))

	if got := len(rec.byType("thinking.step")); got != 0 {
		t.Errorf("thinking.step suppressed but %d emitted", got)
	}

	resp := rec.byType("llm.response")[0].Payload.(events.LLMResponse)
	tb, ok := resp.Metadata["thinking_blocks"].([]map[string]any)
	if !ok || len(tb) != 1 {
		t.Fatalf("thinking_blocks missing despite include_thoughts=false: %+v", resp.Metadata)
	}
	if tb[0]["signature"] != "sig-canonical" {
		t.Error("signature dropped")
	}
}
