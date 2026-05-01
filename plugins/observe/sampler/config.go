package sampler

import (
	"fmt"

	"github.com/frankbardon/nexus/pkg/engine"
)

// config is the parsed plugin configuration. Defaults match the off-by-default
// contract: a config that omits `sampler` entirely (or sets `enabled: false`)
// produces a no-op plugin.
type config struct {
	enabled        bool
	rate           float64
	failureCapture bool
	outDir         string
}

// defaultOutDir is the documented default for sampler.out_dir. Resolved via
// engine.ExpandPath at parse time so users see the expanded form in logs.
const defaultOutDir = "~/.nexus/eval/samples"

// parseConfig reads the raw plugin config map and returns a typed config.
// Returns an error when the input cannot satisfy the documented contract
// (e.g. rate outside [0,1]). enabled=false bypasses validation entirely so
// a stale rate value left in a disabled block does not block boot.
func parseConfig(raw map[string]any) (config, error) {
	cfg := config{
		enabled:        false,
		rate:           0.0,
		failureCapture: true,
		outDir:         defaultOutDir,
	}

	if v, ok := raw["enabled"]; ok {
		if b, ok := v.(bool); ok {
			cfg.enabled = b
		}
	}
	if v, ok := raw["rate"]; ok {
		switch n := v.(type) {
		case float64:
			cfg.rate = n
		case float32:
			cfg.rate = float64(n)
		case int:
			cfg.rate = float64(n)
		case int64:
			cfg.rate = float64(n)
		}
	}
	if v, ok := raw["failure_capture"]; ok {
		if b, ok := v.(bool); ok {
			cfg.failureCapture = b
		}
	}
	if v, ok := raw["out_dir"]; ok {
		if s, ok := v.(string); ok && s != "" {
			cfg.outDir = s
		}
	}

	cfg.outDir = engine.ExpandPath(cfg.outDir)

	if !cfg.enabled {
		return cfg, nil
	}
	if cfg.rate < 0 || cfg.rate > 1 {
		return cfg, fmt.Errorf("sampler.rate must be in [0,1], got %v", cfg.rate)
	}
	if cfg.outDir == "" {
		return cfg, fmt.Errorf("sampler.out_dir must not be empty when enabled")
	}
	return cfg, nil
}
