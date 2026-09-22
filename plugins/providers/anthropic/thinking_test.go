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

func silentTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func warnCaptureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func mustParseThinking(t *testing.T, cfg map[string]any, logger *slog.Logger) thinkingConfig {
	t.Helper()
	tc, err := parseThinkingConfig(cfg, logger)
	if err != nil {
		t.Fatalf("parseThinkingConfig: unexpected error: %v", err)
	}
	return tc
}

// TestParseThinkingConfig_Absent verifies that omitting the `thinking` block
// resolves to mode off, so applyThinking writes nothing at all and no
// thinking.step event is emitted on the bus.
func TestParseThinkingConfig_Absent(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{}, silentTestLogger())
	if tc.Mode != thinkingModeOff {
		t.Errorf("Mode: got %q, want %q", tc.Mode, thinkingModeOff)
	}
	if tc.BudgetTokens != 0 || tc.IncludeThoughts {
		t.Fatalf("expected zero config, got %+v", tc)
	}
}

// TestParseThinkingConfig_PresentNoMode verifies a present-but-minimal block
// resolves to adaptive — the only on-mode the current model families accept —
// with no budget default of any kind.
func TestParseThinkingConfig_PresentNoMode(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{},
	}, silentTestLogger())

	if tc.Mode != thinkingModeAdaptive {
		t.Errorf("Mode: got %q, want %q", tc.Mode, thinkingModeAdaptive)
	}
	if tc.BudgetTokens != 0 {
		t.Errorf("BudgetTokens: got %d, want 0 (no default)", tc.BudgetTokens)
	}
	if !tc.IncludeThoughts {
		t.Errorf("IncludeThoughts: got false, want true (default)")
	}
}

// TestParseThinkingConfig_ExplicitModes verifies each of the four values round
// trips onto the struct.
func TestParseThinkingConfig_ExplicitModes(t *testing.T) {
	for _, mode := range []thinkingMode{thinkingModeAdaptive, thinkingModeDisabled, thinkingModeOff} {
		tc := mustParseThinking(t, map[string]any{
			"thinking": map[string]any{"mode": string(mode)},
		}, silentTestLogger())
		if tc.Mode != mode {
			t.Errorf("Mode: got %q, want %q", tc.Mode, mode)
		}
	}

	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"mode": "budget", "budget_tokens": 16384},
	}, silentTestLogger())
	if tc.Mode != thinkingModeBudget {
		t.Errorf("Mode: got %q, want %q", tc.Mode, thinkingModeBudget)
	}
	if tc.BudgetTokens != 16384 {
		t.Errorf("BudgetTokens: got %d, want 16384", tc.BudgetTokens)
	}
}

// TestParseThinkingConfig_UnknownMode rejects a typo at Init rather than
// shipping it to the API.
func TestParseThinkingConfig_UnknownMode(t *testing.T) {
	_, err := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{"mode": "enabled"},
	}, silentTestLogger())
	if err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
	if !strings.Contains(err.Error(), "thinking.mode") {
		t.Errorf("error should name the key, got: %v", err)
	}
}

// TestParseThinkingConfig_BudgetModeRequiresBudget verifies the missing-budget
// case fails at Init and names the key, not at request time.
func TestParseThinkingConfig_BudgetModeRequiresBudget(t *testing.T) {
	_, err := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{"mode": "budget"},
	}, silentTestLogger())
	if err == nil {
		t.Fatal("expected an error for mode: budget with no budget_tokens")
	}
	if !strings.Contains(err.Error(), "thinking.budget_tokens") {
		t.Errorf("error should name the key, got: %v", err)
	}
}

// TestParseThinkingConfig_FloatBudget covers YAML decoders that surface
// integer values as float64 — the parser should still extract the value.
func TestParseThinkingConfig_FloatBudget(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{
			"mode":          "budget",
			"budget_tokens": float64(4096),
		},
	}, silentTestLogger())
	if tc.BudgetTokens != 4096 {
		t.Fatalf("BudgetTokens: got %d, want 4096", tc.BudgetTokens)
	}
}

