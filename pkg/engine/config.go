package engine

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for the engine.
type Config struct {
	Core CoreConfig `yaml:"core"`
	// Capabilities pins a capability name to a specific provider plugin ID,
	// overriding the default resolution (first active provider, else first
	// registered). Populated from the top-level YAML `capabilities:` block.
	Capabilities map[string]string `yaml:"capabilities"`
	Plugins      PluginsConfig     `yaml:"plugins"`
	// Journal tunes the always-on durable event log. The journal cannot be
	// disabled — it is core, not a plugin — but its fsync, retention, and
	// rotation thresholds are exposed here for ops trade-offs.
	Journal JournalConfig `yaml:"journal"`

	// Raw is the original config YAML bytes the engine was loaded from.
	// Populated by LoadConfigFromBytes (and by extension LoadConfig). Empty
	// for configs constructed in-memory via DefaultConfig. Used by Engine
	// to write a bytes-faithful session config snapshot — re-marshaling the
	// typed Config drops fields parsed via the second-pass raw map (core.models
	// and per-plugin configs both have yaml:"-").
	Raw []byte `yaml:"-"`
}

// JournalConfig tunes the durable per-session event log. Defaults are picked
// for the typical interactive run (turns of 5–30 events, multi-day session
// retention). Set fsync to "every-event" for forensic durability or "none"
// for ephemeral test sessions.
type JournalConfig struct {
	// Fsync controls the disk-flush policy. Values: "turn-boundary"
	// (default), "every-event", "none".
	Fsync string `yaml:"fsync"`
	// RetainDays is the age past which a session journal is swept on
	// engine boot. 0 disables sweeping (in-flight sessions are never
	// touched regardless).
	RetainDays int `yaml:"retain_days"`
	// RotateSizeMB triggers segment rotation on agent.turn.end when the
	// active segment exceeds this many MiB. Rotated segments are
	// zstd-compressed.
	RotateSizeMB int `yaml:"rotate_size_mb"`
}

// CoreConfig holds engine-level settings.
type CoreConfig struct {
	LogLevel            string         `yaml:"log_level"`
	Logging             LoggingConfig  `yaml:"logging"`
	TickInterval        time.Duration  `yaml:"tick_interval"`
	MaxConcurrentEvents int            `yaml:"max_concurrent_events"`
	Sessions            SessionsConfig `yaml:"sessions"`
	ModelsRaw           map[string]any `yaml:"-"` // Parsed from core.models, used to build ModelRegistry
}

// LoggingConfig controls the engine-wide logging pipeline.
//
// The engine logger is a FanoutHandler with a bounded ring buffer and zero or
// more dynamically-registered sinks. Absent a sink, records live only in the
// ring until one registers (typically the logger plugin) or they are evicted.
// This prevents log output from leaking to stdout/stderr when a visual IO
// plugin owns the terminal.
type LoggingConfig struct {
	// BootstrapStderr, when true, registers a stderr sink at engine
	// construction time so pre-sink records appear on the terminal. Default
	// false. Rejected by config validation when a known visual transport
	// plugin (nexus.io.tui, nexus.io.browser, nexus.io.wails) is active,
	// because interleaving slog output with the UI corrupts the display.
	BootstrapStderr bool `yaml:"bootstrap_stderr"`
	// BufferSize is the capacity of the slog log ring buffer. Values <= 0
	// default to DefaultLogRingSize. The bus event ring is sized
	// independently (DefaultEventRingSize) — durable event history lives
	// in the journal, not the bus ring.
	BufferSize int `yaml:"buffer_size"`
}

// SessionsConfig controls session workspace behavior.
type SessionsConfig struct {
	Root      string `yaml:"root"`
	Retention string `yaml:"retention"`
	IDFormat  string `yaml:"id_format"`
}

