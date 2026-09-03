package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"
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

	// snapshotMu serialises whole-tree snapshots. Two snapshots interleaving
	// would race on the commit marker and could publish a marker describing a
	// snapshot that the other one had already half-overwritten. Nested agent
	// loops make concurrent agent.turn.end dispatches entirely plausible, so
	// this is not theoretical.
	snapshotMu sync.Mutex
	// snapshotSeq counts snapshots for this run, starting at 1. Carried in
	// the commit marker and the result event so an operator can line a remote
	// marker up against a log line.
	snapshotSeq uint64
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
	p := filepath.ToSlash(relPath)
	if p == sessionLockFilename {
		return true
	}
	// SQLite sidecars are the second thing that describes a machine rather
	// than a session. store.db-wal holds committed frames not yet folded into
	// the main database and store.db-shm is a process-local index into it;
	// both are meaningless beside a database file that was checkpointed and
	// snapshotted separately (see pkg/engine/storage/snapshot.go), and
	// hydrating a stale -wal next to a fresh store.db is how a database gets
	// silently rolled back to a state neither file ever held. -journal covers
	// the rollback-mode equivalent for any handle not opened in WAL mode.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if strings.HasSuffix(p, ".db"+suffix) {
			return true
		}
	}
	return false
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

// ---------------------------------------------------------------------------
// Turn-boundary snapshots
// ---------------------------------------------------------------------------

// The durability half of the seam. Hydration (above) makes a session present;
// this makes it survivable.
//
// # Why the turn boundary, and why agent.turn.end
//
// A turn is the unit a session can afford to lose. Snapshotting more often
// costs a whole-tree upload per LLM call; snapshotting less often means a
// killed container throws away work the user watched happen. agent.turn.end is
// already the engine's definition of a turn boundary — the journal fsyncs on
// it, rotates on it, and the turn counter and timing journal are driven by it
// (see Boot) — so hanging the snapshot off anything else would invent a second,
// disagreeing notion of "turn".
//
// Crucially the subscription lives *here*, in core. No plugin implements an
// interface, calls a method, or knows the object store exists; an agent loop
// emits the same event it already emitted before this file was written.
// session.snapshot.request is the escape hatch for the two things the turn
// boundary genuinely does not cover — an embedder driving the engine outside an
// agent loop, and a custom agent that emits no turn events.
//
// The handler is a wildcard filtered to agent.turn.end rather than a typed
// subscription, so that it runs after the journal has been handed the boundary
// envelope. installObjectStoreSnapshots carries the full argument; it is the
// difference between a resumed session continuing and a resumed session re-running
// its last completed turn.
//
// # Why synchronous
//
// The handler blocks the goroutine that emitted agent.turn.end for the whole
// upload. Doing it in a background goroutine was considered and rejected: it
// would report a turn complete while its state was still in flight, which is
// precisely the guarantee ("a hard kill loses at most the in-flight turn") the
// snapshot exists to provide. The cost is real and is logged on every snapshot
// — objects, bytes and duration — because it is O(tree size) per turn and the
// tree only grows.
//
// # What "never replaces a good remote copy" means here
//
// Object stores have no multi-object transaction, so a tree spread over N
// objects cannot be replaced atomically. Three things together give the
// property anyway:
//
//  1. Nothing is uploaded from a file that could be torn. The two subsystems
//     that rewrite themselves under a reader — per-plugin SQLite and the
//     journal's active segment — are staged first, into a directory outside the
//     session tree, and uploaded from there.
//  2. A snapshot never deletes. It adds and overwrites only, so a failure can
//     never remove remote state it did not successfully replace.
//  3. A commit marker at sessions/<id>.snapshot.json is written and flushed
//     only after every other object is durable. The marker therefore only ever
//     advances past a complete snapshot: a failed or half-finished upload
//     leaves it naming the previous one, which is the snapshot that is
//     guaranteed restorable.
//
// The marker is a sibling key, not a member of the tree, which is why
// objectstore.TrimKeyPrefix matches whole segments: sessions/<id>.snapshot.json
// is deliberately *not* under prefix sessions/<id>, so it never hydrates back
// into the session and never becomes an input to the next snapshot.
//
// The rejected alternative was generation directories — write the whole tree
// under sessions/<id>/gen-<n>/ and flip a pointer. That gives true atomic
// replace, and costs a second full copy of every session in the bucket plus a
// key scheme hydration would have to resolve through an indirection on every
// boot. The marker buys the same "is the remote state complete?" answer for one
// small object.