// TestParseThinkingConfig_BudgetInfersMode preserves the one legacy shape that
// still works on the wire (Opus 4.6 / Sonnet 4.6), loudly.
func TestParseThinkingConfig_BudgetInfersMode(t *testing.T) {
	var buf bytes.Buffer
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{
			"enabled":       true,
			"budget_tokens": 8192,
		},
	}, warnCaptureLogger(&buf))

	if tc.Mode != thinkingModeBudget {
		t.Errorf("Mode: got %q, want %q", tc.Mode, thinkingModeBudget)
	}
	if tc.BudgetTokens != 8192 {
		t.Errorf("BudgetTokens: got %d, want 8192", tc.BudgetTokens)
	}
	if !strings.Contains(buf.String(), "infers mode: budget") {
		t.Errorf("expected an inference warning, got: %q", buf.String())
	}
}

// TestParseThinkingConfig_EnabledAliasTrue — the deprecated bool still loads,
// with a warning pointing at mode.
func TestParseThinkingConfig_EnabledAliasTrue(t *testing.T) {
	var buf bytes.Buffer
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"enabled": true},
	}, warnCaptureLogger(&buf))

	if tc.Mode != thinkingModeAdaptive {
		t.Errorf("Mode: got %q, want %q", tc.Mode, thinkingModeAdaptive)
	}
	if !strings.Contains(buf.String(), "thinking.mode") {
		t.Errorf("deprecation warning should point at thinking.mode, got: %q", buf.String())
	}
}

// TestParseThinkingConfig_EnabledAliasFalse — `enabled: false` means off (no
// thinking field), not disabled (an explicit {"type":"disabled"}).
func TestParseThinkingConfig_EnabledAliasFalse(t *testing.T) {
	var buf bytes.Buffer
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"enabled": false},
	}, warnCaptureLogger(&buf))

	if tc.Mode != thinkingModeOff {
		t.Errorf("Mode: got %q, want %q", tc.Mode, thinkingModeOff)
	}
	if !strings.Contains(buf.String(), "thinking.mode") {
		t.Errorf("deprecation warning should point at thinking.mode, got: %q", buf.String())
	}
}

// TestParseThinkingConfig_ModeWinsOverEnabled — `enabled` is ignored when
// `mode` is set, and the conflict is warned about.
func TestParseThinkingConfig_ModeWinsOverEnabled(t *testing.T) {
	var buf bytes.Buffer
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"enabled": false, "mode": "adaptive"},
	}, warnCaptureLogger(&buf))

	if tc.Mode != thinkingModeAdaptive {
		t.Errorf("Mode: got %q, want %q", tc.Mode, thinkingModeAdaptive)
	}
	if !strings.Contains(buf.String(), "ignored when thinking.mode is set") {
		t.Errorf("expected an ignored-alias warning, got: %q", buf.String())
	}
}

// TestParseThinkingConfig_IncludeThoughtsOverride verifies the explicit
// override still takes effect.
func TestParseThinkingConfig_IncludeThoughtsOverride(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{
			"mode":             "adaptive",
			"include_thoughts": false,
		},
	}, silentTestLogger())
	if tc.IncludeThoughts {
		t.Fatalf("unexpected config: %+v", tc)
	}
}

// TestParseThinkingConfig_BudgetZeroInfersOff pins the long-documented meaning
// of `budget_tokens: 0` — "disable thinking". Inferring mode: budget from it
// would put {"type":"enabled","budget_tokens":0} on the wire, which is an
// unconditional Anthropic 400.
func TestParseThinkingConfig_BudgetZeroInfersOff(t *testing.T) {
	var buf bytes.Buffer
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"budget_tokens": 0},
	}, warnCaptureLogger(&buf))

	if tc.Mode != thinkingModeOff {
		t.Errorf("Mode: got %q, want %q", tc.Mode, thinkingModeOff)
	}
	if !strings.Contains(buf.String(), "thinking is off") {
		t.Errorf("expected a warning explaining the zero budget, got: %q", buf.String())
	}

	// And nothing reaches the body.
	body := map[string]any{}
	applyThinking(body, tc, silentTestLogger())
	if _, ok := body["thinking"]; ok {
		t.Fatalf("budget_tokens: 0 must not send a thinking field: %+v", body)
	}
}

