package main

import (
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	// Empty YAML => all defaults.
	cfg, err := LoadConfigFromBytes([]byte(""))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	want := DefaultConfig()
	if cfg != want {
		t.Errorf("defaults mismatch:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	yaml := `
listen_addr: "127.0.0.1:9000"
nexus_binary_path: "/opt/nexus/bin/nexus"
max_concurrent: 32
idle_timeout: 2m
queue_wait_timeout: 10s
release_grace: 20s
`
	cfg, err := LoadConfigFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.NexusBinaryPath != "/opt/nexus/bin/nexus" {
		t.Errorf("NexusBinaryPath = %q", cfg.NexusBinaryPath)
	}
	if cfg.MaxConcurrent != 32 {
		t.Errorf("MaxConcurrent = %d", cfg.MaxConcurrent)
	}
	if cfg.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout = %v", cfg.IdleTimeout)
	}
	if cfg.QueueWaitTimeout != 10*time.Second {
		t.Errorf("QueueWaitTimeout = %v", cfg.QueueWaitTimeout)
	}
	if cfg.ReleaseGrace != 20*time.Second {
		t.Errorf("ReleaseGrace = %v", cfg.ReleaseGrace)
	}
}

func TestLoadConfigExpandsBinaryPath(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`nexus_binary_path: "~/bin/nexus"`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.NexusBinaryPath == "~/bin/nexus" {
		t.Errorf("expected ~ to be expanded, got %q", cfg.NexusBinaryPath)
	}
}

func TestLoadConfigPartialOverrideKeepsDefaults(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`listen_addr: ":7777"`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.ListenAddr != ":7777" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	def := DefaultConfig()
	if cfg.MaxConcurrent != def.MaxConcurrent {
		t.Errorf("MaxConcurrent = %d, want default %d", cfg.MaxConcurrent, def.MaxConcurrent)
	}
	if cfg.IdleTimeout != def.IdleTimeout {
		t.Errorf("IdleTimeout = %v, want default %v", cfg.IdleTimeout, def.IdleTimeout)
	}
}
