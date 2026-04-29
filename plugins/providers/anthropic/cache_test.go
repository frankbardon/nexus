package anthropic

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// silentLogger returns a slog.Logger that discards all output (we don't
// inspect logs in these tests beyond the cap-event smoke check below).
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// captureLogger returns a logger that writes structured records into the
// caller-provided buffer so tests can assert that a specific debug message
// fired (used to verify the 4-breakpoint cap warning).
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestParseCacheConfig_Defaults verifies that a `cache: {enabled: true}` block
// fills in the high-leverage defaults (system + tools cached, 5m TTL, no
// message prefix marking).
func TestParseCacheConfig_Defaults(t *testing.T) {
	cfg := map[string]any{
		"cache": map[string]any{
			"enabled": true,
		},
	}

	cc := parseCacheConfig(cfg)

	if !cc.Enabled {
		t.Fatal("Enabled: got false, want true")
	}
	if !cc.System {
		t.Errorf("System: got false, want true (default)")
	}
	if !cc.Tools {
		t.Errorf("Tools: got false, want true (default)")
	}
	if cc.MessagePrefix != 0 {
		t.Errorf("MessagePrefix: got %d, want 0 (default)", cc.MessagePrefix)
	}
	if cc.TTL != "5m" {
		t.Errorf("TTL: got %q, want %q (default)", cc.TTL, "5m")
	}
}

// TestParseCacheConfig_Disabled verifies that an absent or disabled cache
// block produces a zero-value config (so applyCacheControl is a no-op).
func TestParseCacheConfig_Disabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]any
	}{
		{"absent", map[string]any{}},
		{"explicit-false", map[string]any{"cache": map[string]any{"enabled": false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := parseCacheConfig(tc.cfg)
			if cc.Enabled {
				t.Errorf("Enabled: got true, want false")
			}
		})
	}
}

// TestParseCacheConfig_Explicit verifies that explicit field values override
// the enabled-defaults.
func TestParseCacheConfig_Explicit(t *testing.T) {
	cfg := map[string]any{
		"cache": map[string]any{
			"enabled":        true,
			"system":         false,
			"tools":          true,
			"message_prefix": 2,
			"ttl":            "1h",
		},
	}

	cc := parseCacheConfig(cfg)

	if cc.System {
		t.Errorf("System: got true, want false")
	}
	if !cc.Tools {
		t.Errorf("Tools: got false, want true")
	}
	if cc.MessagePrefix != 2 {
		t.Errorf("MessagePrefix: got %d, want 2", cc.MessagePrefix)
	}
	if cc.TTL != "1h" {
		t.Errorf("TTL: got %q, want %q", cc.TTL, "1h")
	}
}

// TestParseCacheConfig_MessagePrefixFloat verifies that a float64-encoded
// message_prefix (e.g. JSON-decoded YAML on some paths) is accepted.
func TestParseCacheConfig_MessagePrefixFloat(t *testing.T) {
	cfg := map[string]any{
		"cache": map[string]any{
			"enabled":        true,
			"message_prefix": float64(3),
		},
	}

	cc := parseCacheConfig(cfg)
	if cc.MessagePrefix != 3 {
		t.Errorf("MessagePrefix: got %d, want 3", cc.MessagePrefix)
	}
}

// TestParseCacheConfig_InvalidTTL verifies that an unsupported TTL string
// falls back to "5m" rather than erroring.
func TestParseCacheConfig_InvalidTTL(t *testing.T) {
	cfg := map[string]any{
		"cache": map[string]any{
			"enabled": true,
			"ttl":     "30m", // not in {5m, 1h}
		},
	}

	cc := parseCacheConfig(cfg)
	if cc.TTL != "5m" {
		t.Errorf("TTL: got %q, want %q (fallback)", cc.TTL, "5m")
	}
}

// TestApplyCacheControl_Disabled verifies that when caching is disabled the
// body is left untouched (system stays a string, tools stay clean).
func TestApplyCacheControl_Disabled(t *testing.T) {
	body := map[string]any{
		"system": "you are helpful",
		"tools": []map[string]any{
			{"name": "shell", "description": "run a command"},
		},
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	}

	applyCacheControl(body, cacheConfig{Enabled: false}, silentLogger())

	if _, ok := body["system"].(string); !ok {
		t.Errorf("system: expected unchanged string, got %T", body["system"])
	}
	tools := body["tools"].([]map[string]any)
	if _, ok := tools[0]["cache_control"]; ok {
		t.Errorf("tools[0]: expected no cache_control marker, got one")
	}
}

