package main

import (
	"fmt"
	"os"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"gopkg.in/yaml.v3"
)

// Config holds the nexus-broker service configuration. It is loaded from a
// YAML file at startup, mirroring the engine's file-based config style.
//
// All keys are live: listen_addr / nexus_binary_path (gateway + spawn),
// max_concurrent (capacity cap), idle_timeout (idle reaping), release_grace
// (graceful-shutdown grace), and queue_wait_timeout (FIFO capacity wait).
type Config struct {
	// ListenAddr is the host:port the broker's HTTP/WS gateway binds to.
	ListenAddr string `yaml:"listen_addr"`

	// NexusBinaryPath is the path to the nexus binary the broker exec()s to
	// spawn OS-isolated instances. Expanded through engine.ExpandPath.
	NexusBinaryPath string `yaml:"nexus_binary_path"`

	// MaxConcurrent caps the number of live instances. Placeholder for the
	// capacity story.
	MaxConcurrent int `yaml:"max_concurrent"`

	// IdleTimeout is how long an idle instance survives before teardown.
	// Placeholder for the lifecycle story.
	IdleTimeout time.Duration `yaml:"idle_timeout"`

	// QueueWaitTimeout is how long an over-capacity claim parks in the FIFO
	// capacity wait queue before returning a timeout error. A non-positive value
	// disables waiting: an at-capacity claim is rejected immediately.
	QueueWaitTimeout time.Duration `yaml:"queue_wait_timeout"`

	// ReleaseGrace bounds how long a release (manual, idle, or crash teardown)
	// waits for an instance to shut its engine down cleanly before the broker
	// force-kills it. The session is always persisted by the graceful path; the
	// kill is the orphan-prevention backstop.
	ReleaseGrace time.Duration `yaml:"release_grace"`
}

// DefaultConfig returns a Config populated with sane defaults. LoadConfig and
// LoadConfigFromBytes merge YAML on top of these.
func DefaultConfig() Config {
	return Config{
		ListenAddr:       ":8080",
		NexusBinaryPath:  "nexus",
		MaxConcurrent:    8,
		IdleTimeout:      5 * time.Minute,
		QueueWaitTimeout: 30 * time.Second,
		ReleaseGrace:     defaultReleaseGrace,
	}
}

// LoadConfig reads a YAML broker config file from disk.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading broker config file: %w", err)
	}
	return LoadConfigFromBytes(data)
}

// LoadConfigFromBytes parses a YAML broker config from bytes, merged on top of
// DefaultConfig. Every filesystem path is funneled through engine.ExpandPath.
func LoadConfigFromBytes(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing broker config: %w", err)
	}
	cfg.NexusBinaryPath = engine.ExpandPath(cfg.NexusBinaryPath)
	return cfg, nil
}
