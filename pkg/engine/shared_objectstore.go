package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/storage"
)

// This file extends the object-store seam past the session tree to the roots
// that deliberately live outside it.
//
// # The four roots and the one interface
//
// Nexus persists state in four places. Three of them are the session tree, the
// blob store inside it and the journal inside it, and session_objectstore.go
// already carries all three. The fourth is everything under the *data* root
// that is not a session:
//
//	<core.storage.root>/plugins/<pluginID>/store.db                    (ScopeApp)
//	<core.storage.root>/agents/<agentID>/plugins/<pluginID>/store.db   (ScopeAgent)
//
// plus eval run output, which is written once by a CLI run rather than by an
// engine and is useful to collect centrally.
//
// None of that needs a second interface. objectstore.Backend takes store-
// relative keys and knows nothing about sessions; "hydrate this prefix",
// "put this file at this key", "flush" is the whole vocabulary, and it serves a
// machine-wide SQLite database exactly as well as it serves a session
// transcript. What differs per root is *policy* — when to push, what wins on a
// collision, whether a local copy may be overwritten — and every bit of that
// policy lives in this file, on the engine side of the seam, where a backend
// author never sees it. That is FR-25 in code: no Backend method mentions a
// root, and adding these two did not change a signature.
//
// # Key layout, and why it is beside sessions/ rather than under it
//
// Keys mirror the on-disk layout beneath the data root, one segment for one
// directory:
//
//	sessions/<id>/...                             the session tree (E1-S4)
//	plugins/<pluginID>/store.db                   app scope
//	agents/<agentID>/plugins/<pluginID>/store.db  agent scope
//	eval/<run-id>/...                             eval run output
//
// E1-S4 chose the "sessions/" segment specifically to keep the rest of the
// namespace free for exactly this, and that reservation is what the layout
// above spends. Mirroring the local tree means the mapping is one line of
// filepath.Rel rather than a table, an operator browsing a bucket sees the
// directory names they already know, and E4/E5 backends re-derive nothing —
// they receive finished keys.
//
// The alternative was to nest shared state under the session that happened to
// flush it, e.g. sessions/<id>/shared/plugins/<pluginID>/store.db. That is the
// one layout that must never be used, and it is worth saying why in the file
// that would have used it. An app-scope store is machine-wide by definition:
// gates/token_budget keeps a *tenant* token ceiling there precisely so it
// spans sessions. Key it under a session and every session gets its own copy,
// every fresh host hydrates the copy belonging to whichever session it resumes,
// and the machine-wide ceiling silently becomes a per-session ceiling. Nothing
// errors; the gate simply stops doing its job. sharedStoreKey refuses to
// produce a key under sessions/ so that mistake cannot be made by accident.
//
// # Concurrency: these roots are shared, and the session tree never was
//
// A session tree has one writer by construction — the session lock enforces it.
// An app-scope store has as many writers as there are Nexus processes on the
// machine, and that is a supported (if not preferred) configuration today.
// Two cases, with different answers:
//
//   - Two processes on ONE host share the same store.db file. SQLite's WAL and
//     busy timeout serialise them, so at any instant the file holds both
//     processes' committed writes and a snapshot of it is a superset of each.
//     Both processes upload to the same key, so the later upload wins — and the
//     later upload is a strict superset of the earlier one. Safe.
//
//   - Two processes on DIFFERENT hosts each have their own local copy, and
//     there is no shared serialisation point. Both upload to the same key at
//     whole-database granularity, so the later flush silently discards the
//     other host's writes. This is genuinely last-writer-wins and it is not
//     fixable at this layer: merging two SQLite databases is a schema-specific
//     operation the engine has no basis to perform, and per-row sync would be a
//     different product.
//
// The conclusion is therefore *not* "assume a single writer". It is: a shared
// root may have one writing host at a time, the same constraint the local
// filesystem already implies, and it is documented in
// docs/src/architecture/storage.md rather than left for an operator to
// discover. Two mitigations are real: hydration never overwrites a plugin
// directory that already exists locally (see hydrateSharedPluginStores), so a
// remote copy can never clobber a live local database mid-run; and the
// snapshot is taken with the same checkpoint-then-VACUUM INTO discipline as the
// session tree, so what lands remotely is always a self-consistent database
// rather than a torn one. Split-brain *detection* (session_owner.go) is scoped to
// sessions and deliberately stops there. "Who owns this root" is a different
// question from "who owns this session": a shared root is machine-wide and
// outlives every session, so its holder is not one engine run and its marker
// could not be claimed on Boot and released on Stop the way a session's is.
// Extending detection here is what would turn the one-writing-host rule from a
// documented constraint into a detected violation, and it needs its own answer
// to that lifetime question rather than a reuse of this one.

