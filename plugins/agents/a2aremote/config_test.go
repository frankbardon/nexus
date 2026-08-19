package a2aremote

import (
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"agents": []any{
			map[string]any{"name": "Deep Research", "base_url": "https://research.internal"},
		},
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if !cfg.cacheEnabled {
		t.Error("cache should default to enabled")
	}
	if cfg.cacheSize != defaultCacheSize {
		t.Errorf("cache_size = %d, want %d", cfg.cacheSize, defaultCacheSize)
	}
	if cfg.maxDepth != defaultMaxDepth {
		t.Errorf("max_depth = %d, want %d", cfg.maxDepth, defaultMaxDepth)
	}
	if len(cfg.agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(cfg.agents))
	}
	ac := cfg.agents[0]
	if ac.toolName != "delegate_a2a_deep_research" {
		t.Errorf("tool name = %q, want delegate_a2a_deep_research", ac.toolName)
	}
	if !ac.transport.streaming() {
		t.Error("streaming should default to on")
	}
	if ac.transport.callTimeout() != 0 {
		t.Errorf("call timeout should be unset by default, got %s", ac.transport.callTimeout())
	}
}

func TestParseConfigAgentInheritsPluginDefaults(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"binding":             "http+json",
		"stream_idle_timeout": "20m",
		"stream":              false,
		"retry":               map[string]any{"max_attempts": 5},
		"agents": []any{
			map[string]any{"name": "inherits", "base_url": "https://a.internal"},
			map[string]any{
				"name":                "overrides",
				"base_url":            "https://b.internal",
				"stream_idle_timeout": "30s",
				"stream":              true,
			},
		},
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	inherits, overrides := cfg.agents[0], cfg.agents[1]

	if got := *inherits.transport.binding; got != a2a.BindingHTTPJSON {
		t.Errorf("inherited binding = %q, want %q", got, a2a.BindingHTTPJSON)
	}
	if got := *inherits.transport.streamIdleTimeout; got != 20*time.Minute {
		t.Errorf("inherited stream_idle_timeout = %s, want 20m", got)
	}
	if inherits.transport.streaming() {
		t.Error("inherited stream should be false")
	}
	if got := inherits.transport.retry.MaxAttempts; got != 5 {
		t.Errorf("inherited retry.max_attempts = %d, want 5", got)
	}

	if got := *overrides.transport.streamIdleTimeout; got != 30*time.Second {
		t.Errorf("overridden stream_idle_timeout = %s, want 30s", got)
	}
	if !overrides.transport.streaming() {
		t.Error("overridden stream should be true")
	}
	if got := *overrides.transport.binding; got != a2a.BindingHTTPJSON {
		t.Errorf("override should still inherit binding, got %q", got)
	}
}

func TestParseConfigRejections(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{
			name: "no agents",
			raw:  map[string]any{},
			want: "non-empty list",
		},
		{
			name: "empty agents",
			raw:  map[string]any{"agents": []any{}},
			want: "non-empty list",
		},
		{
			name: "agent without name",
			raw:  map[string]any{"agents": []any{map[string]any{"base_url": "https://x"}}},
			want: "name is required",
		},
		{
			name: "agent without any url",
			raw:  map[string]any{"agents": []any{map[string]any{"name": "x"}}},
			want: "base_url is required",
		},
		{
			name: "duplicate tool names",
			raw: map[string]any{"agents": []any{
				map[string]any{"name": "one two", "base_url": "https://a"},
				map[string]any{"name": "one-two", "base_url": "https://b"},
			}},
			want: "already taken",
		},
		{
			name: "unknown binding",
			raw: map[string]any{
				"binding": "grpc",
				"agents":  []any{map[string]any{"name": "x", "base_url": "https://a"}},
			},
			want: "unknown binding",
		},
		{
			name: "bare number duration",
			raw: map[string]any{
				"timeout": 600,
				"agents":  []any{map[string]any{"name": "x", "base_url": "https://a"}},
			},
			want: "want a duration string",
		},
		{
			name: "negative duration",
			raw: map[string]any{
				"timeout": "-5s",
				"agents":  []any{map[string]any{"name": "x", "base_url": "https://a"}},
			},
			want: "must not be negative",
		},
		{
			name: "zero retry attempts",
			raw: map[string]any{
				"retry":  map[string]any{"max_attempts": 0},
				"agents": []any{map[string]any{"name": "x", "base_url": "https://a"}},
			},
			want: "at least 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig(tc.raw)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseConfigZeroDurationIsMeaningful(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"agents": []any{map[string]any{
			"name":                "x",
			"base_url":            "https://a",
			"stream_idle_timeout": "0s",
		}},
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	idle := cfg.agents[0].transport.streamIdleTimeout
	if idle == nil {
		t.Fatal("an explicit 0s must survive as a set value, not as unset")
	}
	if *idle != 0 {
		t.Errorf("stream_idle_timeout = %s, want 0", *idle)
	}
}

func TestSanitizeToolSuffix(t *testing.T) {
	tests := map[string]string{
		"Deep Research": "deep_research",
		"legal":         "legal",
		"a-b_c":         "a_b_c",
		"  spaced  ":    "spaced",
		"???":           "",
	}
	for in, want := range tests {
		if got := sanitizeToolSuffix(in); got != want {
			t.Errorf("sanitizeToolSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}
