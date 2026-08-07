package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	// Empty YAML => all defaults. Compared with reflect.DeepEqual rather than ==
	// because Config carries the raw auth map, which makes it non-comparable.
	cfg, err := LoadConfigFromBytes([]byte(""))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	want := DefaultConfig()
	if !reflect.DeepEqual(cfg, want) {
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

// TestLoadConfigAuthAbsentDisablesAuth pins the backward-compatibility
// guarantee: a broker.yaml with no auth block loads clean and yields a chain
// that is disabled (not permissive, not nil).
func TestLoadConfigAuthAbsentDisablesAuth(t *testing.T) {
	for _, yaml := range []string{
		``,
		"listen_addr: \":7777\"\n",
		"auth:\n", // present but empty: a commented-out block mid-edit
		"auth: {}\n",
		"auth:\n  validators: []\n",
	} {
		cfg, err := LoadConfigFromBytes([]byte(yaml))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes(%q): %v", yaml, err)
		}
		if cfg.AuthChain == nil {
			t.Fatalf("AuthChain is nil for %q; load must always build one", yaml)
		}
		if cfg.AuthChain.Enabled() {
			t.Errorf("AuthChain.Enabled() = true for %q, want disabled", yaml)
		}
	}
}

// TestLoadConfigAuthStaticBuildsChain proves a well-formed auth block produces
// an enabled chain, in configured order.
func TestLoadConfigAuthStaticBuildsChain(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`
auth:
  validators:
    - type: static
      tokens:
        - token: "s3cret"
          principal: "ci-runner"
          tenant: "acme"
          scopes: "broker.claim broker.release"
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if !cfg.AuthChain.Enabled() {
		t.Fatal("AuthChain.Enabled() = false, want enabled")
	}
	if got := cfg.AuthChain.Names(); !reflect.DeepEqual(got, []string{"static"}) {
		t.Errorf("Names() = %v, want [static]", got)
	}
	// The raw block is retained so it stays inspectable/loggable.
	if cfg.Auth == nil {
		t.Error("Auth = nil, want the raw block retained")
	}
}

// TestLoadConfigMalformedAuthBlockFails is the security-relevant half of the
// contract: a malformed auth block must be a BOOT FAILURE naming the offending
// key, never a silent fallback to "auth disabled".
func TestLoadConfigMalformedAuthBlockFails(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantKeys []string // substrings the error must mention
	}{
		{
			name:     "auth is not a mapping",
			yaml:     "auth: \"on\"\n",
			wantKeys: []string{"auth"},
		},
		{
			name:     "unknown key inside auth",
			yaml:     "auth:\n  validatorz: []\n",
			wantKeys: []string{"validatorz"},
		},
		{
			name:     "validators is not a list",
			yaml:     "auth:\n  validators: {}\n",
			wantKeys: []string{"validators"},
		},
		{
			name:     "validator entry missing type",
			yaml:     "auth:\n  validators:\n    - tokens: []\n",
			wantKeys: []string{"validators[0]", "type"},
		},
		{
			name:     "unknown validator type",
			yaml:     "auth:\n  validators:\n    - type: telepathy\n",
			wantKeys: []string{"telepathy"},
		},
		{
			name:     "static validator without tokens",
			yaml:     "auth:\n  validators:\n    - type: static\n",
			wantKeys: []string{"tokens"},
		},
		{
			name:     "misspelled token key",
			yaml:     "auth:\n  validators:\n    - type: static\n      tokens:\n        - tokne: x\n          principal: p\n",
			wantKeys: []string{"tokne"},
		},
		{
			name:     "token without a principal",
			yaml:     "auth:\n  validators:\n    - type: static\n      tokens:\n        - token: x\n",
			wantKeys: []string{"principal"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("LoadConfigFromBytes succeeded; want a boot failure (chain enabled=%v)",
					cfg.AuthChain.Enabled())
			}
			for _, key := range tc.wantKeys {
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error %q does not name %q", err, key)
				}
			}
		})
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
