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
}

// CoreConfig holds engine-level settings.
type CoreConfig struct {
	LogLevel            string         `yaml:"log_level"`
	TickInterval        time.Duration  `yaml:"tick_interval"`
	MaxConcurrentEvents int            `yaml:"max_concurrent_events"`
	Sessions            SessionsConfig `yaml:"sessions"`
	ModelsRaw           map[string]any `yaml:"-"` // Parsed from core.models, used to build ModelRegistry
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

	return cfg, nil
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
