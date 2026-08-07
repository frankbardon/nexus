package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/nexusauth"
	"gopkg.in/yaml.v3"
)

// Config holds the nexus-broker service configuration. It is loaded from a
// YAML file at startup, mirroring the engine's file-based config style.
//
// All keys are live: listen_addr / nexus_binary_path (gateway + spawn),
// max_concurrent (capacity cap), idle_timeout (idle reaping), release_grace
// (graceful-shutdown grace), queue_wait_timeout (FIFO capacity wait), and auth
// (client authentication).
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

	// Auth is the raw `auth:` block, handed to pkg/nexusauth verbatim. It is
	// kept as a map rather than a typed struct because nexusauth owns the
	// validator vocabulary and parses it identically for the broker and for
	// plugin config; duplicating that shape here would let the two drift.
	//
	// Absent (or empty) means authentication is not configured, which the broker
	// treats as auth-off plus a loud startup warning — see run() in main.go.
	Auth authBlock `yaml:"auth"`

	// AdminScope is the scope a validated credential must carry to be treated as
	// a broker operator. It widens GET /leases from "the caller's own leases" to
	// the whole registry plus the capacity aggregates, and nothing else — the
	// mutating lease routes remain strict principal-id ownership.
	//
	// It is configured as `auth.admin_scope` but is NOT part of the map handed to
	// nexusauth: that package owns the `auth:` vocabulary and rejects every key it
	// does not know, so LoadConfigFromBytes lifts this one out of the block before
	// building the chain (see liftAdminScope). One key, one owner.
	//
	// Empty means NO caller is an operator: with auth on, GET /leases is then
	// caller-scoped for everybody. That is the safe reading of "no admin scope was
	// configured", and setting `admin_scope: ""` is the supported way to switch the
	// operator view off entirely.
	AdminScope string `yaml:"-"`

	// AuthChain is the validator chain built from Auth. It is not a YAML key:
	// LoadConfigFromBytes populates it so a malformed `auth:` block fails at
	// load — before anything is served — and so the chain (which a future
	// network-backed validator may hold live state for) is constructed exactly
	// once per process. It is never nil after a successful load, and a chain with
	// no validators is disabled, not permissive.
	AuthChain *nexusauth.Chain `yaml:"-"`
}

// authBlock is the raw `auth:` mapping. It exists only to give a non-mapping
// `auth:` value an error that names the offending key: a silent fallback to
// "auth disabled" on a malformed block would be a security bug, so every
// malformed shape has to be a boot failure the operator can act on.
type authBlock map[string]any

// UnmarshalYAML implements yaml.Unmarshaler.
func (a *authBlock) UnmarshalYAML(node *yaml.Node) error {
	// `auth:` with nothing under it decodes as a null scalar. Treat it as absent
	// rather than malformed — it is a commented-out block mid-edit, not a typo.
	if node.Tag == "!!null" {
		*a = nil
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("auth: want a mapping of auth settings, got %s", node.Tag)
	}
	m := map[string]any{}
	if err := node.Decode(&m); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	*a = m
	return nil
}

// keyAdminScope is the broker-only key inside the `auth:` block that names the
// operator scope. It lives in the auth block because it is an authorization
// setting, but it is the broker's key, not nexusauth's — see liftAdminScope.
const keyAdminScope = "admin_scope"

// defaultAdminScope is the scope GET /leases treats as "operator" when the
// config does not say otherwise. It is namespaced like the rest of the project's
// dotted identifiers so it cannot collide with a scope an operator's IdP already
// issues for something else; a deployment whose IdP uses a different vocabulary
// overrides it with `auth.admin_scope`.
const defaultAdminScope = "nexus.broker.admin"

// DefaultConfig returns a Config populated with sane defaults. LoadConfig and
// LoadConfigFromBytes merge YAML on top of these.
//
// Auth defaults to absent, and therefore to a disabled AuthChain: a broker that
// has never been told about authentication must keep serving exactly as it did
// before authentication existed. AdminScope carries a default even so, because it
// only ever takes effect once auth IS configured.
func DefaultConfig() Config {
	return Config{
		ListenAddr:       ":8080",
		NexusBinaryPath:  "nexus",
		MaxConcurrent:    8,
		IdleTimeout:      5 * time.Minute,
		QueueWaitTimeout: 30 * time.Second,
		ReleaseGrace:     defaultReleaseGrace,
		AdminScope:       defaultAdminScope,
		AuthChain:        nexusauth.NewChain(),
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

	// Take the broker's own key out of the auth block BEFORE the chain is built,
	// so what reaches nexusauth is exactly the block it owns.
	adminScope, err := liftAdminScope(cfg.Auth, cfg.AdminScope)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.AdminScope = adminScope

	// Build the validator chain here, at load, so an operator learns about a
	// misconfigured `auth:` block from a failed boot rather than from requests
	// being refused (or worse, waved through) in production. nexusauth's errors
	// name the offending key and path.
	chain, err := nexusauth.ChainFromMap(cfg.Auth)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.AuthChain = chain
	return cfg, nil
}

// liftAdminScope removes the broker-only `admin_scope` key from the raw auth
// block and returns the value it should take, falling back to current (the
// default) when the key is absent.
//
// It REMOVES rather than reads in place because everything left in the block is
// handed verbatim to nexusauth, which rejects any key it does not recognize: a
// key left behind would turn a perfectly valid config into a boot failure. The
// map is mutated in place so cfg.Auth keeps exactly one meaning — "the block
// nexusauth owns" — and nothing downstream has to remember to skip a key.
//
// `admin_scope:` present but empty (or explicitly null) is honoured as "no caller
// is an operator", so the operator view can be switched off; only an ABSENT key
// inherits the default.
func liftAdminScope(block authBlock, current string) (string, error) {
	raw, present := block[keyAdminScope]
	if !present {
		return current, nil
	}
	delete(block, keyAdminScope)
	if raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("auth: %s: want a string, got %T", keyAdminScope, raw)
	}
	return strings.TrimSpace(s), nil
}