const (
	// sessionsKeyRoot is the first key segment of every session tree. Named
	// so the guard in sharedStoreKey and the session prefix cannot drift.
	sessionsKeyRoot = "sessions"

	// appStoreKeyRoot and agentStoreKeyRoot are the first key segments of the
	// two shared plugin-store roots. They match the directory names
	// storage.Manager.dirFor uses, which is not a coincidence: the key is
	// derived from the live path by filepath.Rel, so these constants only
	// exist to be asserted against, not to build paths with.
	appStoreKeyRoot   = "plugins"
	agentStoreKeyRoot = "agents"

	// evalKeyRoot is the first key segment of eval run output.
	evalKeyRoot = "eval"
)

// EvalObjectKeyPrefix maps an eval run ID onto the key prefix holding its
// report. Exported because the caller is cmd/nexus rather than the engine: an
// eval run is a CLI invocation that writes its output once and exits, so there
// is no session, no turn boundary and no engine lifecycle to hang it off.
//
// Kept here beside the other roots rather than in cmd/nexus so the whole key
// namespace is visible in one file. A second root scheme invented in a command
// is how a bucket ends up with two layouts.
func EvalObjectKeyPrefix(runID string) string { return evalKeyRoot + "/" + runID }

// sharedStoreKey maps a live store.db path onto its object key, by
// relativising it against the data root.
//
// Derived rather than composed on purpose. Composing the key from scope and
// plugin ID would mean this file holding a second copy of
// storage.Manager.dirFor's path rules, free to disagree with it after any
// change to either; relativising the path the manager actually opened means
// the key follows the layout by construction. The cost is that a handle whose
// file is somehow outside the data root has no key, which is an error rather
// than a guess.
func sharedStoreKey(dataRoot string, livePath string) (string, error) {
	rel, err := filepath.Rel(dataRoot, livePath)
	if err != nil {
		return "", fmt.Errorf("relativising %s against %s: %w", livePath, dataRoot, err)
	}
	key := filepath.ToSlash(rel)
	if key == ".." || strings.HasPrefix(key, "../") {
		return "", fmt.Errorf("store %s is outside the data root %s", livePath, dataRoot)
	}
	if err := objectstore.ValidateKey(key); err != nil {
		return "", err
	}
	// The lifetime invariant, enforced rather than documented. A shared store
	// under sessions/ would be hydrated and snapshotted per session, turning a
	// machine-wide ceiling into a per-session one with nothing to notice it.
	// Only reachable if someone points core.storage.root inside the sessions
	// root, which is legal on disk and wrong here.
	if key == sessionsKeyRoot || strings.HasPrefix(key, sessionsKeyRoot+"/") {
		return "", fmt.Errorf("store %s would be keyed under %q, which would make a cross-session store per-session",
			livePath, sessionsKeyRoot)
	}
	return key, nil
}

// sharedStoreKeyPrefixes returns the key prefixes holding this engine's shared
// plugin stores, paired with the local directory each hydrates into.
//
// Agent scope is included only when core.agent_id is set. That mirrors
// storage.Manager, which collapses ScopeAgent onto ScopeApp for an engine with
// no agent ID so a single-agent embedder does not end up with two connection
// pools on one file — plugins/vectorstore/sqlite_fts relies on exactly that
// fallback. Listing agents/<empty>/plugins would be a malformed key, and
// snapshotting both scopes on a collapsed engine would upload the same handles
// twice under the same key.
func (e *Engine) sharedStoreKeyPrefixes() []struct {
	keyPrefix string
	destDir   string
} {
	root := storageRoot(e.Config)
	out := []struct {
		keyPrefix string
		destDir   string
	}{
		{keyPrefix: appStoreKeyRoot, destDir: filepath.Join(root, appStoreKeyRoot)},
	}
	if id := e.Config.Core.AgentID; id != "" {
		out = append(out, struct {
			keyPrefix string
			destDir   string
		}{
			keyPrefix: agentStoreKeyRoot + "/" + id + "/" + appStoreKeyRoot,
			destDir:   filepath.Join(root, agentStoreKeyRoot, id, appStoreKeyRoot),
		})
	}
	return out
}