// TestApplyCacheControl_SystemOnly verifies that with cache enabled but only
// the system breakpoint requested, only `system` is mutated (string -> array
// with one cache_control text block) and tools remain untouched.
func TestApplyCacheControl_SystemOnly(t *testing.T) {
	body := map[string]any{
		"system": "you are helpful",
		"tools": []map[string]any{
			{"name": "shell"},
		},
	}
	cfg := cacheConfig{Enabled: true, System: true, Tools: false, TTL: "5m"}

	applyCacheControl(body, cfg, silentLogger())

	sys, ok := body["system"].([]map[string]any)
	if !ok {
		t.Fatalf("system: expected []map[string]any, got %T", body["system"])
	}
	if len(sys) != 1 {
		t.Fatalf("system: got %d blocks, want 1", len(sys))
	}
	if sys[0]["type"] != "text" {
		t.Errorf("system[0].type: got %v, want text", sys[0]["type"])
	}
	if sys[0]["text"] != "you are helpful" {
		t.Errorf("system[0].text: got %v", sys[0]["text"])
	}
	cc, ok := sys[0]["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("system[0].cache_control: missing or wrong type")
	}
	if cc["type"] != "ephemeral" {
		t.Errorf("cache_control.type: got %v, want ephemeral", cc["type"])
	}
	if _, hasTTL := cc["ttl"]; hasTTL {
		t.Errorf("cache_control.ttl: should be absent for 5m TTL")
	}

	tools := body["tools"].([]map[string]any)
	if _, ok := tools[0]["cache_control"]; ok {
		t.Errorf("tools[0]: expected no cache_control, got one")
	}
}

// TestApplyCacheControl_ToolsOnly verifies that with only the tools
// breakpoint requested, the LAST tool definition gets a cache_control marker
// and earlier tools remain untouched.
func TestApplyCacheControl_ToolsOnly(t *testing.T) {
	body := map[string]any{
		"system": "you are helpful",
		"tools": []map[string]any{
			{"name": "shell"},
			{"name": "fileio"},
			{"name": "search"},
		},
	}
	cfg := cacheConfig{Enabled: true, System: false, Tools: true, TTL: "5m"}

	applyCacheControl(body, cfg, silentLogger())

	if _, ok := body["system"].(string); !ok {
		t.Errorf("system: expected unchanged string when System=false, got %T", body["system"])
	}

	tools := body["tools"].([]map[string]any)
	if len(tools) != 3 {
		t.Fatalf("tools length changed: got %d, want 3", len(tools))
	}
	if _, ok := tools[0]["cache_control"]; ok {
		t.Errorf("tools[0]: expected no marker, got one")
	}
	if _, ok := tools[1]["cache_control"]; ok {
		t.Errorf("tools[1]: expected no marker, got one")
	}
	cc, ok := tools[2]["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("tools[2].cache_control: missing")
	}
	if cc["type"] != "ephemeral" {
		t.Errorf("tools[2].cache_control.type: got %v, want ephemeral", cc["type"])
	}
}

// TestApplyCacheControl_FourBreakpoints verifies that requesting system +
// tools + 2 message markers produces all 4 breakpoints (the API limit) with
// no cap warning.
func TestApplyCacheControl_FourBreakpoints(t *testing.T) {
	body := map[string]any{
		"system": "you are helpful",
		"tools": []map[string]any{
			{"name": "shell"},
		},
		"messages": []map[string]any{
			{"role": "user", "content": "msg one"},
			{"role": "user", "content": "msg two"},
			{"role": "assistant", "content": "ok"},
		},
	}
	cfg := cacheConfig{Enabled: true, System: true, Tools: true, MessagePrefix: 2, TTL: "5m"}

	buf := new(bytes.Buffer)
	applyCacheControl(body, cfg, captureLogger(buf))

	if strings.Contains(buf.String(), "capped at 4-breakpoint") {
		t.Errorf("did not expect cap log, got: %s", buf.String())
	}

	// system marked
	if _, ok := body["system"].([]map[string]any); !ok {
		t.Errorf("system: not converted to array form")
	}
	// last tool marked
	tools := body["tools"].([]map[string]any)
	if _, ok := tools[0]["cache_control"]; !ok {
		t.Errorf("tools[0]: missing cache_control")
	}
	// first two user msgs marked, assistant untouched
	msgs := body["messages"].([]map[string]any)
	for i := 0; i < 2; i++ {
		blocks, ok := msgs[i]["content"].([]map[string]any)
		if !ok {
			t.Errorf("msg[%d]: content not converted to array form: %T", i, msgs[i]["content"])
			continue
		}
		if _, ok := blocks[0]["cache_control"].(map[string]any); !ok {
			t.Errorf("msg[%d].content[0]: missing cache_control", i)
		}
	}
	// assistant must remain string-form, no marker
	if _, ok := msgs[2]["content"].(string); !ok {
		t.Errorf("msg[2] (assistant): content mutated, got %T", msgs[2]["content"])
	}
}