// TestParseThinkingConfig_BudgetZeroWithExplicitMode — an explicit mode is the
// operator naming the wire shape, so the zero passes straight through and the
// API owns the rejection. Only the *inference* treats 0 as "off".
func TestParseThinkingConfig_BudgetZeroWithExplicitMode(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"mode": "budget", "budget_tokens": 0},
	}, silentTestLogger())

	if tc.Mode != thinkingModeBudget {
		t.Errorf("Mode: got %q, want %q", tc.Mode, thinkingModeBudget)
	}
	if tc.BudgetTokens != 0 {
		t.Errorf("BudgetTokens: got %d, want 0", tc.BudgetTokens)
	}
}

// --- thinking.display -------------------------------------------------------

// TestParseThinkingConfig_DisplayExplicit round trips both legal values.
func TestParseThinkingConfig_DisplayExplicit(t *testing.T) {
	for _, want := range []thinkingDisplay{thinkingDisplaySummarized, thinkingDisplayOmitted} {
		tc := mustParseThinking(t, map[string]any{
			"thinking": map[string]any{"mode": "adaptive", "display": string(want)},
		}, silentTestLogger())
		if tc.Display != want {
			t.Errorf("Display: got %q, want %q", tc.Display, want)
		}
	}
}

// TestParseThinkingConfig_DisplayUnknown rejects a typo at Init rather than
// shipping it to the API.
func TestParseThinkingConfig_DisplayUnknown(t *testing.T) {
	_, err := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{"mode": "adaptive", "display": "updates"},
	}, silentTestLogger())
	if err == nil {
		t.Fatal("expected an error for an unknown display")
	}
	if !strings.Contains(err.Error(), "thinking.display") {
		t.Errorf("error should name the key, got: %v", err)
	}
}

// TestParseThinkingConfig_DisplayInferredFromIncludeThoughts is the whole point
// of the key-infers-key behavior: `omitted` is the API default on the current
// model families and streams thinking blocks with EMPTY text, so without this
// inference include_thoughts emits contentless thinking.step events.
func TestParseThinkingConfig_DisplayInferredFromIncludeThoughts(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"mode": "adaptive", "include_thoughts": true},
	}, silentTestLogger())
	if tc.Display != thinkingDisplaySummarized {
		t.Errorf("Display: got %q, want %q", tc.Display, thinkingDisplaySummarized)
	}

	// include_thoughts defaults to true, so a bare on-mode block infers it too.
	bare := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"mode": "budget", "budget_tokens": 8192},
	}, silentTestLogger())
	if bare.Display != thinkingDisplaySummarized {
		t.Errorf("Display (defaulted include_thoughts): got %q, want %q", bare.Display, thinkingDisplaySummarized)
	}
}

// TestParseThinkingConfig_DisplayNotInferredWithoutThoughts — nothing is read
// out of the thinking text, so the model's own default stands.
func TestParseThinkingConfig_DisplayNotInferredWithoutThoughts(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"mode": "adaptive", "include_thoughts": false},
	}, silentTestLogger())
	if tc.Display != thinkingDisplayUnset {
		t.Errorf("Display: got %q, want unset", tc.Display)
	}
}

// TestParseThinkingConfig_ExplicitDisplayWinsOverInference — in both
// directions. `omitted` alongside include_thoughts: true is a legitimate (if
// self-defeating) request and must survive.
func TestParseThinkingConfig_ExplicitDisplayWinsOverInference(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{
			"mode":             "adaptive",
			"include_thoughts": true,
			"display":          "omitted",
		},
	}, silentTestLogger())
	if tc.Display != thinkingDisplayOmitted {
		t.Errorf("Display: got %q, want %q (explicit must win)", tc.Display, thinkingDisplayOmitted)
	}

	other := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{
			"mode":             "adaptive",
			"include_thoughts": false,
			"display":          "summarized",
		},
	}, silentTestLogger())
	if other.Display != thinkingDisplaySummarized {
		t.Errorf("Display: got %q, want %q (explicit must win)", other.Display, thinkingDisplaySummarized)
	}
}