// hydrateSharedRoots pulls app- and agent-scope plugin stores down before any
// plugin can open one.
//
// Called from Boot immediately after the backend is resolved and well before
// plugin Init, because storage.Manager.Open creates the directory as a side
// effect of handing out a handle: hydrating afterwards would find every
// directory already present and skip all of them.
//
// A no-op when no backend is configured.
func (e *Engine) hydrateSharedRoots(ctx context.Context) error {
	store := e.objectStore
	if store == nil {
		return nil
	}
	for _, root := range e.sharedStoreKeyPrefixes() {
		if err := objectstore.ValidateKeyPrefix(root.keyPrefix); err != nil {
			// Only reachable from a hostile core.agent_id ("../..", a path
			// separator). Refusing the boot is right: silently dropping agent
			// scope would start the agent against an empty store and then
			// overwrite the good remote copy at the first turn boundary.
			return fmt.Errorf("hydrate: core.agent_id makes an invalid object key: %w", err)
		}
		if err := e.hydrateSharedPluginStores(ctx, store, root.keyPrefix, root.destDir); err != nil {
			return fmt.Errorf("hydrating shared plugin stores at %q: %w", root.keyPrefix, err)
		}
	}
	return nil
}

// hydrateSharedPluginStores hydrates each per-plugin directory under keyPrefix
// that is not already present locally.
//
// # Why per plugin directory rather than per root or per file
//
// Backend.Hydrate documents that it *overwrites* existing files, which is the
// right behaviour for a session tree landing on an empty host and precisely the
// wrong behaviour here. A shared store may be open, in this process or another
// one on the same machine, and replacing the file under a live SQLite handle
// does not produce a stale database — it produces a corrupt one. So a directory
// that exists locally is never touched, on the same "the local copy is the live
// working copy and wins" rule hydrateSessionTree applies to a session tree.
//
// Whole-root granularity ("if <root>/plugins exists, skip everything") was the
// first version and is too coarse: that directory exists after the first ever
// run on a durable host, so a plugin whose store only exists remotely would
// never arrive. Per-file granularity is too fine in the other direction — it
// would mix a remote store.db with a local -wal that describes different
// content. A plugin directory is the unit storage.Manager creates and opens,
// so it is the unit with a meaningful "present locally" answer.
//
// # Why a listing failure fails the boot
//
// The same argument hydrateSessionTree makes. Under FailurePolicyDegrade the
// engine keeps running against the local copy — but if the listing failed there
// may be no local copy, and carrying on hands plugins an empty machine-wide
// store that the first turn boundary then uploads over the good one. A failed
// boot is recoverable; that is not.
func (e *Engine) hydrateSharedPluginStores(ctx context.Context, store *sessionObjectStore, keyPrefix string, destDir string) error {
	objects, err := store.backend.List(ctx, keyPrefix)
	if err != nil {
		return fmt.Errorf("listing: %w", err)
	}
	if len(objects) == 0 {
		return nil
	}

	// Collect the distinct plugin directories the store holds. Sorted so a log
	// of a cold boot reads the same way twice.
	seen := make(map[string]bool, len(objects))
	for _, obj := range objects {
		rel, ok := objectstore.TrimKeyPrefix(obj.Key, keyPrefix)
		if !ok {
			continue
		}
		pluginID, _, nested := strings.Cut(rel, "/")
		if !nested || pluginID == "" {
			// An object directly at <keyPrefix>/<name> is not a plugin
			// directory. Nothing in Nexus writes one; ignoring it is safer
			// than inventing a directory for it.
			continue
		}
		seen[pluginID] = true
	}
	pluginIDs := make([]string, 0, len(seen))
	for id := range seen {
		pluginIDs = append(pluginIDs, id)
	}
	sort.Strings(pluginIDs)

	for _, pluginID := range pluginIDs {
		prefix := keyPrefix + "/" + pluginID
		// Validate the composed prefix rather than the bare segment: a ".",
		// ".." or separator arriving in a remote key would otherwise become a
		// filepath.Join out of the data root.
		if err := objectstore.ValidateKey(prefix); err != nil {
			e.Logger.Warn("object store: skipping shared store with an invalid key",
				"key_prefix", keyPrefix, "plugin", pluginID, "error", err)
			continue
		}

		dest := filepath.Join(destDir, pluginID)
		if _, err := os.Stat(dest); err == nil {
			e.Logger.Debug("object store: shared plugin store present locally, skipping hydration",
				"plugin", pluginID, "dir", dest)
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", dest, err)
		}

		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", destDir, err)
		}
		if err := e.hydrateOneSharedStore(ctx, store, prefix, destDir, pluginID, dest); err != nil {
			return err
		}
	}
	return nil
}

