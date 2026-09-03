package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// This file is the only place in the engine that talks to
// pkg/engine/objectstore. Everything below it — plugins, tools, the journal,
// per-plugin SQLite — keeps reading and writing ordinary local files with
// ordinary os.* calls and never learns that a remote store exists. That is the
// whole point of the seam being a *lifecycle* interface rather than a
// filesystem abstraction; see the package doc on objectstore for the design
// rationale and the alternative that was rejected.
//
// # Lifetime of the local working copy
//
// The local tree under core.sessions.root is NOT wiped on clean exit, and that
// is deliberate.
//
//   - On the target deployment — a container, Cloud Run, Lambda — the
//     filesystem vanishes when the process does. Wiping buys nothing there
//     except a slower shutdown and a window in which a crash mid-wipe leaves a
//     half-deleted tree that the next Boot on a warm instance would hydrate
//     around.
//   - On a durable host the local tree is a warm cache. Keeping it makes the
//     next resume of the same session free (hydrateSessionTree short-circuits
//     on a present tree) and, more importantly, it is the copy that
//     FailurePolicyDegrade explicitly falls back to. Deleting it would mean a
//     store outage during shutdown silently destroys the only good copy of the
//     session.
//   - Deleting user data is irreversible and the engine has no way to know the
//     operator is finished with it. `core.sessions.retention` already exists as
//     the operator-owned answer to "when does local session data go away";
//     inventing a second, implicit, shutdown-triggered answer would be a
//     surprise.
//
// The rejected alternative was "wipe on clean exit so the store is
// unambiguously the source of truth". It buys tidiness on hosts that do not
// need it, at the cost of destroying the fallback copy on the hosts that do.

// objectStoreFlushTimeout bounds the shutdown flush. Mirrors the journal's
// close deadline in Stop for the same reason: a wedged remote must not hang a
// process that has already finished all of its local work. Deliberately not a
// config key — an operator who wants different behaviour here is really asking
// for a different failure_policy, and every extra knob is a knob that has to be
// documented, validated and supported forever.
const objectStoreFlushTimeout = 30 * time.Second

// hydrateStagingPrefix names the temporary directory a hydration lands in
// before it is committed. Dot-prefixed so a leftover from a crashed hydration
// is visibly not a session to anything listing the sessions root.
const hydrateStagingPrefix = ".hydrating-"

// sessionObjectStore is the engine's handle on the configured backend for the
// life of one run. Installed by Boot, released by Stop.
type sessionObjectStore struct {
	// backend is never nil — openObjectStore leaves e.objectStore nil rather
	// than storing a struct with a nil backend, so every call site is a single
	// nil check on the handle.
	backend objectstore.Backend
	// cfg is the resolved block, including the injected logger. Held so the
	// failure policy and the backend name are available at the shutdown call
	// site without reaching back into Engine.Config.
	cfg objectstore.Config
}

// close releases backend-held resources.
//
// objectstore.Backend deliberately has no Close method. Adding one to the
// published interface would force every out-of-repo backend — including the
// stateless ones that hold nothing but a bucket name and an http.Client the
// runtime will collect anyway — to implement a no-op, and widening a published
// interface later is a breaking change for exactly the third-party modules the
// registry exists to support. So the engine type-asserts io.Closer instead:
// the stdlib idiom for "close it if it has something to close". A backend
// holding an SDK client, a connection pool or a background uploader implements
// io.Closer and gets a real release point; one that holds nothing implements
// nothing and costs nothing.
func (s *sessionObjectStore) close(logger *slog.Logger) {
	c, ok := s.backend.(io.Closer)
	if !ok {
		return
	}
	if err := c.Close(); err != nil {
		logger.Warn("object store: backend close failed", "backend", s.cfg.BackendName, "error", err)
	}
}

// sessionObjectKeyPrefix maps a session ID onto the object-key prefix holding
// its tree. Keys are store-relative — Config.Bucket and Config.Prefix are the
// backend's to apply — so this is the entire engine-side key scheme.
//
// The "sessions/" segment mirrors the on-disk layout (<data root>/sessions/<id>)
// and, more usefully, reserves the rest of the namespace under the operator's
// prefix for trees that are not sessions: agent- and app-scope per-plugin
// storage are the obvious future candidates. Flattening to "<id>/" would have
// been shorter and would have made that impossible to add without a migration.
func sessionObjectKeyPrefix(sessionID string) string {
	return "sessions/" + sessionID
}

