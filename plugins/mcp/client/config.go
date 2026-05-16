package client

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Config is the parsed plugin configuration.
type Config struct {
	Servers  []ServerConfig
	Defaults Defaults
	Aliases  map[string]string // alias name -> "<server>.<prompt>"
}

// ServerConfig describes one MCP server connection.
type ServerConfig struct {
	Name      string
	Transport string // "stdio" | "http"

	// stdio
	Command        string
	Args           []string
	Env            map[string]string
	EnvPassthrough []string

	// http (streamable HTTP transport)
	URL     string
	Headers map[string]string

	Lifecycle string        // "engine" | "session"
	Timeout   time.Duration // per-request timeout

	Tools     ToolFilter
	Resources ResourceConfig
	Prompts   PromptConfig
}

// ToolFilter is the per-server tool allowlist/blocklist applied to MCP tools
// before they reach the catalog. Matched against the raw MCP tool name, not
// the Nexus-namespaced name.
type ToolFilter struct {
	Allow []string
	Deny  []string
}

// ResourceConfig governs how MCP resources are surfaced into the catalog and
// onto the bus.
type ResourceConfig struct {
	Enabled              bool
	AutoRegisterStatic   bool
	AutoRegisterMax      int
	AutoRegisterTemplate bool
	SubscribeUpdates     bool
}

// PromptConfig governs the slash-command surface for MCP prompts.
type PromptConfig struct {
	Enabled bool
}

// Defaults applied to every ServerConfig unless overridden inline.
type Defaults struct {
	Lifecycle     string
	Timeout       time.Duration
	Resources     ResourceConfig
	Prompts       PromptConfig
	CommandPrefix string // slash prefix; default "mcp"
}

// parseConfig converts the raw YAML map into a typed Config. ${ENV_VAR}
// substitutions are expanded for env values and HTTP header values so
// developer configs can reference secrets without inlining them.
func parseConfig(raw map[string]any) (Config, error) {
	cfg := Config{
		Defaults: Defaults{
			Lifecycle:     "engine",
			Timeout:       30 * time.Second,
			CommandPrefix: "mcp",
			Resources: ResourceConfig{
				Enabled:              true,
				AutoRegisterStatic:   true,
				AutoRegisterMax:      50,
				AutoRegisterTemplate: true,
				SubscribeUpdates:     true,
			},
			Prompts: PromptConfig{Enabled: true},
		},
		Aliases: map[string]string{},
	}

	if d, ok := raw["defaults"].(map[string]any); ok {
		if v, ok := d["lifecycle"].(string); ok && v != "" {
			cfg.Defaults.Lifecycle = v
		}
		if v, ok := d["timeout"].(string); ok && v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return cfg, fmt.Errorf("defaults.timeout %q: %w", v, err)
			}
			cfg.Defaults.Timeout = parsed
		}
		if v, ok := d["command_prefix"].(string); ok && v != "" {
			cfg.Defaults.CommandPrefix = v
		}
		if r, ok := d["resources"].(map[string]any); ok {
			cfg.Defaults.Resources = applyResourceConfig(cfg.Defaults.Resources, r)
		}
		if pp, ok := d["prompts"].(map[string]any); ok {
			if v, ok := pp["enabled"].(bool); ok {
				cfg.Defaults.Prompts.Enabled = v
			}
		}
	}

	if al, ok := raw["aliases"].(map[string]any); ok {
		for k, v := range al {
			if s, ok := v.(string); ok && s != "" {
				cfg.Aliases[k] = s
			}
		}
	}

	rawServers, _ := raw["servers"].([]any)
	for i, s := range rawServers {
		m, ok := s.(map[string]any)
		if !ok {
			return cfg, fmt.Errorf("servers[%d]: expected map", i)
		}
		sc, err := parseServer(m, cfg.Defaults)
		if err != nil {
			return cfg, fmt.Errorf("servers[%d]: %w", i, err)
		}
		cfg.Servers = append(cfg.Servers, sc)
	}

	for i, sc := range cfg.Servers {
		for j := i + 1; j < len(cfg.Servers); j++ {
			if cfg.Servers[j].Name == sc.Name {
				return cfg, fmt.Errorf("duplicate server name %q", sc.Name)
			}
		}
	}

	return cfg, nil
}

