// Package objectstore defines the lifecycle seam between a Nexus data tree on
// local disk and a remote object store, plus the driver-style registry that
// lets a backend module make itself selectable by name.
//
// # Why this is not a filesystem abstraction
//
// The obvious design — wrap os.Open/os.WriteFile behind an interface and swap
// in a cloud implementation — was considered and rejected. Every read path in
// the engine and in ~60 plugins would have had to route through it, SQLite
// cannot run against anything but a real local file, and "behaves exactly like
// local disk" would have become an aspiration rather than a guarantee.
//
// Instead this is a *lifecycle* interface. Core and plugins keep writing
// ordinary local files with ordinary os.* calls. The engine calls this seam at
// a handful of lifecycle points only — hydrate before a session opens, push at
// turn boundaries and on artifact events, flush on shutdown. That keeps the
// blast radius to pkg/engine and makes the local tree the single thing every
// read path ever sees.
//
// # Implementable from outside this repo
//
// Nothing here refers to a bucket API, a credential type, an HTTP client or
// any other cloud-specific concept, and this package imports nothing outside
// the standard library. A third party can implement Backend in their own
// module, depend on whatever SDK they like, and register it with a blank
// import — no change to Nexus core and no PR to this repository. That is also
// why this lives in its own package rather than in package engine: a backend
// module must not have to import the whole engine to satisfy the seam.
package objectstore

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Backend moves a local data tree to and from durable remote storage.
//
// # Keys
//
// Every key crossing this interface is *store-relative*: "/"-separated, no
// leading or trailing "/", no "." or ".." segments, and never an OS-specific
// separator. Applying Config.Bucket and Config.Prefix is the implementation's
// job, so the engine never does prefix arithmetic and a backend is free to
// choose its own physical layout underneath.
//
// Those rules are enforced, not merely documented: every method must reject a
// malformed key or prefix with an error wrapping ErrInvalidKey, and must do so
// before touching the remote store or the local filesystem. ValidateKey and
// ValidateKeyPrefix implement exactly the behaviour the contract suite checks.
//
// A key prefix matches on *segment* boundaries. Key K is under prefix P when P
// is empty, or when K begins with P + "/". Raw string matching — the native
// behaviour of every cloud list API — would make the prefix "sessions/sess-1"
// select the objects of "sessions/sess-10", so a backend listing with a raw
// prefix must post-filter. TrimKeyPrefix is that rule in code, and also yields
// the relative path Hydrate needs.
//
// # Durability
//
// Put and Delete may complete asynchronously — a backend is expected to queue
// and batch, and a degraded backend keeps the session running against the
// local copy (see FailurePolicyDegrade). Flush is the only method that
// promises durability: when it returns nil every prior Put and Delete is
// durably stored. Callers that need a guarantee call Flush.
//
// Implementations must be safe for concurrent use — the engine pushes
// artifacts from bus handlers while turn-boundary snapshots run.
type Backend interface {
	// Hydrate populates destDir from every object under keyPrefix, creating
	// directories as needed. It is called before the engine opens a session,
	// so on return every subsequent os.* read must behave exactly as it would
	// on a host that never left.
	//
	// The prefix is *stripped*: an object at "sessions/s1/files/a.md" hydrated
	// under prefix "sessions/s1" lands at filepath.Join(destDir, "files",
	// "a.md"). Existing entries at destDir that no object corresponds to are
	// left alone — Hydrate adds and overwrites, it does not mirror.
	//
	// A keyPrefix with no objects is not an error: it means a brand-new
	// session, and destDir is left as-is (a valid empty tree).
	Hydrate(ctx context.Context, keyPrefix string, destDir string) error

	// Put stores the file at localPath under key, replacing any object already
	// there. The local file is only read: Put must leave it in place and
	// unmodified, since it is the live working copy the session keeps using.
	//
	// An unreadable or absent localPath is an error, and must not leave a
	// partial or empty object at key.
	//
	// A local path is taken rather than an io.Reader deliberately: a path is
	// re-openable and seekable, which is what lets an implementation size a
	// multipart upload and retry a failed part without the caller having to
	// buffer the whole object in memory.
	Put(ctx context.Context, key string, localPath string) error

	// Delete removes key. A missing key is not an error, matching the
	// semantics of every object store worth targeting.
	Delete(ctx context.Context, key string) error

	// List returns the objects under keyPrefix, which may be empty to mean
	// "everything the configured prefix covers". Order is unspecified, and
	// callers must not depend on one.
	//
	// The result is complete: a backend whose remote API pages must follow
	// every page. Returning only the first page is the single most common way
	// to be quietly wrong here, so the contract suite lists past any plausible
	// page size.
	List(ctx context.Context, keyPrefix string) ([]Object, error)

	// Flush blocks until every Put and Delete issued before the call is
	// durably stored, or until ctx is done. It is the engine's turn-boundary
	// and shutdown barrier.
	Flush(ctx context.Context) error
}