const (
	// objectStoreSnapshotTimeout bounds one whole-tree snapshot. Generous
	// because the snapshot is O(tree size) over a network, and tight enough
	// that a wedged remote cannot hang an interactive agent indefinitely.
	// Not a config key, for the same reason objectStoreFlushTimeout is not:
	// an operator who wants different behaviour here wants a different
	// failure_policy.
	objectStoreSnapshotTimeout = 2 * time.Minute

	// snapshotStagingPrefix names the directory holding the staged copies of
	// files that cannot be uploaded in place. Dot-prefixed and rooted in the
	// sessions root, exactly like hydrateStagingPrefix, so a leftover from a
	// killed snapshot is visibly not a session.
	snapshotStagingPrefix = ".snapshot-"

	snapshotTriggerTurn     = "turn"
	snapshotTriggerShutdown = "shutdown"
	snapshotTriggerRequest  = "request"

	// sessionSnapshotMarkerSuffix turns a session key prefix into the key of
	// its commit marker.
	sessionSnapshotMarkerSuffix = ".snapshot.json"

	// sessionSnapshotMarkerVersion is the marker's own on-disk format version,
	// kept separate from the bus event so an operator reading the bucket can
	// tell what they are looking at without a Nexus build to hand.
	sessionSnapshotMarkerVersion = 1
)

// sessionSnapshotMarkerKey is the commit-record key for a session. See the
// block comment above for why it is a sibling of the tree rather than a member
// of it.
func sessionSnapshotMarkerKey(sessionID string) string {
	return sessionObjectKeyPrefix(sessionID) + sessionSnapshotMarkerSuffix
}

// sessionSnapshotMarker is the JSON body of the commit record. Deliberately
// small and deliberately without a file listing: it is written on every
// snapshot, so anything proportional to the tree would make the marker itself a
// cost that grows with the session.
type sessionSnapshotMarker struct {
	SchemaVersion int       `json:"_schema_version"`
	SessionID     string    `json:"session_id"`
	KeyPrefix     string    `json:"key_prefix"`
	Sequence      uint64    `json:"sequence"`
	Trigger       string    `json:"trigger"`
	TurnID        string    `json:"turn_id,omitempty"`
	CompletedAt   time.Time `json:"completed_at"`
	Objects       int       `json:"objects"`
	Bytes         int64     `json:"bytes"`
}

// snapshotRequest is what one snapshot needs to know about its caller.
//
// The journal writer is passed in rather than read from e.Journal because the
// shutdown snapshot runs after Stop has closed and cleared it, and a closed
// writer is still perfectly snapshottable — the segments are on disk and
// nothing is going to rotate them.
type snapshotRequest struct {
	trigger string
	turnID  string
	reason  string
	journal *journal.Writer
}

// snapshotEntry pairs a session-relative path with the local file the bytes
// come from. The two differ for anything that had to be staged.
type snapshotEntry struct {
	rel string
	src string
}

// snapshotStats is what the snapshot reports back — the cost measurement.
type snapshotStats struct {
	Sequence uint64
	Objects  int
	Bytes    int64
	// DBBytes and DBDuration isolate the per-plugin SQLite share of the
	// snapshot, which is the part that is O(database size) on every single
	// turn regardless of how little changed.
	DBBytes    int64
	DBDuration time.Duration
	Duration   time.Duration
}

