package client

import (
	"os"
	"testing"
)

func TestParseConfig_DefaultsApplyToServer(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"defaults": map[string]any{
			"timeout":         "10s",
			"lifecycle":       "session",
			"command_prefix":  "x",
			"resources":       map[string]any{"auto_register_max": 7},
		},
		"servers": []any{
			map[string]any{
				"name":      "fs",
				"transport": "stdio",
				"command":   "echo",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.CommandPrefix != "x" {
		t.Errorf("command_prefix: %q", cfg.Defaults.CommandPrefix)
	}
	if cfg.Servers[0].Lifecycle != "session" {
		t.Errorf("server lifecycle should inherit defaults: %q", cfg.Servers[0].Lifecycle)
	}
	if cfg.Servers[0].Timeout.Seconds() != 10 {
		t.Errorf("server timeout should inherit defaults: %v", cfg.Servers[0].Timeout)
	}
	if cfg.Servers[0].Resources.AutoRegisterMax != 7 {
		t.Errorf("resources.auto_register_max should inherit defaults: %d", cfg.Servers[0].Resources.AutoRegisterMax)
	}
}

func TestParseConfig_InlineOverridesDefaults(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"defaults": map[string]any{"lifecycle": "engine"},
		"servers": []any{
			map[string]any{
				"name":      "fs",
				"transport": "stdio",
				"command":   "echo",
				"lifecycle": "session",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers[0].Lifecycle != "session" {
		t.Fatalf("inline lifecycle should win: %q", cfg.Servers[0].Lifecycle)
	}
}

func TestParseConfig_RejectsBadTransport(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"servers": []any{
			map[string]any{"name": "x", "transport": "telepathy", "command": "n"},
		},
	})
	if err == nil {
		t.Fatal("expected error for bogus transport")
	}
}

func TestParseConfig_RejectsDuplicateServers(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"servers": []any{
			map[string]any{"name": "fs", "transport": "stdio", "command": "echo"},
			map[string]any{"name": "fs", "transport": "stdio", "command": "ls"},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestParseConfig_ExpandsEnvInValues(t *testing.T) {
	t.Setenv("NEXUS_TEST_MCP_TOKEN", "secret123")
	cfg, err := parseConfig(map[string]any{
		"servers": []any{
			map[string]any{
				"name":      "gh",
				"transport": "http",
				"url":       "http://x",
				"headers": map[string]any{
					"Authorization": "Bearer ${NEXUS_TEST_MCP_TOKEN}",
				},
				"env": map[string]any{
					"TOKEN": "${NEXUS_TEST_MCP_TOKEN}",
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers[0].Headers["Authorization"] != "Bearer secret123" {
		t.Fatalf("header not expanded: %q", cfg.Servers[0].Headers["Authorization"])
	}
	if cfg.Servers[0].Env["TOKEN"] != "secret123" {
		t.Fatalf("env not expanded: %q", cfg.Servers[0].Env["TOKEN"])
	}
}

func TestParseConfig_StdioRequiresCommand(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"servers": []any{map[string]any{"name": "x", "transport": "stdio"}},
	})
	if err == nil {
		t.Fatal("expected stdio-requires-command error")
	}
}

func TestParseConfig_HTTPRequiresURL(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"servers": []any{map[string]any{"name": "x", "transport": "http"}},
	})
	if err == nil {
		t.Fatal("expected http-requires-url error")
	}
}

func TestEnvSlice_IncludesPassthrough(t *testing.T) {
	t.Setenv("NEXUS_MCP_PASS", "yes")
	got := envSlice(ServerConfig{
		Env:            map[string]string{"A": "1"},
		EnvPassthrough: []string{"NEXUS_MCP_PASS", "NONEXISTENT_NEVER_SET_VAR_QQ"},
	})
	var sawA, sawPass bool
	for _, v := range got {
		if v == "A=1" {
			sawA = true
		}
		if v == "NEXUS_MCP_PASS=yes" {
			sawPass = true
		}
	}
	if !sawA || !sawPass {
		t.Fatalf("got %v", got)
	}
}

func TestToolFilter_AllowAndDeny(t *testing.T) {
	tt := []struct {
		name    string
		filter  ToolFilter
		input   string
		allowed bool
	}{
		{"empty", ToolFilter{}, "x", true},
		{"allow-hit", ToolFilter{Allow: []string{"x"}}, "x", true},
		{"allow-miss", ToolFilter{Allow: []string{"y"}}, "x", false},
		{"deny", ToolFilter{Deny: []string{"x"}}, "x", false},
		{"deny-wins", ToolFilter{Allow: []string{"x"}, Deny: []string{"x"}}, "x", false},
	}
	for _, c := range tt {
		t.Run(c.name, func(t *testing.T) {
			if c.filter.allowed(c.input) != c.allowed {
				t.Fatalf("filter %v on %q: want %v", c.filter, c.input, c.allowed)
			}
		})
	}
}

func TestMain(m *testing.M) {
	// Ensure deterministic env for env-expansion tests.
	_ = os.Unsetenv("NEXUS_TEST_MCP_TOKEN")
	os.Exit(m.Run())
}