// Object describes one stored object. Kept to what a hydration diff and a
// consistency check need — deliberately no ETag, generation, storage class or
// any other field that would only make sense to one vendor.
type Object struct {
	// Key is the store-relative key, in the form described on Backend.
	Key string
	// Size is the object size in bytes.
	Size int64
	// ModTime is when the object was last written. Backends that cannot
	// report a modification time leave it zero.
	ModTime time.Time
}

// FailurePolicy selects what the engine does when the backend cannot persist
// state. It is config-driven rather than per-backend because it is a
// durability trade-off the operator owns, not an implementation detail.
type FailurePolicy string

const (
	// FailurePolicyDegrade keeps the session running against the local copy,
	// queues pushes and retries with backoff. The default: an object store
	// outage should not take down an interactive agent that still has a
	// perfectly good local working tree.
	FailurePolicyDegrade FailurePolicy = "degrade"
	// FailurePolicyStrict fails the turn when state cannot be persisted. For
	// deployments where an unpersisted turn is worse than a failed one.
	FailurePolicyStrict FailurePolicy = "strict"
)

// Config is the parsed object-store configuration block. It is both the YAML
// shape the engine unmarshals into and the value handed to a Factory, so a
// backend sees exactly what the operator wrote with no lossy translation
// layer in between.
//
// Engine-injected values that are not YAML (a logger, for instance) are added
// here as `yaml:"-"` fields as they are needed — the same pattern Config.Raw
// and CoreConfig.ModelsRaw already use — so the Factory signature never churns
// and out-of-repo backends keep compiling.
type Config struct {
	// BackendName selects a registered backend. Empty means object storage is
	// disabled and no object-store code runs at all.
	//
	// Named "BackendName" rather than "Backend" only to keep it distinct from
	// the Backend interface in this package.
	BackendName string `yaml:"backend"`

	// Bucket is the container the backend writes to. Required whenever
	// BackendName is set; Nexus never creates it.
	Bucket string `yaml:"bucket"`

	// Prefix is an optional key prefix within the bucket, letting several
	// deployments share one bucket. It is an *object key* prefix, not a
	// filesystem path, so it is deliberately NOT passed through
	// engine.ExpandPath — a leading "~" is a legal (if odd) key segment and
	// expanding it to a home directory would silently corrupt the layout.
	Prefix string `yaml:"prefix"`

	// Region is the backend's region, where the backend needs one.
	Region string `yaml:"region"`

	// Endpoint overrides the default service endpoint. This is what makes
	// S3-compatible stores (MinIO, R2, Ceph) reachable, and what lets a
	// developer reproduce production behaviour against a local emulator.
	Endpoint string `yaml:"endpoint"`

	// CredentialsFile is an optional path to a static credentials file. Empty
	// means "use ambient credentials" — workload identity, instance role,
	// environment — which is the preferred production path because it needs
	// no key material on disk.
	//
	// The engine expands this through engine.ExpandPath at config load, so a
	// backend receives an already-absolute path and must not re-expand it.
	// Expansion cannot happen inside a backend: ExpandPath lives in package
	// engine, and this package must stay importable without it.
	CredentialsFile string `yaml:"credentials_file"`

	// FailurePolicy is degrade (default) or strict. Normalised to a non-empty
	// value by Validate whenever BackendName is set.
	FailurePolicy FailurePolicy `yaml:"failure_policy"`

	// Logger is the engine's structured logger, already tagged with the
	// subsystem and backend name. Injected by the engine immediately before
	// Open; nil for anyone constructing a Config by hand, so a backend must
	// nil-check (or route through slog.Default) rather than assume it.
	//
	// Passed on the Config rather than added as a Factory parameter on
	// purpose: Factory's signature is the one thing out-of-repo backend
	// modules compile against, and every future engine-injected value would
	// otherwise break them. A yaml:"-" field is the house pattern for exactly
	// this — Config.Raw and CoreConfig.ModelsRaw carry non-YAML state the
	// same way — and it keeps the value out of the session config snapshot,
	// which is built from the YAML keys only.
	Logger *slog.Logger `yaml:"-"`
}