// installObjectStoreSnapshots subscribes the turn-boundary and on-request
// snapshot handlers. Called from Boot, after startJournal — see below for why
// the order is not incidental.
//
// Nothing is subscribed when no backend is configured, so the default build
// does not merely skip the work: it never enters the dispatch table, and
// agent.turn.end costs exactly what it cost before this file existed.
func (e *Engine) installObjectStoreSnapshots() {
	if e.objectStore == nil {
		return
	}
	if e.Journal == nil {
		// Only reachable if this is ever called before startJournal. The
		// snapshot still works — it just cannot capture a journal it has no
		// handle on — but the ordering argument below no longer holds, so say
		// so rather than degrading silently.
		e.Logger.Warn("object store: installing snapshots with no journal writer; " +
			"turn snapshots will not include the journal")
	}

	// A wildcard subscription filtered to agent.turn.end, NOT a typed
	// subscription on it. That is a correctness requirement, not a style
	// choice.
	//
	// EmitEvent runs every typed handler before any wildcard, and the journal
	// is itself a wildcard, installed by startJournal earlier in Boot. A typed
	// handler here would therefore run before the agent.turn.end envelope had
	// even been handed to the journal, and would snapshot a journal ending one
	// envelope short of the exact turn boundary it is reacting to. The
	// consequence is not subtle: a journal whose last turn has no agent.turn.end
	// is what journal.Coordinator.IsPartialTurn calls an unfinished turn, so
	// every resume from that snapshot would re-fire the last input and re-run a
	// turn that had already completed.
	//
	// Registering after the journal's wildcard puts this handler after it in
	// dispatch order — wildcards run in subscription order — so the envelope is
	// already queued by the time Barrier waits for it. It also means rotation
	// for this turn has finished before the journal is captured.
	//
	// The cost is a string comparison per dispatched event, paid only when a
	// backend is configured.
	e.runUnsubs = append(e.runUnsubs, e.Bus.SubscribeAll(func(ev Event[any]) {
		if ev.Type != "agent.turn.end" {
			return
		}
		info, _ := ev.Payload.(events.TurnInfo)
		e.runSessionSnapshot(snapshotRequest{
			trigger: snapshotTriggerTurn,
			turnID:  info.TurnID,
			journal: e.Journal,
		})
	}))

	// An explicit request has no ordering constraint — the request event is not
	// a turn boundary and nothing reads it back — so it stays a plain typed
	// subscription.
	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("session.snapshot.request", func(ev Event[any]) {
		req, _ := ev.Payload.(events.SessionSnapshotRequest)
		e.runSessionSnapshot(snapshotRequest{
			trigger: snapshotTriggerRequest,
			reason:  req.Reason,
			journal: e.Journal,
		})
	}, WithSource("nexus.engine.objectstore")))
}

// runSessionSnapshot performs one snapshot and reports the outcome on the bus,
// applying the configured failure policy.
//
// It never returns an error: every caller is a bus handler or a shutdown step
// that has nothing useful to do with one. The outcome travels as a
// session.snapshot.result event, and under FailurePolicyStrict also as a
// core.error, so a subscriber that cares about an unpersisted turn can see it.
func (e *Engine) runSessionSnapshot(req snapshotRequest) {
	store := e.objectStore
	if store == nil || e.Session == nil {
		return
	}

	// A fresh context with its own deadline: the shutdown caller routinely has
	// an already-cancelled one, and the final snapshot is precisely the work
	// that must still happen after cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), objectStoreSnapshotTimeout)
	defer cancel()

	stats, err := e.snapshotSessionTree(ctx, store, req)

	result := events.SessionSnapshotResult{
		SchemaVersion: events.SessionSnapshotResultVersion,
		SessionID:     e.Session.ID,
		Trigger:       req.trigger,
		Sequence:      stats.Sequence,
		TurnID:        req.turnID,
		Objects:       stats.Objects,
		Bytes:         stats.Bytes,
		DurationMs:    float64(stats.Duration.Microseconds()) / 1000,
		OK:            err == nil,
	}

	if err != nil {
		result.ErrorMessage = err.Error()
		if store.cfg.FailurePolicy == objectstore.FailurePolicyStrict {
			// Strict means an unpersisted turn is worse than a failed one.
			// The engine cannot un-run a turn that already completed, so the
			// strongest honest signal is a loud, non-fatal error carrying the
			// backend name — which is what an operator needs to decide whether
			// to stop the run.
			_ = e.Bus.Emit("core.error", events.ErrorInfo{
				SchemaVersion: events.ErrorInfoVersion,
				Source:        "nexus.engine.objectstore",
				Err:           err,
			})
			e.Logger.Error("object store: session snapshot failed",
				"backend", store.cfg.BackendName, "trigger", req.trigger, "error", err)
		} else {
			e.Logger.Warn("object store: session snapshot failed, the stored session is stale",
				"backend", store.cfg.BackendName, "trigger", req.trigger, "error", err)
		}
	} else {
		// The cost measurement. Logged on every snapshot rather than sampled,
		// because "the database got big and turns got slow" is exactly the
		// correlation an operator needs and it is invisible in aggregate.
		e.Logger.Info("object store: session snapshot",
			"session_id", e.Session.ID,
			"trigger", req.trigger,
			"reason", req.reason,
			"sequence", stats.Sequence,
			"objects", stats.Objects,
			"bytes", stats.Bytes,
			"db_bytes", stats.DBBytes,
			"db_duration", stats.DBDuration,
			"duration", stats.Duration,
		)
	}

	_ = e.Bus.Emit("session.snapshot.result", result)
}

