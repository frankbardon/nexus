package main

import (
	"fmt"
	"net"
	"net/url"
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
// advertise_addr (client-reachable address), max_concurrent (capacity cap),
// idle_timeout (idle reaping), release_grace (graceful-shutdown grace),
// queue_wait_timeout (FIFO capacity wait), and auth (client authentication).
type Config struct {
	// ListenAddr is the host:port the broker's HTTP/WS gateway binds to.
	ListenAddr string `yaml:"listen_addr"`

	// AdvertiseAddr is the address CLIENTS use to reach THIS broker, and it is
	// the highest-precedence input to the ws_url returned by POST /claim.
	//
	// It exists because listen_addr cannot answer the question. A wildcard bind
	// (":8080") names no host, so without this key the ws_url falls back to the
	// claim request's Host header — which behind a reverse proxy or load
	// balancer names the PROXY. A client then reconnects through the LB and may
	// land on a different broker, one that does not hold its lease. Auto-detection
	// is deliberately not attempted: a heuristic would fail silently in exactly
	// the deployment shape that matters, so this is explicit config or nothing.
	//
	// Accepted forms are `host:port` (implying the ws:// scheme, so existing
	// deployments see no change) or a scheme-qualified `ws://`, `wss://`,
	// `http://` or `https://` host, with the port optional in that form. The
	// scheme-qualified form exists for the TLS-terminating-proxy deployment,
	// where the broker itself speaks plain HTTP but clients must dial wss://.
	//
	// Empty (the default) preserves the pre-existing resolution order exactly.
	AdvertiseAddr string `yaml:"advertise_addr"`

	// AdvertiseScheme and AdvertiseHost are the parsed, validated form of
	// AdvertiseAddr. They are not YAML keys: LoadConfigFromBytes derives them so
	// a malformed advertise_addr fails the BOOT rather than quietly minting
	// broken URLs at claim time, and so the parse happens once per process
	// instead of once per claim.
	//
	// Both are empty when advertise_addr is unset. AdvertiseScheme is empty for
	// the bare `host:port` form too — the ws:// default is applied at use
	// (clientWSScheme), which keeps "unset" a single state rather than two.
	AdvertiseScheme string `yaml:"-"`
	AdvertiseHost   string `yaml:"-"`

	// NexusBinaryPath is the path to the nexus binary the broker exec()s to
	// spawn OS-isolated instances. Expanded through engine.ExpandPath.
	NexusBinaryPath string `yaml:"nexus_binary_path"`

	// StateDir is the per-broker directory holding this broker's lease journal
	// (`leases.jsonl`) and, when broker_id is unset, its generated identity
	// (`broker-id`). Expanded through engine.ExpandPath.
	//
	// EMPTY (the default) DISABLES PERSISTENCE ENTIRELY: nothing is written, no
	// directory is created, and the broker behaves exactly as it did before lease
	// durability existed. That is the default because durability writes an
	// operator did not ask for — into a path Nexus picked — is a worse surprise
	// than a restart that loses in-memory bookkeeping, which is the behaviour
	// every existing deployment already has.
	//
	// It must NOT be shared between brokers. Two brokers pointed at one directory
	// would append to the same journal and compact each other's live leases away.
	// Each broker gets its own.
	StateDir string `yaml:"state_dir"`

	// BrokerID is this broker's identity, stamped on every persisted lease record
	// alongside advertise_addr so a future shared store can tell whose lease is
	// whose. It must be STABLE ACROSS RESTARTS of the same broker.
	//
	// Empty (the default) means the broker generates one on first boot and
	// persists it at <state_dir>/broker-id, reusing it thereafter — stable and
	// unique with no operator effort. Set it explicitly to give a broker a name
	// that means something in a cluster ("broker-eu-1"). Irrelevant while
	// state_dir is unset, since nothing is then recorded.
	BrokerID string `yaml:"broker_id"`

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

	// ReattachWindow bounds how long a lease restored from the journal at boot may
	// sit with no instance reattached before the broker reaps it: kills the
	// process, frees the slot, and closes the record out.
	//
	// It exists because restart recovery adopts leases on the strength of a live
	// pid, and a pid can be alive without anything ever coming back for it — the
	// instance may have been killed and its pid reused, or it may be wedged. The
	// window is the point at which the broker stops holding a capacity slot open
	// for an instance that is not going to reconnect.
	//
	// A non-positive value falls back to defaultReattachWindow. It is deliberately
	// NOT disableable: "wait forever" is the orphaned-lease behaviour this key
	// exists to bound. Irrelevant while state_dir is unset, since nothing is then
	// restored.
	ReattachWindow time.Duration `yaml:"reattach_window"`

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
		ReattachWindow:   defaultReattachWindow,
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
	// Trimmed before expansion so a whitespace-only value reads as "unset"
	// (persistence disabled) rather than as a directory literally named " ".
	cfg.StateDir = engine.ExpandPath(strings.TrimSpace(cfg.StateDir))

	// Parse advertise_addr here so a malformed value is a boot failure. Deferring
	// it to claim time would surface the mistake as clients failing to connect to
	// a URL nobody looked at, which is the failure mode this key exists to remove.
	scheme, host, err := parseAdvertiseAddr(cfg.AdvertiseAddr)
	if err != nil {
		return Config{}, fmt.Errorf("broker config: %w", err)
	}
	cfg.AdvertiseScheme = scheme
	cfg.AdvertiseHost = host

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

// parseAdvertiseAddr splits a raw advertise_addr into the WebSocket scheme a
// client should use and the host:port it should dial. An empty input returns two
// empty strings and no error: advertise_addr is optional, and unset must keep the
// pre-existing ws_url resolution intact.
//
// Validation is deliberately strict, because every value that gets through here
// is handed to clients as an absolute URL:
//
//   - The bare form must carry a port. `example.com` alone would silently mean
//     port 80 via ws://, which is almost never where a broker listens.
//   - A wildcard host is rejected in either form. `0.0.0.0` is a bind address,
//     not a dialable one, and accepting it would reintroduce exactly the broken
//     ws_url this key exists to prevent.
//   - Path, query, fragment and userinfo are rejected in the URL form. The lease
//     path is appended by the caller, so anything here would corrupt the result.
func parseAdvertiseAddr(raw string) (scheme, host string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", nil
	}

	// "://" is the discriminator rather than ":" because an IPv6 literal
	// ("[::1]:8080") is full of colons but is not a URL.
	if strings.Contains(raw, "://") {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", "", fmt.Errorf("advertise_addr: %q is not a valid URL: %w", raw, perr)
		}
		switch u.Scheme {
		case "ws", "http":
			// http is accepted and normalized because operators reach for the
			// scheme they type into a browser; refusing it would be pedantry.
			scheme = "ws"
		case "wss", "https":
			scheme = "wss"
		default:
			return "", "", fmt.Errorf("advertise_addr: %q has unsupported scheme %q, want ws, wss, http or https", raw, u.Scheme)
		}
		if u.User != nil {
			return "", "", fmt.Errorf("advertise_addr: %q must not carry userinfo", raw)
		}
		if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", fmt.Errorf("advertise_addr: %q must name only a host[:port]; the lease path is appended by the broker", raw)
		}
		host = u.Host
	} else {
		h, port, serr := net.SplitHostPort(raw)
		if serr != nil {
			return "", "", fmt.Errorf("advertise_addr: %q is not a host:port (add a port, or use the ws://host / wss://host form): %w", raw, serr)
		}
		if port == "" {
			return "", "", fmt.Errorf("advertise_addr: %q has no port", raw)
		}
		host = net.JoinHostPort(h, port)
	}

	if err := checkAdvertiseHost(raw, host); err != nil {
		return "", "", err
	}
	return scheme, host, nil
}

// checkAdvertiseHost rejects a host that no client could dial. hostPort may or
// may not carry a port (the URL form allows omitting it), so the port is stripped
// only when it is actually there.
func checkAdvertiseHost(raw, hostPort string) error {
	bare := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		bare = h
	}
	switch bare {
	case "", "0.0.0.0", "::":
		return fmt.Errorf("advertise_addr: %q names no dialable host; it must name the address clients use to reach this broker", raw)
	}
	return nil
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