// hydrateOneSharedStore lands one plugin's directory through a staging
// directory and an atomic rename, so a hydration that dies partway leaves
// nothing at dest and the next boot retries from scratch instead of opening a
// half-populated database.
func (e *Engine) hydrateOneSharedStore(ctx context.Context, store *sessionObjectStore, keyPrefix, destDir, pluginID, dest string) error {
	// MkdirTemp rejects a pattern containing a path separator, which is a
	// second line of defence against a traversal-shaped plugin ID reaching
	// filepath.Join above.
	staging, err := os.MkdirTemp(destDir, hydrateStagingPrefix+pluginID+"-")
	if err != nil {
		return fmt.Errorf("staging dir for %q: %w", pluginID, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			e.Logger.Warn("object store: discarding partial shared-store hydration failed",
				"plugin", pluginID, "dir", staging, "error", rmErr)
		}
	}()

	started := time.Now()
	if err := store.backend.Hydrate(ctx, keyPrefix, staging); err != nil {
		return fmt.Errorf("hydrating %q: %w", keyPrefix, err)
	}
	// The -wal/-shm sidecars describe the machine that produced them; a stale
	// one beside a fresh store.db is how a database gets rolled back to a state
	// neither file ever held. Same scrub the session tree gets.
	if err := scrubObjectStoreExcluded(staging); err != nil {
		return fmt.Errorf("hydrating %q: %w", keyPrefix, err)
	}
	// MkdirTemp creates 0o700; storage.Manager creates its directories 0o755.
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("setting mode on %q: %w", pluginID, err)
	}

	if err := os.Rename(staging, dest); err != nil {
		// Another process on this host may have created the directory between
		// the Stat above and here. Its copy is the live one — it may already
		// have an open handle on it — so it wins and this hydration is
		// discarded, exactly as if the Stat had seen it.
		if _, statErr := os.Stat(dest); statErr == nil {
			e.Logger.Info("object store: shared plugin store appeared locally during hydration, keeping the local copy",
				"plugin", pluginID, "dir", dest)
			return nil
		}
		return fmt.Errorf("committing %q to %s: %w", pluginID, dest, err)
	}
	committed = true

	e.Logger.Info("object store: shared plugin store hydrated",
		"plugin", pluginID, "key_prefix", keyPrefix, "dir", dest, "duration", time.Since(started))
	return nil
}

// sharedSnapshotStats is the shared-root half of a turn-boundary snapshot's
// cost measurement, kept separate from the session numbers because it is paid
// against a different root with a different lifetime.
type sharedSnapshotStats struct {
	Objects    int
	Bytes      int64
	DBDuration time.Duration
}

// snapshotSharedStores checkpoints and uploads every open app- and agent-scope
// store.db.
//
// # Why the same turn boundary as the session tree
//
// "A turn is the unit a session can afford to lose" applies verbatim to a
// tenant token ceiling: a kill that loses the last turn's token accounting
// leaves the ceiling permanently under-counting. Snapshotting only on shutdown
// would be cheaper and would lose all of it on a hard kill, which is the
// failure mode this whole seam exists to remove.
//
// The cost is real and is the same shape E1-S4 accepted for session-scope
// SQLite: O(database size) per turn, per open handle, regardless of how little
// changed. It is logged separately (shared_objects / shared_bytes /
// shared_db_duration) rather than folded into the session numbers, because
// "the agent-scope FTS index got big and turns got slow" is a different
// diagnosis from "the session got big". Skipping an unchanged database via the
// SQLite header change counter is a real optimisation and is deliberately not
// here: it needs a memo that is only safe to advance after a successful Flush,
// which is a self-contained change worth making on its own evidence.
//
// # Ordering and failure
//
// Runs after the session commit marker is published, so a shared-root outage
// can never hold back a session that is otherwise fully durable. Its error is
// still the snapshot's error, which under FailurePolicyStrict raises core.error
// and marks session.snapshot.result not-OK: a turn boundary is meant to persist
// everything the engine owns, and reporting partial success as success is how
// an operator finds out too late. The already-published session marker stays
// correct — the session genuinely is durable; only the shared state is stale.
func (e *Engine) snapshotSharedStores(ctx context.Context, store *sessionObjectStore, stage string) (stats sharedSnapshotStats, err error) {
	if e.Storage == nil {
		return stats, nil
	}
	dataRoot := storageRoot(e.Config)

	scopes := []storage.Scope{storage.ScopeApp}
	if e.Config.Core.AgentID != "" {
		// See sharedStoreKeyPrefixes: on an engine with no agent ID the
		// manager files agent-scope handles under ScopeApp, so asking for both
		// would upload the same databases twice.
		scopes = append(scopes, storage.ScopeAgent)
	}

	for _, scope := range scopes {
		dbStarted := time.Now()
		snaps, snapErr := e.Storage.Snapshot(scope, filepath.Join(stage, "shared", scope.String()))
		stats.DBDuration += time.Since(dbStarted)
		if snapErr != nil {
			return stats, fmt.Errorf("snapshot: %w", snapErr)
		}
		for _, snap := range snaps {
			key, keyErr := sharedStoreKey(dataRoot, snap.LivePath)
			if keyErr != nil {
				// Not fatal: a handle the key scheme cannot express is a
				// configuration the seam does not cover, and failing every
				// turn boundary over it would be worse than saying so once per
				// snapshot and persisting everything else.
				e.Logger.Warn("object store: shared store has no object key, not snapshotted",
					"plugin", snap.PluginID, "scope", scope.String(), "path", snap.LivePath, "error", keyErr)
				continue
			}
			if snap.Checkpoint.Busy {
				e.Logger.Warn("object store: WAL checkpoint was busy before shared-store snapshot",
					"plugin", snap.PluginID, "scope", scope.String(), "path", snap.LivePath)
			}
			if err := store.backend.Put(ctx, key, snap.Path); err != nil {
				return stats, fmt.Errorf("snapshot: uploading shared store %s: %w", key, err)
			}
			stats.Objects++
			stats.Bytes += snap.Bytes
		}
	}

	if stats.Objects == 0 {
		// No plugin at these scopes: no upload, so nothing to make durable.
		// Skipping the barrier keeps an engine with no shared state paying
		// nothing at all for this path.
		return stats, nil
	}
	if err := store.backend.Flush(ctx); err != nil {
		return stats, fmt.Errorf("snapshot: flushing shared stores: %w", err)
	}
	return stats, nil
}