// TestApplyCacheControl_CapsAtFour verifies that requesting 5 message markers
// (alongside system + tools = 2 already used) is silently capped at 2 (the
// remaining budget) and emits a debug log.
func TestApplyCacheControl_CapsAtFour(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": "msg 0"},
		{"role": "user", "content": "msg 1"},
		{"role": "user", "content": "msg 2"},
		{"role": "user", "content": "msg 3"},
		{"role": "user", "content": "msg 4"},
	}
	body := map[string]any{
		"system":   "sys",
		"tools":    []map[string]any{{"name": "shell"}},
		"messages": msgs,
	}
	cfg := cacheConfig{Enabled: true, System: true, Tools: true, MessagePrefix: 5, TTL: "5m"}

	buf := new(bytes.Buffer)
	applyCacheControl(body, cfg, captureLogger(buf))

	if !strings.Contains(buf.String(), "capped at 4-breakpoint") {
		t.Errorf("expected cap log, got: %s", buf.String())
	}

	// System (1) + tools (1) used 2 of 4 breakpoints, leaving 2 for messages.
	// User asked for 5 — drop the OLDEST 3, keep the LATEST 2 (msg[3], msg[4]).
	// Later markers cover strictly larger cacheable prefixes, so keeping the
	// tail is what the plan calls for.
	mutated := body["messages"].([]map[string]any)
	markedCount := 0
	for i, m := range mutated {
		blocks, ok := m["content"].([]map[string]any)
		if !ok {
			continue
		}
		_, has := blocks[0]["cache_control"]
		if !has {
			continue
		}
		markedCount++
		if i < 3 {
			t.Errorf("msg[%d]: should not have been marked (oldest dropped first)", i)
		}
	}
	if markedCount != 2 {
		t.Errorf("marked count: got %d, want 2 (budget cap)", markedCount)
	}
}

// TestApplyCacheControl_StopsAtAssistant verifies that the message-marker
// pass stops scanning at the first non-user turn (assistant interleavings
// break the cacheable prefix invariant).
func TestApplyCacheControl_StopsAtAssistant(t *testing.T) {
	body := map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "u0"},
			{"role": "assistant", "content": "a0"},
			{"role": "user", "content": "u1"}, // must not be marked
		},
	}
	cfg := cacheConfig{Enabled: true, MessagePrefix: 3, TTL: "5m"}

	applyCacheControl(body, cfg, silentLogger())

	msgs := body["messages"].([]map[string]any)
	// u0 marked
	if blocks, ok := msgs[0]["content"].([]map[string]any); !ok {
		t.Errorf("msg[0]: expected array form")
	} else if _, has := blocks[0]["cache_control"]; !has {
		t.Errorf("msg[0]: missing cache_control")
	}
	// u1 must remain string-form
	if _, ok := msgs[2]["content"].(string); !ok {
		t.Errorf("msg[2]: content mutated past assistant boundary, got %T", msgs[2]["content"])
	}
}

// TestApplyCacheControl_StopsAtToolResult verifies that user messages whose
// content is a tool_result envelope aren't marked (tool results change every
// iteration, so they can't form a stable cacheable prefix).
func TestApplyCacheControl_StopsAtToolResult(t *testing.T) {
	body := map[string]any{
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "abc", "content": "ok"},
				},
			},
		},
	}
	cfg := cacheConfig{Enabled: true, MessagePrefix: 1, TTL: "5m"}

	applyCacheControl(body, cfg, silentLogger())

	blocks := body["messages"].([]map[string]any)[0]["content"].([]map[string]any)
	if _, has := blocks[0]["cache_control"]; has {
		t.Errorf("tool_result block: should not be marked")
	}
}