// snapshotSessionTree uploads the whole session tree and publishes the commit
// marker. See the block comment above for the durability argument.
// Named results are load-bearing: the deferred timer below writes to stats, and
// with unnamed results that write would land on a copy the caller never sees —
// silently reporting every snapshot as taking zero time, which is precisely the
// number this whole path exists to surface.
func (e *Engine) snapshotSessionTree(ctx context.Context, store *sessionObjectStore, req snapshotRequest) (stats snapshotStats, err error) {
	store.snapshotMu.Lock()
	defer store.snapshotMu.Unlock()

	store.snapshotSeq++
	stats.Sequence = store.snapshotSeq

	started := time.Now()
	defer func() { stats.Duration = time.Since(started) }()

	sessionRoot := e.Session.RootDir
	sessionsRoot := ExpandPath(e.Config.Core.Sessions.Root)

	// Staging lives beside the session, not inside it: a staging directory in
	// the tree would be walked and uploaded as session data, and would have to
	// be excluded by name forever after.
	stage, err := os.MkdirTemp(sessionsRoot, snapshotStagingPrefix+e.Session.ID+"-")
	if err != nil {
		return stats, fmt.Errorf("snapshot: staging dir for session %q: %w", e.Session.ID, err)
	}
	defer func() {
		if rmErr := os.RemoveAll(stage); rmErr != nil {
			e.Logger.Warn("object store: removing snapshot staging dir failed",
				"session_id", e.Session.ID, "dir", stage, "error", rmErr)
		}
	}()

	// staged maps a session-relative path to the file the bytes must come
	// from. Every entry in it also tells the tree walk below to leave the live
	// file alone.
	staged := make(map[string]string)

	// --- the journal ---------------------------------------------------
	//
	// Barrier first: appends are asynchronous, so without it this handler
	// would snapshot a journal that stops a few envelopes short of the very
	// turn boundary that triggered it, and every resume would re-fire its last
	// input. Then capture a consistent instant, which is what keeps a
	// concurrent rotation from dropping or duplicating a segment.
	journalCaptured := false
	if req.journal != nil {
		if err := req.journal.Barrier(ctx); err != nil {
			return stats, fmt.Errorf("snapshot: draining journal: %w", err)
		}
		files, err := req.journal.Snapshot(filepath.Join(stage, "journal"))
		if err != nil {
			return stats, fmt.Errorf("snapshot: capturing journal: %w", err)
		}
		for _, f := range files {
			staged["journal/"+f.Name] = f.Path
		}
		journalCaptured = true
	}

	// --- per-plugin SQLite ---------------------------------------------
	//
	// Checkpoint then VACUUM INTO, per handle. This is the only way the
	// uploaded store.db is restorable without its sidecars, and the only way
	// a plugin committing mid-snapshot cannot tear the upload.
	if e.Storage != nil {
		dbStarted := time.Now()
		snaps, err := e.Storage.Snapshot(storage.ScopeSession, filepath.Join(stage, "storage"))
		if err != nil {
			return stats, fmt.Errorf("snapshot: %w", err)
		}
		stats.DBDuration = time.Since(dbStarted)
		for _, snap := range snaps {
			rel, relErr := filepath.Rel(sessionRoot, snap.LivePath)
			if relErr != nil || strings.HasPrefix(rel, "..") {
				// A session-scope handle whose file is not under this session
				// tree should be impossible; skipping rather than failing
				// keeps a future scope reshuffle from breaking snapshots.
				e.Logger.Warn("object store: session-scope store.db outside the session tree, not snapshotted",
					"plugin", snap.PluginID, "path", snap.LivePath)
				continue
			}
			staged[filepath.ToSlash(rel)] = snap.Path
			stats.DBBytes += snap.Bytes
			if snap.Checkpoint.Busy {
				// Not fatal — VACUUM INTO still produced a consistent copy —
				// but it means the *local* database file is still carrying a
				// WAL, which is worth knowing when a resume looks stale.
				e.Logger.Warn("object store: WAL checkpoint was busy before snapshot",
					"plugin", snap.PluginID, "path", snap.LivePath)
			}
		}
	}

	// --- the rest of the tree ------------------------------------------
	entries := make([]snapshotEntry, 0, len(staged)+32)
	walkErr := filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(sessionRoot, path)
		if relErr != nil {
			return fmt.Errorf("relativising %s: %w", path, relErr)
		}
		slash := filepath.ToSlash(rel)
		if objectStoreExcluded(slash) {
			return nil
		}
		if _, ok := staged[slash]; ok {
			return nil
		}
		// Top-level journal files are described entirely by the captured
		// instant. Anything the walk finds there that the instant did not name
		// appeared after the capture — a segment a rotation has just written —
		// and uploading it now would put the same events in the bucket twice,
		// once in the new .zst and once in the active segment we staged.
		// journal/cache/ is ordinary data and is deliberately still walked.
		if journalCaptured && isJournalSegmentPath(slash) {
			return nil
		}
		entries = append(entries, snapshotEntry{rel: slash, src: path})
		return nil
	})
	if walkErr != nil {
		return stats, fmt.Errorf("snapshot: walking session tree: %w", walkErr)
	}
	for rel, src := range staged {
		entries = append(entries, snapshotEntry{rel: rel, src: src})
	}

	// Sorted so the upload order is reproducible across runs and platforms,
	// which is what makes a failure diagnosable from a bucket listing. It also
	// happens to put "journal/events-NNN.jsonl.zst" ahead of
	// "journal/events.jsonl" ('-' sorts before '.'), so an interrupted
	// snapshot leaves the bucket with a duplicated segment rather than a lost
	// one — the recoverable half of the two. The commit marker is what makes
	// that state detectable; this only decides which way it fails.
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	prefix := sessionObjectKeyPrefix(e.Session.ID)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return stats, fmt.Errorf("snapshot: %w", err)
		}
		info, statErr := os.Stat(entry.src)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				// The tree is live. A file that vanished between the walk and
				// here (a tool cleaning up after itself, a cache sweep) is not
				// a snapshot failure.
				continue
			}
			return stats, fmt.Errorf("snapshot: stat %s: %w", entry.rel, statErr)
		}
		if err := store.backend.Put(ctx, prefix+"/"+entry.rel, entry.src); err != nil {
			return stats, fmt.Errorf("snapshot: uploading %s: %w", entry.rel, err)
		}
		stats.Objects++
		stats.Bytes += info.Size()
	}

	// The durability barrier. Everything above is only queued until this
	// returns; the marker below must not be published ahead of it.
	if err := store.backend.Flush(ctx); err != nil {
		return stats, fmt.Errorf("snapshot: flushing session objects: %w", err)
	}

	if err := e.publishSnapshotMarker(ctx, store, stage, req, stats); err != nil {
		return stats, err
	}
	return stats, nil
}