// objectStoreExcluded reports whether a session-relative path must never cross
// the seam — neither hydrated down nor, once the push paths land, uploaded.
//
// session.lock is the whole reason this exists. It is an advisory lock file
// carrying the *local* PID of the process that owns the session
// (pkg/engine/session_lock.go), and Boot refuses to start when it finds one
// whose PID is still alive. Round-tripping it through the store would stamp one
// host's PID onto every subsequent resume: on a fresh container that PID is
// almost certainly free, so the lock reads as stale and gets overwritten — the
// engine would be correct by coincidence, and wrong the moment the PID happens
// to be in use. The lock describes a running process, not session state, and
// therefore has no business leaving the machine.
//
// Kept as a predicate over the session-relative path rather than an inline
// check at the hydrate call site so the turn-boundary push (E1-S4) and the
// event-driven push (E2) share exactly one definition of "never syncs".
func objectStoreExcluded(relPath string) bool {
	return filepath.ToSlash(relPath) == sessionLockFilename
}

// openObjectStore resolves the configured backend once, at the very top of
// Boot, before anything has touched the session tree.
//
// With no backend named it leaves e.objectStore nil and returns, which is what
// keeps the default path byte-identical to a build that has never heard of
// object storage: no goroutine, no handle, no branch taken anywhere below.
func (e *Engine) openObjectStore(ctx context.Context) error {
	cfg := e.Config.Core.Sessions.ObjectStore
	if !cfg.Enabled() {
		return nil
	}

	// cfg is a copy, so tagging the logger here cannot leak a non-YAML field
	// back into Engine.Config or into the session config snapshot.
	cfg.Logger = e.Logger.With("subsystem", "objectstore", "backend", cfg.BackendName)

	backend, err := objectstore.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("opening object store: %w", err)
	}
	e.objectStore = &sessionObjectStore{backend: backend, cfg: cfg}
	e.Logger.Info("object store enabled",
		"backend", cfg.BackendName,
		"bucket", cfg.Bucket,
		"prefix", cfg.Prefix,
		"failure_policy", cfg.FailurePolicy,
	)
	return nil
}

// releaseObjectStore drops the handle without flushing. Used only on the
// Boot-failure path: a boot that never opened a session has nothing worth
// persisting, and flushing there would trade a clear boot error for a slower,
// more confusing one.
func (e *Engine) releaseObjectStore() {
	store := e.objectStore
	if store == nil {
		return
	}
	e.objectStore = nil
	store.close(e.Logger)
}

