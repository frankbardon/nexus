package main

import (
	"fmt"
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

// TestLoadConfigExpandsStateDir pins the path rule for the durability key: every
// config-supplied filesystem path goes through engine.ExpandPath, so an operator
// can write `~/.nexus/broker` wherever a path is accepted.
func TestLoadConfigExpandsStateDir(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`state_dir: "~/.nexus/broker-state"`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if strings.HasPrefix(cfg.StateDir, "~") {
		t.Errorf("expected ~ to be expanded, got %q", cfg.StateDir)
	}
	if !strings.HasSuffix(cfg.StateDir, "/.nexus/broker-state") {
		t.Errorf("StateDir = %q, want the configured suffix preserved", cfg.StateDir)
	}
}

// TestLoadConfigStateDirDefaultsToDisabled pins the compatibility guarantee: a
// broker.yaml that never mentions state_dir persists nothing, so an existing
// deployment behaves exactly as it did before durability existed. A whitespace
// -only value reads as unset too, rather than as a directory named " ".
func TestLoadConfigStateDirDefaultsToDisabled(t *testing.T) {
	for _, yaml := range []string{
		``,
		"listen_addr: \":7777\"\n",
		"state_dir: \"\"\n",
		"state_dir: \"   \"\n",
	} {
		cfg, err := LoadConfigFromBytes([]byte(yaml))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes(%q): %v", yaml, err)
		}
		if cfg.StateDir != "" {
			t.Errorf("StateDir = %q for %q, want empty (persistence disabled)", cfg.StateDir, yaml)
		}
	}
}

// TestLoadConfigBrokerID: an explicit broker_id is carried verbatim; absent it
// stays empty and the store generates and persists one.
func TestLoadConfigBrokerID(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte("broker_id: \"broker-eu-1\"\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.BrokerID != "broker-eu-1" {
		t.Errorf("BrokerID = %q, want broker-eu-1", cfg.BrokerID)
	}
	if def := DefaultConfig(); def.BrokerID != "" {
		t.Errorf("DefaultConfig().BrokerID = %q, want empty", def.BrokerID)
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

// TestLoadConfigAdminScope covers the broker-only `auth.admin_scope` key: it
// defaults, it can be overridden, it can be switched OFF with an empty value, and
// — the load-bearing part — it is lifted out of the block before nexusauth sees
// it. nexusauth rejects unknown keys, so a config carrying admin_scope alongside
// validators must still build a chain; if the lift regressed, every such config
// would fail to boot.
func TestLoadConfigAdminScope(t *testing.T) {
	const withValidators = `
auth:
  admin_scope: %s
  validators:
    - type: static
      tokens:
        - token: "t"
          principal: "p"
`
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"absent keeps the default", "", defaultAdminScope},
		{"absent alongside an auth block keeps the default", "auth:\n  validators: []\n", defaultAdminScope},
		{"explicit value wins", fmt.Sprintf(withValidators, `"ops.admin"`), "ops.admin"},
		{"surrounding whitespace is trimmed", fmt.Sprintf(withValidators, `"  ops.admin  "`), "ops.admin"},
		{"empty string disables the operator view", fmt.Sprintf(withValidators, `""`), ""},
		{"null disables the operator view", fmt.Sprintf(withValidators, ``), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("LoadConfigFromBytes: %v", err)
			}
			if cfg.AdminScope != tc.want {
				t.Errorf("AdminScope = %q, want %q", cfg.AdminScope, tc.want)
			}
			// The key must not survive in the block handed to nexusauth.
			if _, present := cfg.Auth[keyAdminScope]; present {
				t.Errorf("%s left in the raw auth block; nexusauth would reject it", keyAdminScope)
			}
		})
	}

	// A non-string value is a boot failure naming the key, not a silent ignore
	// that would leave the operator view wide open on the default scope.
	if _, err := LoadConfigFromBytes([]byte("auth:\n  admin_scope: 42\n")); err == nil {
		t.Fatal("LoadConfigFromBytes succeeded for a non-string admin_scope; want a boot failure")
	} else if !strings.Contains(err.Error(), keyAdminScope) {
		t.Errorf("error %q does not name %q", err, keyAdminScope)
	}
}