// TestParseThinkingConfig_DisplayNotInferredWhenNotThinking — there is no
// reasoning to display under disabled or off, so the inference stays out of it.
func TestParseThinkingConfig_DisplayNotInferredWhenNotThinking(t *testing.T) {
	for _, mode := range []thinkingMode{thinkingModeDisabled, thinkingModeOff} {
		tc := mustParseThinking(t, map[string]any{
			"thinking": map[string]any{"mode": string(mode), "include_thoughts": true},
		}, silentTestLogger())
		if tc.Display != thinkingDisplayUnset {
			t.Errorf("mode %q: Display got %q, want unset", mode, tc.Display)
		}
	}
}

// --- applyThinking ----------------------------------------------------------

// TestApplyThinking_Off writes no thinking key at all — the zero value and an
// explicit off behave identically.
func TestApplyThinking_Off(t *testing.T) {
	for _, cfg := range []thinkingConfig{{}, {Mode: thinkingModeOff}} {
		body := map[string]any{"max_tokens": 1024}
		applyThinking(body, cfg, silentTestLogger())
		if _, ok := body["thinking"]; ok {
			t.Fatalf("mode %q should not set thinking", cfg.Mode)
		}
		if len(body) != 1 {
			t.Fatalf("body mutated unexpectedly: %+v", body)
		}
	}
}

// TestApplyThinking_Adaptive produces {"type":"adaptive"} with no budget.
func TestApplyThinking_Adaptive(t *testing.T) {
	body := map[string]any{}
	applyThinking(body, thinkingConfig{Mode: thinkingModeAdaptive}, silentTestLogger())

	got, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking missing or wrong type: %T", body["thinking"])
	}
	if got["type"] != "adaptive" {
		t.Errorf("type: got %v, want adaptive", got["type"])
	}
	if _, ok := got["budget_tokens"]; ok {
		t.Errorf("adaptive must not carry budget_tokens: %+v", got)
	}
}

// TestApplyThinking_Disabled produces an explicit {"type":"disabled"}, which is
// a different request from omitting the field.
func TestApplyThinking_Disabled(t *testing.T) {
	body := map[string]any{}
	applyThinking(body, thinkingConfig{Mode: thinkingModeDisabled}, silentTestLogger())

	got, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking missing or wrong type: %T", body["thinking"])
	}
	if got["type"] != "disabled" {
		t.Errorf("type: got %v, want disabled", got["type"])
	}
}