// hydrateSessionTree makes the local tree for sessionID present and complete
// before any workspace is opened against it.
//
// Hydration is eager and whole-tree: when this returns nil, every subsequent
// os.* read in the engine and in every plugin behaves exactly as it would on a
// host that never left. There is deliberately no lazy or faulting read path —
// one would have to be threaded through ~60 plugins and SQLite could not use it
// at all, so "behaves like local disk" would become an aspiration instead of a
// guarantee.
//
// A no-op when no backend is configured.
func (e *Engine) hydrateSessionTree(ctx context.Context, sessionID string) error {
	store := e.objectStore
	if store == nil {
		return nil
	}
	if sessionID == "" {
		return fmt.Errorf("hydrate: empty session id")
	}

	root := ExpandPath(e.Config.Core.Sessions.Root)
	dest := filepath.Join(root, sessionID)

	// A tree already on disk is the live working copy and wins. Re-hydrating
	// over it would resurrect objects the running host has since deleted and
	// could overwrite writes that have not been pushed yet. The rejected
	// alternative — always hydrate, treating the store as authoritative — is
	// only safe if the store is guaranteed ahead of local disk, which it never
	// is while pushes are asynchronous.
	if _, err := os.Stat(dest); err == nil {
		e.Logger.Info("object store: local session tree present, skipping hydration",
			"session_id", sessionID, "dir", dest)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("hydrate: stat %s: %w", dest, err)
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("hydrate: creating sessions root %s: %w", root, err)
	}

	// Hydrate into a staging directory inside the sessions root — same
	// filesystem, so the commit below is an atomic rename — and only publish it
	// under the real session ID once the whole tree is down. A hydration that
	// dies partway therefore leaves nothing at dest, so the engine cannot
	// mistake a half-populated tree for a complete session; the partial state
	// is discarded, never silently used.
	//
	// MkdirTemp also rejects a pattern containing a path separator, which
	// incidentally stops a traversal-shaped session ID here rather than letting
	// filepath.Join resolve it somewhere outside the sessions root.
	staging, err := os.MkdirTemp(root, hydrateStagingPrefix+sessionID+"-")
	if err != nil {
		return fmt.Errorf("hydrate: staging dir for session %q: %w", sessionID, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			e.Logger.Warn("object store: discarding partial hydration failed",
				"session_id", sessionID, "dir", staging, "error", rmErr)
		}
	}()

	started := time.Now()
	if err := store.backend.Hydrate(ctx, sessionObjectKeyPrefix(sessionID), staging); err != nil {
		// Hydration failure fails the boot under *both* failure policies.
		// FailurePolicyDegrade means "keep running against the local copy", and
		// at hydrate time there is no local copy — degrading here would hand the
		// agent an empty session that looks complete and let it write a new
		// history over the top of the real one. A failed boot is recoverable;
		// that is not.
		return fmt.Errorf("hydrating session %q from object store: %w", sessionID, err)
	}

	// Scrub before committing, so an excluded object can never be observed at
	// dest even for an instant.
	if err := scrubObjectStoreExcluded(staging); err != nil {
		return fmt.Errorf("hydrating session %q: %w", sessionID, err)
	}

	empty, err := dirIsEmpty(staging)
	if err != nil {
		return fmt.Errorf("hydrating session %q: %w", sessionID, err)
	}
	if empty {
		// An unknown session ID is not an error: it is a brand-new session that
		// happens to have been named by the caller. Mint it through the ordinary
		// new-session path so it is byte-identical to a session created locally,
		// rather than committing the empty staging directory — which would give
		// a session root with no metadata/session.json and turn the next step
		// into a confusing "reading session metadata" failure.
		e.Logger.Info("object store: no objects for session, starting empty",
			"session_id", sessionID, "key_prefix", sessionObjectKeyPrefix(sessionID))
		if _, err := newSessionWorkspaceAt(root, sessionID, e.Bus); err != nil {
			return fmt.Errorf("creating empty session %q after hydration found nothing: %w", sessionID, err)
		}
		return nil
	}

	// MkdirTemp creates 0o700; the session tree is 0o755 everywhere else.
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("hydrate: setting mode on session %q: %w", sessionID, err)
	}
	if err := os.Rename(staging, dest); err != nil {
		return fmt.Errorf("hydrate: committing session %q to %s: %w", sessionID, dest, err)
	}
	committed = true

	e.Logger.Info("object store: session hydrated",
		"session_id", sessionID, "dir", dest, "duration", time.Since(started))
	return nil
}

// finalizeObjectStore flushes and releases the backend on clean shutdown.
//
// Called at the end of Stop, after every local writer has closed (plugins, the
// journal, per-plugin SQLite) so the flush barrier covers a quiesced tree.
// Returns an error only under FailurePolicyStrict, where an unpersisted session
// is worse than a failed shutdown.
func (e *Engine) finalizeObjectStore() error {
	store := e.objectStore
	if store == nil {
		return nil
	}
	e.objectStore = nil
	defer store.close(e.Logger)

	// A fresh background context with its own deadline, for the same reason the
	// journal close above uses one: hosts routinely hand Stop an
	// already-cancelled context, and the final flush is precisely the work that
	// must still happen after cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), objectStoreFlushTimeout)
	defer cancel()

	if err := store.backend.Flush(ctx); err != nil {
		err = fmt.Errorf("flushing object store on shutdown: %w", err)
		if store.cfg.FailurePolicy == objectstore.FailurePolicyStrict {
			return err
		}
		e.Logger.Warn("object store: shutdown flush failed, the stored session may be stale",
			"backend", store.cfg.BackendName, "error", err)
		return nil
	}

	e.Logger.Info("object store: flushed on shutdown", "backend", store.cfg.BackendName)
	return nil
}

// scrubObjectStoreExcluded removes anything objectStoreExcluded rejects from a
// freshly hydrated tree. Defence in depth: a well-behaved push path never
// uploads these, but the store may hold objects written by an older build, a
// different tool, or a backend that walked the tree itself.
func scrubObjectStoreExcluded(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("relativising %s: %w", path, relErr)
		}
		if !objectStoreExcluded(rel) {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("removing excluded object %s: %w", rel, rmErr)
		}
		return nil
	})
}

// dirIsEmpty reports whether dir has no entries at all. Reads a single name
// rather than the full listing so a large hydrated tree costs one syscall.
func dirIsEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", dir, err)
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", dir, err)
	}
	return false, nil
}