// publishSnapshotMarker writes and durably stores the commit record. Runs only
// after every session object is durable, so the marker can only ever describe a
// complete snapshot.
func (e *Engine) publishSnapshotMarker(ctx context.Context, store *sessionObjectStore, stage string, req snapshotRequest, stats snapshotStats) error {
	marker := sessionSnapshotMarker{
		SchemaVersion: sessionSnapshotMarkerVersion,
		SessionID:     e.Session.ID,
		KeyPrefix:     sessionObjectKeyPrefix(e.Session.ID),
		Sequence:      stats.Sequence,
		Trigger:       req.trigger,
		TurnID:        req.turnID,
		CompletedAt:   time.Now().UTC(),
		Objects:       stats.Objects,
		Bytes:         stats.Bytes,
	}
	body, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("snapshot: marshaling commit marker: %w", err)
	}
	path := filepath.Join(stage, "snapshot.json")
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("snapshot: staging commit marker: %w", err)
	}
	if err := store.backend.Put(ctx, sessionSnapshotMarkerKey(e.Session.ID), path); err != nil {
		return fmt.Errorf("snapshot: uploading commit marker: %w", err)
	}
	if err := store.backend.Flush(ctx); err != nil {
		return fmt.Errorf("snapshot: flushing commit marker: %w", err)
	}
	return nil
}

// isJournalSegmentPath reports whether a session-relative path names a file
// directly inside the journal directory — a segment or the header, all of which
// journal.Writer.Snapshot is authoritative for. Files further down (the
// tool-result cache at journal/cache/) are ordinary tree data.
func isJournalSegmentPath(slash string) bool {
	rest, ok := strings.CutPrefix(slash, "journal/")
	return ok && !strings.Contains(rest, "/")
}

// snapshotSessionOnShutdown takes the final snapshot of a run.
//
// Without it a clean shutdown would flush a backend that had never been handed
// the last turn's state — and a session that completed no turns at all would
// leave nothing in the bucket whatsoever. Called from Stop after the plugins
// and the journal have closed but before per-plugin SQLite does, because the
// WAL checkpoint needs those handles open.
func (e *Engine) snapshotSessionOnShutdown(jw *journal.Writer) {
	if e.objectStore == nil || e.Session == nil {
		return
	}
	e.runSessionSnapshot(snapshotRequest{trigger: snapshotTriggerShutdown, journal: jw})
}