// TestApplyThinking_Budget produces the legacy fixed-budget shape.
func TestApplyThinking_Budget(t *testing.T) {
	body := map[string]any{}
	applyThinking(body, thinkingConfig{Mode: thinkingModeBudget, BudgetTokens: 8192}, silentTestLogger())

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

// TestApplyThinking_DisplayRidesInsideThinking — display goes in the thinking
// object alongside type, not at the top level of the body.
func TestApplyThinking_DisplayRidesInsideThinking(t *testing.T) {
	cases := []thinkingConfig{
		{Mode: thinkingModeAdaptive, Display: thinkingDisplaySummarized},
		{Mode: thinkingModeBudget, BudgetTokens: 8192, Display: thinkingDisplayOmitted},
		{Mode: thinkingModeDisabled, Display: thinkingDisplayOmitted},
	}
	for _, cfg := range cases {
		body := map[string]any{}
		applyThinking(body, cfg, silentTestLogger())

		got, ok := body["thinking"].(map[string]any)
		if !ok {
			t.Fatalf("mode %q: thinking missing or wrong type: %T", cfg.Mode, body["thinking"])
		}
		if got["display"] != string(cfg.Display) {
			t.Errorf("mode %q: display got %v, want %q", cfg.Mode, got["display"], cfg.Display)
		}
		if _, ok := body["display"]; ok {
			t.Errorf("mode %q: display must not be a top-level body key: %+v", cfg.Mode, body)
		}
	}
}

// TestApplyThinking_DisplayUnsetOmitsKey — an unset display leaves the key off
// entirely so the target model's own default applies.
func TestApplyThinking_DisplayUnsetOmitsKey(t *testing.T) {
	for _, cfg := range []thinkingConfig{
		{Mode: thinkingModeAdaptive},
		{Mode: thinkingModeBudget, BudgetTokens: 8192},
		{Mode: thinkingModeDisabled},
	} {
		body := map[string]any{}
		applyThinking(body, cfg, silentTestLogger())

		got, _ := body["thinking"].(map[string]any)
		if _, ok := got["display"]; ok {
			t.Errorf("mode %q: unset display must not appear: %+v", cfg.Mode, got)
		}
	}
}

// TestApplyThinking_OffIgnoresDisplay — mode off has no thinking object to
// carry a display, so the whole field stays absent even when one is set.
func TestApplyThinking_OffIgnoresDisplay(t *testing.T) {
	body := map[string]any{"max_tokens": 1024}
	applyThinking(body, thinkingConfig{Mode: thinkingModeOff, Display: thinkingDisplaySummarized}, silentTestLogger())

	if _, ok := body["thinking"]; ok {
		t.Fatalf("mode off must not set thinking: %+v", body)
	}
	if len(body) != 1 {
		t.Fatalf("body mutated unexpectedly: %+v", body)
	}
}

// TestApplyThinking_StripsTemperature verifies non-1.0 temperature is removed
// and a warning is logged. Anthropic requires temp=1 (or unset) when thinking
// is enabled.
func TestApplyThinking_StripsTemperature(t *testing.T) {
	var buf bytes.Buffer
	body := map[string]any{"temperature": 0.7}
	applyThinking(body, thinkingConfig{Mode: thinkingModeAdaptive}, warnCaptureLogger(&buf))

	if _, ok := body["temperature"]; ok {
		t.Fatal("temperature should have been stripped")
	}
	if !strings.Contains(buf.String(), "stripping temperature") {
		t.Errorf("expected warning log, got: %q", buf.String())
	}
}

// TestApplyThinking_DisabledKeepsTemperature — thinking is off in this mode, so
// the temp=1 constraint does not apply and the caller's value survives.
func TestApplyThinking_DisabledKeepsTemperature(t *testing.T) {
	body := map[string]any{"temperature": 0.7}
	applyThinking(body, thinkingConfig{Mode: thinkingModeDisabled}, silentTestLogger())

	if body["temperature"] != 0.7 {
		t.Fatalf("temperature should have survived: %+v", body)
	}
}

// TestApplyThinking_TemperatureOneNoWarn — temp=1.0 is silently dropped (it's
// the value Anthropic would have used anyway), no warning emitted.
func TestApplyThinking_TemperatureOneNoWarn(t *testing.T) {
	var buf bytes.Buffer
	body := map[string]any{"temperature": 1.0}
	applyThinking(body, thinkingConfig{Mode: thinkingModeBudget, BudgetTokens: 4096}, warnCaptureLogger(&buf))

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
	body := map[string]any{"max_tokens": 4096}
	applyThinking(body, thinkingConfig{Mode: thinkingModeAdaptive}, warnCaptureLogger(&buf))

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
		thinking: thinkingConfig{Mode: thinkingModeBudget, BudgetTokens: 8192, IncludeThoughts: true},
		pricing:  pricing.DefaultsFor(pricing.ProviderAnthropic),
	}

	p.handleStreamResponse(strings.NewReader(canonicalThinkingStream), "test-req", nil, nil)

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
		thinking: thinkingConfig{Mode: thinkingModeBudget, BudgetTokens: 8192, IncludeThoughts: false},
		pricing:  pricing.DefaultsFor(pricing.ProviderAnthropic),
	}

	p.handleStreamResponse(strings.NewReader(canonicalThinkingStream), "test-req", nil, nil)

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