// Enabled reports whether a backend was selected. The zero Config is disabled,
// which is what keeps the default path byte-identical to a build with no
// object-store support at all.
func (c Config) Enabled() bool { return c.BackendName != "" }

// Validate checks the block for internal consistency and confirms the named
// backend is actually registered, normalising FailurePolicy in the process.
// keyPrefix is the dotted config path this block was parsed from (e.g.
// "core.sessions.object_store") and appears in every message so an operator
// is pointed at the exact key rather than at a symptom.
//
// This runs at config load, not at first write. A misconfigured bucket that
// only surfaced when the first artifact was flushed — an hour into a session,
// with the operator long gone — is the failure mode this exists to prevent.
func (c *Config) Validate(keyPrefix string) error {
	if c.BackendName == "" {
		// Nothing selected. Catch the case where the operator filled in the
		// block but forgot the one key that turns it on, which would
		// otherwise be a silent no-op.
		if set := c.populatedKeys(); len(set) > 0 {
			return fmt.Errorf("%s.backend is empty but %s is set; name a backend or remove the block",
				keyPrefix, strings.Join(prefixEach(keyPrefix, set), ", "))
		}
		return nil
	}

	if !Registered(c.BackendName) {
		return fmt.Errorf("%s.backend %q is not a registered object-store backend (registered: %s); "+
			"add the backend module to your build and import it for its side effect",
			keyPrefix, c.BackendName, registeredList())
	}

	if c.Bucket == "" {
		return fmt.Errorf("%s.bucket is required when %s.backend is set", keyPrefix, keyPrefix)
	}
	if strings.HasPrefix(c.Prefix, "/") || strings.HasSuffix(c.Prefix, "/") {
		return fmt.Errorf("%s.prefix %q must not begin or end with %q — keys are store-relative", keyPrefix, c.Prefix, "/")
	}

	switch c.FailurePolicy {
	case "":
		// Normalise at load so no downstream caller has to re-derive the
		// default, and so the resolved value shows up in the session config
		// snapshot.
		c.FailurePolicy = FailurePolicyDegrade
	case FailurePolicyDegrade, FailurePolicyStrict:
	default:
		return fmt.Errorf("%s.failure_policy %q is not valid (want %q or %q)",
			keyPrefix, c.FailurePolicy, FailurePolicyDegrade, FailurePolicyStrict)
	}
	return nil
}

// populatedKeys returns the leaf key names set on the block, ignoring
// BackendName. Used only to build the "you forgot to name a backend" message.
func (c Config) populatedKeys() []string {
	var set []string
	for _, kv := range []struct {
		key string
		val string
	}{
		{"bucket", c.Bucket},
		{"prefix", c.Prefix},
		{"region", c.Region},
		{"endpoint", c.Endpoint},
		{"credentials_file", c.CredentialsFile},
		{"failure_policy", string(c.FailurePolicy)},
	} {
		if kv.val != "" {
			set = append(set, kv.key)
		}
	}
	return set
}

func prefixEach(keyPrefix string, keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = keyPrefix + "." + k
	}
	return out
}