// TestLoadConfigAdvertiseAddrDefaultsToUnset pins the backward-compatibility
// half of E3-S1: a broker.yaml that never heard of advertise_addr must leave both
// derived fields empty, which is what makes clientWSHost behave exactly as it did
// before the key existed.
func TestLoadConfigAdvertiseAddrDefaultsToUnset(t *testing.T) {
	for _, yaml := range []string{``, "listen_addr: \":8080\"\n", "advertise_addr: \"\"\n", "advertise_addr:\n"} {
		cfg, err := LoadConfigFromBytes([]byte(yaml))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes(%q): %v", yaml, err)
		}
		if cfg.AdvertiseHost != "" || cfg.AdvertiseScheme != "" {
			t.Errorf("for %q: AdvertiseScheme/Host = %q/%q, want both empty",
				yaml, cfg.AdvertiseScheme, cfg.AdvertiseHost)
		}
	}
}

// TestLoadConfigAdvertiseAddrParsed covers every accepted form: the bare
// host:port (which must NOT set a scheme, so ws:// stays the default) and the
// scheme-qualified forms, including http/https normalization and the
// port-optional case.
func TestLoadConfigAdvertiseAddrParsed(t *testing.T) {
	cases := []struct {
		raw        string
		wantScheme string
		wantHost   string
	}{
		{"broker.example.com:8443", "", "broker.example.com:8443"},
		{"  broker.example.com:8443  ", "", "broker.example.com:8443"}, // trimmed
		{"10.0.0.7:8080", "", "10.0.0.7:8080"},
		{"[2001:db8::1]:8080", "", "[2001:db8::1]:8080"},
		{"ws://broker.example.com:8080", "ws", "broker.example.com:8080"},
		{"wss://broker.example.com:443", "wss", "broker.example.com:443"},
		{"wss://broker.example.com", "wss", "broker.example.com"}, // port optional with a scheme
		{"http://broker.example.com:8080", "ws", "broker.example.com:8080"},
		{"https://broker.example.com", "wss", "broker.example.com"},
		{"wss://broker.example.com/", "wss", "broker.example.com"}, // bare trailing slash is not a path
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte("advertise_addr: " + fmt.Sprintf("%q", tc.raw) + "\n"))
			if err != nil {
				t.Fatalf("LoadConfigFromBytes: %v", err)
			}
			if cfg.AdvertiseScheme != tc.wantScheme {
				t.Errorf("AdvertiseScheme = %q, want %q", cfg.AdvertiseScheme, tc.wantScheme)
			}
			if cfg.AdvertiseHost != tc.wantHost {
				t.Errorf("AdvertiseHost = %q, want %q", cfg.AdvertiseHost, tc.wantHost)
			}
			// The raw value is retained verbatim (modulo nothing) for logging.
			if cfg.AdvertiseAddr != tc.raw {
				t.Errorf("AdvertiseAddr = %q, want the raw value %q", cfg.AdvertiseAddr, tc.raw)
			}
		})
	}
}

// TestLoadConfigMalformedAdvertiseAddrFails is the story's boot-failure
// criterion: a value that could not produce a dialable URL must stop startup,
// naming the key, rather than surfacing as clients failing to connect later.
func TestLoadConfigMalformedAdvertiseAddrFails(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"bare host without a port", "broker.example.com"},
		{"port only", ":8080"},
		{"wildcard bare", "0.0.0.0:8080"},
		{"wildcard v6 bare", "[::]:8080"},
		{"wildcard in url form", "ws://0.0.0.0:8080"},
		{"scheme with no host", "wss://"},
		{"unsupported scheme", "tcp://broker.example.com:8080"},
		{"carries a path", "wss://broker.example.com/gateway"},
		{"carries a query", "wss://broker.example.com?x=1"},
		{"carries userinfo", "wss://user:pw@broker.example.com"},
		{"too many colons", "a:b:c"},
		{"not parseable as a url", "ws://[::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte("advertise_addr: " + fmt.Sprintf("%q", tc.raw) + "\n"))
			if err == nil {
				t.Fatalf("LoadConfigFromBytes(%q) succeeded (host=%q); want a boot failure",
					tc.raw, cfg.AdvertiseHost)
			}
			if !strings.Contains(err.Error(), "advertise_addr") {
				t.Errorf("error %q does not name advertise_addr", err)
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