// PluginsConfig holds plugin activation and per-plugin config.
type PluginsConfig struct {
	Active  []string                  `yaml:"active"`
	Configs map[string]map[string]any `yaml:"-"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Core: CoreConfig{
			LogLevel:            "info",
			TickInterval:        1 * time.Second,
			MaxConcurrentEvents: 100,
			Sessions: SessionsConfig{
				Root:      "~/.nexus/sessions",
				Retention: "30d",
				IDFormat:  "timestamp",
			},
		},
		Capabilities: map[string]string{},
		Plugins: PluginsConfig{
			Active:  []string{},
			Configs: make(map[string]map[string]any),
		},
		Journal: JournalConfig{
			Fsync:        "turn-boundary",
			RetainDays:   30,
			RotateSizeMB: 4,
		},
	}
}

// LoadConfig reads a YAML config file from disk and returns a
// Config. It is a thin wrapper around LoadConfigFromBytes and exists
// for the CLI binary that still reads config from a path; embedders
// that want to ship a compiled-in config should use
// LoadConfigFromBytes (or NewFromBytes on Engine) instead, so the
// final binary has no filesystem dependency at boot.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	return LoadConfigFromBytes(data)
}

// LoadConfigFromBytes parses a YAML config from a byte slice and
// returns a Config merged on top of DefaultConfig. Embedders that
// //go:embed a config.yaml alongside their main package use this to
// build a Config without ever touching the filesystem.
//
// The two-pass unmarshal (typed struct first, then raw map for
// per-plugin configs and core.models) is the same shape the
// file-based loader always used — this function just takes the
// bytes as an argument instead of reading them itself.
func LoadConfigFromBytes(data []byte) (*Config, error) {
	cfg := DefaultConfig()

	// First pass: unmarshal the known fields.
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Second pass: extract per-plugin configs from the plugins section.
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing raw config: %w", err)
	}

	if pluginsRaw, ok := raw["plugins"]; ok {
		if pluginsMap, ok := pluginsRaw.(map[string]any); ok {
			cfg.Plugins.Configs = extractPluginConfigs(pluginsMap)
		}
	}

	// Extract core.models section for ModelRegistry.
	if coreRaw, ok := raw["core"]; ok {
		if coreMap, ok := coreRaw.(map[string]any); ok {
			if modelsRaw, ok := coreMap["models"]; ok {
				if modelsMap, ok := modelsRaw.(map[string]any); ok {
					cfg.Core.ModelsRaw = modelsMap
				}
			}
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// Stash the input bytes so the engine can write a bytes-faithful config
	// snapshot at session start. yaml.Marshal(cfg) loses core.models and
	// per-plugin configs (both yaml:"-"), which breaks session recall.
	cfg.Raw = append([]byte(nil), data...)

	return cfg, nil
}

// visualTransportPlugins are base plugin IDs that own the terminal or a
// webview when active. Writing slog output to stderr while any of these is
// active corrupts the UI, so BootstrapStderr is rejected in that case.
var visualTransportPlugins = map[string]bool{
	"nexus.io.tui":     true,
	"nexus.io.browser": true,
	"nexus.io.wails":   true,
}

// validate is a post-load check. Keep rules narrow: only flag combinations
// the engine cannot recover from, not stylistic issues.
func (c *Config) validate() error {
	if c.Core.Logging.BootstrapStderr {
		for _, id := range c.Plugins.Active {
			base := id
			if i := len(id); i > 0 {
				// Strip instance suffix ("foo/bar" -> "foo") without pulling
				// in PluginBaseID to keep this file free of cross-deps.
				for j := 0; j < i; j++ {
					if id[j] == '/' {
						base = id[:j]
						break
					}
				}
			}
			if visualTransportPlugins[base] {
				return fmt.Errorf("invalid config: core.logging.bootstrap_stderr is true but visual transport plugin %q is active; disable bootstrap_stderr or remove the visual plugin", id)
			}
		}
	}
	return nil
}

// extractPluginConfigs pulls per-plugin config maps from the plugins section.
// Keys that are not "active" are treated as plugin IDs.
func extractPluginConfigs(pluginsMap map[string]any) map[string]map[string]any {
	configs := make(map[string]map[string]any)
	for key, val := range pluginsMap {
		if key == "active" {
			continue
		}
		if m, ok := val.(map[string]any); ok {
			configs[key] = m
		}
	}
	return configs
}