// PublishTree uploads every regular file under localDir to keyPrefix and
// flushes, opening and closing the backend around the call.
//
// This is the seam's entry point for output that is produced outside an engine
// run — eval reports are the reason it exists. An eval run is a CLI invocation
// that writes a report directory once and exits: there is no session to hydrate
// into, no turn boundary to hang a snapshot off, and no Engine at all, so it
// gets a function rather than a lifecycle hook. It is still the same Backend
// with the same key rules, which is what keeps "one interface serves all four
// roots" true rather than aspirational.
//
// skip is called with each entry's slash-separated path relative to localDir
// and reports whether to leave it out; a nil skip includes everything.
// Returning true for a directory prunes it.
//
// A disabled cfg is a no-op returning zero counts and no error, so a caller
// does not have to branch on config internals.
func PublishTree(ctx context.Context, cfg objectstore.Config, keyPrefix string, localDir string, skip func(rel string, isDir bool) bool) (objects int, bytes int64, err error) {
	if !cfg.Enabled() {
		return 0, 0, nil
	}
	if err := objectstore.ValidateKeyPrefix(keyPrefix); err != nil {
		return 0, 0, err
	}

	backend, err := objectstore.Open(ctx, cfg)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if c, ok := backend.(io.Closer); ok {
			if closeErr := c.Close(); closeErr != nil && err == nil {
				err = fmt.Errorf("closing object-store backend %q: %w", cfg.BackendName, closeErr)
			}
		}
	}()

	type upload struct {
		key string
		src string
	}
	var uploads []upload
	walkErr := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(localDir, path)
		if relErr != nil {
			return fmt.Errorf("relativising %s: %w", path, relErr)
		}
		if rel == "." {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if skip != nil && skip(slash, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		key := slash
		if keyPrefix != "" {
			key = keyPrefix + "/" + slash
		}
		if keyErr := objectstore.ValidateKey(key); keyErr != nil {
			return keyErr
		}
		uploads = append(uploads, upload{key: key, src: path})
		return nil
	})
	if walkErr != nil {
		return 0, 0, fmt.Errorf("walking %s: %w", localDir, walkErr)
	}

	// Sorted so an interrupted publish leaves a bucket listing that reads the
	// same way twice, the same reason the session snapshot sorts.
	sort.Slice(uploads, func(i, j int) bool { return uploads[i].key < uploads[j].key })

	for _, u := range uploads {
		info, statErr := os.Stat(u.src)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return objects, bytes, fmt.Errorf("stat %s: %w", u.src, statErr)
		}
		if putErr := backend.Put(ctx, u.key, u.src); putErr != nil {
			return objects, bytes, fmt.Errorf("uploading %s: %w", u.key, putErr)
		}
		objects++
		bytes += info.Size()
	}

	if flushErr := backend.Flush(ctx); flushErr != nil {
		return objects, bytes, fmt.Errorf("flushing: %w", flushErr)
	}
	return objects, bytes, nil
}