// TestApplyCacheControl_OneHourTTL verifies that 1h TTL emits a cache_control
// block carrying ttl:"1h" (not the bare ephemeral form).
func TestApplyCacheControl_OneHourTTL(t *testing.T) {
	body := map[string]any{
		"system": "sys",
	}
	cfg := cacheConfig{Enabled: true, System: true, TTL: "1h"}

	applyCacheControl(body, cfg, silentLogger())

	sys := body["system"].([]map[string]any)
	cc := sys[0]["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" {
		t.Errorf("cache_control.type: got %v, want ephemeral", cc["type"])
	}
	if cc["ttl"] != "1h" {
		t.Errorf("cache_control.ttl: got %v, want 1h", cc["ttl"])
	}
}

// TestApplyCacheControl_NoSystemNoTools verifies that with neither system nor
// tools enabled, all 4 breakpoints are available for messages.
func TestApplyCacheControl_NoSystemNoTools(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": "u0"},
		{"role": "user", "content": "u1"},
		{"role": "user", "content": "u2"},
		{"role": "user", "content": "u3"},
		{"role": "user", "content": "u4"},
	}
	body := map[string]any{"messages": msgs}
	cfg := cacheConfig{Enabled: true, System: false, Tools: false, MessagePrefix: 5, TTL: "5m"}

	buf := new(bytes.Buffer)
	applyCacheControl(body, cfg, captureLogger(buf))

	if !strings.Contains(buf.String(), "capped at 4-breakpoint") {
		t.Errorf("expected cap log when MessagePrefix exceeds 4, got: %s", buf.String())
	}

	mutated := body["messages"].([]map[string]any)
	marked := 0
	for i, m := range mutated {
		blocks, ok := m["content"].([]map[string]any)
		if !ok {
			continue
		}
		_, has := blocks[0]["cache_control"]
		if !has {
			continue
		}
		marked++
		if i == 0 {
			t.Errorf("msg[0]: should not have been marked (oldest dropped, budget=4 of 5)")
		}
	}
	if marked != 4 {
		t.Errorf("marked count: got %d, want 4 (full budget)", marked)
	}
}

// TestBuildRequestBody_WithCache integrates parseCacheConfig +
// buildRequestBody to verify the on-the-wire JSON Anthropic actually receives
// when caching is enabled. Acts as the closest-to-end-to-end check without an
// HTTP fixture.
func TestBuildRequestBody_WithCache(t *testing.T) {
	p := &Plugin{
		cache:  cacheConfig{Enabled: true, System: true, Tools: true, TTL: "1h"},
		logger: silentLogger(),
	}

	req := events.LLMRequest{
		Model:     "claude-sonnet-4-6-20250514",
		MaxTokens: 1024,
		Messages: []events.Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: "hi"},
		},
		Tools: []events.ToolDef{
			{Name: "shell", Description: "run a command", Parameters: map[string]any{}},
			{Name: "fileio", Description: "read or write a file", Parameters: map[string]any{}},
		},
	}

	body := p.buildRequestBody(req.Model, req.MaxTokens, req)

	// system is now array form with cache_control
	sys, ok := body["system"].([]map[string]any)
	if !ok {
		t.Fatalf("system: got %T, want []map[string]any", body["system"])
	}
	if cc, ok := sys[0]["cache_control"].(map[string]any); !ok {
		t.Errorf("system[0].cache_control: missing")
	} else if cc["ttl"] != "1h" {
		t.Errorf("system[0].cache_control.ttl: got %v, want 1h", cc["ttl"])
	}

	// last tool has cache_control
	tools := body["tools"].([]map[string]any)
	if _, ok := tools[len(tools)-1]["cache_control"]; !ok {
		t.Errorf("last tool: missing cache_control")
	}
	// first tool does NOT
	if _, ok := tools[0]["cache_control"]; ok {
		t.Errorf("first tool: should not be marked")
	}

	// JSON roundtrip should succeed (catch any non-marshalable values).
	if _, err := json.Marshal(body); err != nil {
		t.Fatalf("json.Marshal(body): %v", err)
	}
}

// TestBuildRequestBody_CacheDisabled verifies the body shape is untouched
// when caching is off — system stays string-form, tools have no markers.
func TestBuildRequestBody_CacheDisabled(t *testing.T) {
	p := &Plugin{
		cache:  cacheConfig{Enabled: false},
		logger: silentLogger(),
	}

	req := events.LLMRequest{
		Model:     "claude-sonnet-4-6-20250514",
		MaxTokens: 1024,
		Messages: []events.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		},
		Tools: []events.ToolDef{
			{Name: "shell", Parameters: map[string]any{}},
		},
	}

	body := p.buildRequestBody(req.Model, req.MaxTokens, req)

	if _, ok := body["system"].(string); !ok {
		t.Errorf("system: expected string, got %T", body["system"])
	}
	tools := body["tools"].([]map[string]any)
	if _, ok := tools[0]["cache_control"]; ok {
		t.Errorf("tools[0]: should not be marked when cache disabled")
	}
}