func parseServer(m map[string]any, defaults Defaults) (ServerConfig, error) {
	sc := ServerConfig{
		Lifecycle: defaults.Lifecycle,
		Timeout:   defaults.Timeout,
		Resources: defaults.Resources,
		Prompts:   defaults.Prompts,
	}

	name, _ := m["name"].(string)
	if name == "" {
		return sc, fmt.Errorf("name is required")
	}
	if !serverNameRE.MatchString(name) {
		return sc, fmt.Errorf("name %q must match %s", name, serverNameRE.String())
	}
	sc.Name = name

	transport, _ := m["transport"].(string)
	if transport == "" {
		transport = "stdio"
	}
	switch transport {
	case "stdio", "http":
		sc.Transport = transport
	default:
		return sc, fmt.Errorf("transport %q must be stdio or http", transport)
	}

	if v, ok := m["lifecycle"].(string); ok && v != "" {
		switch v {
		case "engine", "session":
			sc.Lifecycle = v
		default:
			return sc, fmt.Errorf("lifecycle %q must be engine or session", v)
		}
	}

	if v, ok := m["timeout"].(string); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return sc, fmt.Errorf("timeout %q: %w", v, err)
		}
		sc.Timeout = d
	}

	sc.Command, _ = m["command"].(string)
	if rawArgs, ok := m["args"].([]any); ok {
		for _, a := range rawArgs {
			if s, ok := a.(string); ok {
				sc.Args = append(sc.Args, s)
			}
		}
	}
	if rawEnv, ok := m["env"].(map[string]any); ok {
		sc.Env = map[string]string{}
		for k, v := range rawEnv {
			s, _ := v.(string)
			sc.Env[k] = expandEnv(s)
		}
	}
	if rawPT, ok := m["env_passthrough"].([]any); ok {
		for _, a := range rawPT {
			if s, ok := a.(string); ok {
				sc.EnvPassthrough = append(sc.EnvPassthrough, s)
			}
		}
	}

	sc.URL, _ = m["url"].(string)
	if rawHeaders, ok := m["headers"].(map[string]any); ok {
		sc.Headers = map[string]string{}
		for k, v := range rawHeaders {
			s, _ := v.(string)
			sc.Headers[k] = expandEnv(s)
		}
	}

	switch sc.Transport {
	case "stdio":
		if sc.Command == "" {
			return sc, fmt.Errorf("stdio transport requires command")
		}
	case "http":
		if sc.URL == "" {
			return sc, fmt.Errorf("http transport requires url")
		}
	}

	if rawTools, ok := m["tools"].(map[string]any); ok {
		sc.Tools = parseToolFilter(rawTools)
	}
	if rawRes, ok := m["resources"].(map[string]any); ok {
		sc.Resources = applyResourceConfig(sc.Resources, rawRes)
	}
	if rawPrompts, ok := m["prompts"].(map[string]any); ok {
		if v, ok := rawPrompts["enabled"].(bool); ok {
			sc.Prompts.Enabled = v
		}
	}

	return sc, nil
}

func parseToolFilter(m map[string]any) ToolFilter {
	var f ToolFilter
	if v, ok := m["allow"].([]any); ok {
		for _, item := range v {
			if s, ok := item.(string); ok {
				f.Allow = append(f.Allow, s)
			}
		}
	}
	if v, ok := m["deny"].([]any); ok {
		for _, item := range v {
			if s, ok := item.(string); ok {
				f.Deny = append(f.Deny, s)
			}
		}
	}
	return f
}

func applyResourceConfig(base ResourceConfig, raw map[string]any) ResourceConfig {
	out := base
	if v, ok := raw["enabled"].(bool); ok {
		out.Enabled = v
	}
	if v, ok := raw["auto_register_static"].(bool); ok {
		out.AutoRegisterStatic = v
	}
	if v, ok := raw["auto_register_template"].(bool); ok {
		out.AutoRegisterTemplate = v
	}
	if v, ok := raw["auto_register_max"]; ok {
		if n, ok := intLike(v); ok && n >= 0 {
			out.AutoRegisterMax = n
		}
	}
	if v, ok := raw["subscribe_updates"].(bool); ok {
		out.SubscribeUpdates = v
	}
	return out
}

// allowed reports whether the raw MCP tool name passes the filter.
func (f ToolFilter) allowed(name string) bool {
	for _, d := range f.Deny {
		if d == name {
			return false
		}
	}
	if len(f.Allow) == 0 {
		return true
	}
	for _, a := range f.Allow {
		if a == name {
			return true
		}
	}
	return false
}

var (
	serverNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	envVarRE     = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)
)

// expandEnv replaces ${VAR} with os.Getenv("VAR"). Unset vars expand to
// empty string — same UX as docker-compose / k8s. Plain $VAR (no braces) is
// left untouched so we don't surprise users by mangling literal dollars.
func expandEnv(s string) string {
	return envVarRE.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		return os.Getenv(name)
	})
}

// envSlice converts a ServerConfig's env + passthrough into the "K=V"
// slice that os/exec consumes via mcp-go's stdio transport. Order: explicit
// env entries first, then passthrough values inherited from the host.
func envSlice(sc ServerConfig) []string {
	var out []string
	for k, v := range sc.Env {
		out = append(out, k+"="+v)
	}
	for _, k := range sc.EnvPassthrough {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// intLike accepts int, int64, or float64 (YAML may decode any of these).
func intLike(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// firstNonEmpty returns the first non-empty argument; used when a server
// supplies both Title and Name and we want Title-first display.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
