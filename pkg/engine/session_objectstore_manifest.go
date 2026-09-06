package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// This file closes the gap E1-S4 opened and E1-S5 proved.
//
// # The gap
//
// A snapshot never deletes. It adds and overwrites only, which is what makes a
// failed snapshot unable to destroy remote state it did not successfully
// replace — and which also means an *interrupted* snapshot leaves the bucket
// holding a superset: some objects from generation N+1 sitting beside the rest
// of generation N. The commit marker at sessions/<id>.snapshot.json is written
// last and therefore always names the last COMPLETE snapshot, but until this
// file existed nothing read it back. hydrateSessionTree pulled down whatever
// happened to be under the prefix, so a resumed session could come up with
// files/ from the turn that died beside a journal and a store.db from the turn
// before it. Nothing errored. Nothing logged. The transcript simply disagreed
// with the artifacts, and only a human comparing them would ever notice.
//
// # The fix, and the cost it accepts
//
// A generation stamp plus a per-object manifest. Every snapshot writes, after
// every session object is durable and before the commit marker, a small object
// listing exactly the session-relative paths that generation asserts are
// present. Hydration reads it and materialises exactly that set: an object in
// the bucket but absent from the committed manifest is left in the bucket and
// never lands in the tree.
//
// E1-S4 deliberately kept the marker free of a file listing, precisely because
// a listing grows with the tree and the marker is rewritten every turn. That
// argument was correct and is now overridden on purpose: the user chose
// correctness over the cost. So the manifest is kept as cheap as it can
// possibly be — a sorted array of session-relative paths and nothing else. No
// sizes, no digests, no mtimes, no per-object metadata. It is a *set*, not an
// index, and anything that turns it into an index is a change to be argued for
// on its own merits rather than smuggled in here. Measured cost is in
// BenchmarkSessionSnapshot's manifest_KiB metric, against the same 91 MiB /
// 1007-object session E1-S4 benchmarked.
//
// # Why the manifest is a separate object from the marker
//
// Folding the listing into sessions/<id>.snapshot.json was the obvious
// alternative: one object, one write, no possible skew between two records.
// It was rejected because the marker is the thing an operator reads by hand to
// answer "what is the state of this session in the bucket?", and a marker with
// a thousand paths in it is a marker nobody reads. Keeping the marker small
// preserves the property E1-S4 built it for; the manifest carries the same
// generation number, so the two are still trivially lined up.
//
// The skew that split them creates is bounded and safe in the one direction it
// can occur. The manifest is written first and the marker second, so the only
// reachable mismatch is "manifest names G, marker names G-1" — a snapshot that
// made every object AND the manifest durable and then failed on the marker.
// The manifest is still exactly right about generation G in that case, which is
// why hydration keys off the manifest rather than the marker.
//
// # Why the manifest is a directory-shaped sibling key
//
// It lives at sessions/<id>.manifest/manifest.json, which — because
// objectstore.TrimKeyPrefix matches on whole segments — is NOT under the
// session's own prefix sessions/<id>. So it never hydrates down into the local
// tree and never becomes an input to the next snapshot. That is exactly the
// argument sessionSnapshotMarkerKey and sessionOwnerKeyPrefix already make.
//
// It is a *prefix* with an object under it, rather than a flat
// "sessions/<id>.manifest.json", for the reason session_owner.go spells out:
// objectstore.Backend has no single-object read, Hydrate is the only way to
// pull bytes down, and Hydrate takes a prefix whose exact-match object is
// explicitly not "under" it. Widening the published Backend interface with a
// Get would be a breaking change for every out-of-repo backend module the
// registry exists to support. One extra key segment costs nothing. It is also
// the reason the commit marker itself cannot be the thing hydration reads: at
// its flat key, no Backend method can fetch it.
//
// # THE TRAP: the committed set is not "what this turn uploaded"
//
// This is the sharpest thing in the story and it is worth being blunt about.
// The manifest is built from snapshotEntry.included, never from a tally of
// successful Put calls, because two whole categories of committed object are
// not uploaded by the snapshot that commits them:
//
//  1. Immutable-skip (E2-S2). Sealed journal segments and content-addressed
//     blobs are skipped when a listing taken moments earlier proves the store
//     already holds them at exactly that size. They are unambiguously part of
//     the committed set; they simply cost nothing this turn.
//  2. Blob write-through (E3-S2) and its retry queue (E3-S4). Blobs are pushed
//     the instant they land, outside any snapshot, possibly several turns
//     earlier.
//
// A manifest built from the Put tally would omit every one of those, and a
// hydration honouring that manifest would then faithfully reproduce a session
// with its journal history and every blob missing — a far worse bug than the
// one this file fixes. snapshotEntry.included is true for uploaded and skipped
// entries alike and false only for a file that vanished from the live tree
// mid-snapshot, which is precisely the predicate wanted.
//
// # The one exemption from pruning, and why it is not a loophole
//
// Content-addressed blobs are kept even when the committed manifest does not
// name them. Not for bandwidth — for correctness, and the argument is specific
// to them:
//
//   - blobs.Store sweeps the LOCAL blob tree under an LRU byte budget, while a
//     snapshot never deletes remotely. So a blob referenced by a
//     nexus-blob: URI in generation N's conversation history can legitimately
//     be absent from generation N's manifest (it was swept before the walk) and
//     present in the bucket. Pruning it would break a URI that resolves today.
//   - Every object written outside a snapshot is, by construction, a
//     content-addressed blob: the write-through hook and the retry queue push
//     nothing else. So the exemption is exactly coextensive with "objects a
//     snapshot manifest cannot be expected to have seen".
//   - A blob cannot make the tree disagree with itself. Its key is the sha256
//     of its content, so an extra blob is an unreferenced file, never a stale
//     version of something else. Contrast files/report.md from a dead turn
//     sitting beside a journal from the turn before it, which is the exact
//     inconsistency this file exists to prevent.
//
// Rotated journal segments are immutable too and are deliberately NOT exempt:
// a sealed segment from an interrupted generation carries events the committed
// history does not, so restoring it would reintroduce the disagreement.
//
// # What is deliberately not done
//
// The orphaned objects of the interrupted generation are left in the bucket.
// Nothing in this effort deletes remote data — the same stance the broker takes
// toward PVCs. Reclamation is the operator's, and a hydration that started
// deleting objects it decided were stale would be one bug away from destroying
// a session it merely misread.

const (
	// sessionManifestKeySuffix turns a session key prefix into the prefix
	// holding its committed-object manifest. A sibling of the tree, sharing
	// the one pathological case the owner marker's ".owner" suffix records: a
	// session literally named "<other-id>.manifest" would collide. Session IDs
	// are timestamps or UUIDs, so this is recorded rather than guarded against
	// with a second naming rule.
	sessionManifestKeySuffix = ".manifest"

	// sessionManifestObjectName is the object under that prefix. Fixed rather
	// than per-generation on purpose: a per-generation key would accumulate
	// one object per turn for ever, in a subsystem that never deletes. One
	// object the next generation overwrites is the only shape that does not
	// grow without bound.
	sessionManifestObjectName = "manifest.json"

	// sessionSnapshotManifestVersion is the manifest's own on-disk format
	// version, kept separate from any bus event so an operator reading the
	// bucket can tell what they are looking at without a Nexus build to hand.
	// Same split sessionSnapshotMarkerVersion and sessionOwnerMarkerVersion
	// make.
	sessionSnapshotManifestVersion = 1

	// sessionManifestReadTimeout bounds the one-object read hydration does
	// before it prunes. Short: a wedged store must not turn a resume into a
	// hang, and the fallback (no manifest, hydrate everything) is exactly the
	// behaviour that shipped before this file existed.
	sessionManifestReadTimeout = 30 * time.Second
)

// sessionManifestKeyPrefix is the key prefix holding a session's manifest.
func sessionManifestKeyPrefix(sessionID string) string {
	return sessionObjectKeyPrefix(sessionID) + sessionManifestKeySuffix
}

// sessionManifestKey is the manifest object itself.
func sessionManifestKey(sessionID string) string {
	return sessionManifestKeyPrefix(sessionID) + "/" + sessionManifestObjectName
}

// sessionSnapshotManifest is the JSON body of the per-object commit record:
// exactly the object set one generation asserts is durably present, as
// session-relative "/"-separated paths.
//
// Objects is the only field that grows with the session, and it holds paths
// alone. See the block comment above for why nothing else is in it.
type sessionSnapshotManifest struct {
	SchemaVersion int    `json:"_schema_version"`
	SessionID     string `json:"session_id"`
	KeyPrefix     string `json:"key_prefix"`
	// Generation is the same monotonically increasing stamp the commit marker
	// carries, so a manifest and a marker pulled from a bucket can be lined up
	// without trusting timestamps written by two different clocks.
	Generation  uint64    `json:"generation"`
	CompletedAt time.Time `json:"completed_at"`
	// Objects are session-relative paths, sorted, so two manifests of the same
	// tree are byte-identical and a diff between generations is readable.
	Objects []string `json:"objects"`
}

// set renders the manifest as a membership test.
func (m sessionSnapshotManifest) set() map[string]struct{} {
	out := make(map[string]struct{}, len(m.Objects))
	for _, rel := range m.Objects {
		out[rel] = struct{}{}
	}
	return out
}

// buildSnapshotManifest turns the marked entries of one snapshot into the
// committed-object record.
//
// Built from entry.included, NOT from a count of successful Put calls. That is
// the whole correctness argument and it is spelled out in the block comment
// above under "THE TRAP" — a skipped immutable file and a write-through blob
// are both part of the committed set despite this snapshot having uploaded
// neither.
func buildSnapshotManifest(sessionID string, generation uint64, entries []snapshotEntry) sessionSnapshotManifest {
	objects := make([]string, 0, len(entries))
	for i := range entries {
		if !entries[i].included {
			continue
		}
		objects = append(objects, entries[i].rel)
	}
	sort.Strings(objects)
	return sessionSnapshotManifest{
		SchemaVersion: sessionSnapshotManifestVersion,
		SessionID:     sessionID,
		KeyPrefix:     sessionObjectKeyPrefix(sessionID),
		Generation:    generation,
		CompletedAt:   time.Now().UTC(),
		Objects:       objects,
	}
}

// publishSnapshotManifest stages the manifest and makes it durable.
//
// Runs after the barrier that made every session object durable and before the
// commit marker is written, so a manifest is never visible ahead of the objects
// it describes. Its own Flush is load-bearing for exactly the same reason the
// marker's is: an unflushed manifest is one a resuming host cannot see, and the
// generation it describes would be silently unrestorable.
func (e *Engine) publishSnapshotManifest(ctx context.Context, store *sessionObjectStore, stage string, manifest sessionSnapshotManifest) error {
	// Not indented, unlike the marker. A thousand-entry array at two spaces of
	// indent per line is most of the manifest's bytes, and unlike the marker
	// this object is not meant to be read by eye without tooling.
	body, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("snapshot: marshaling object manifest: %w", err)
	}
	path := filepath.Join(stage, sessionManifestObjectName)
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("snapshot: staging object manifest: %w", err)
	}
	if err := store.backend.Put(ctx, sessionManifestKey(manifest.SessionID), path); err != nil {
		return fmt.Errorf("snapshot: uploading object manifest: %w", err)
	}
	if err := store.backend.Flush(ctx); err != nil {
		return fmt.Errorf("snapshot: flushing object manifest: %w", err)
	}
	return nil
}

// readSessionSnapshotManifest pulls the committed manifest down and parses it.
//
// found is false — with no error — when the store holds no manifest. That is
// not exotic: it is every session written by a build older than this file, and
// every session that has never completed a snapshot. The caller treats it as
// "hydrate everything", which is exactly the behaviour that shipped before,
// so an old bucket keeps resuming.
//
// The read goes through Hydrate because Backend has no single-object get; see
// the block comment above for why the interface was not widened. scratch gets
// a fresh subdirectory so a stale file from an earlier call can never be
// mistaken for a fresh read.
func (e *Engine) readSessionSnapshotManifest(ctx context.Context, store *sessionObjectStore, sessionID string, scratch string) (sessionSnapshotManifest, bool, error) {
	var manifest sessionSnapshotManifest

	readDir := filepath.Join(scratch, "manifest-read")
	if err := os.RemoveAll(readDir); err != nil {
		return manifest, false, fmt.Errorf("object manifest: clearing read staging: %w", err)
	}
	if err := os.MkdirAll(readDir, 0o700); err != nil {
		return manifest, false, fmt.Errorf("object manifest: read staging: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(readDir); err != nil {
			e.Logger.Warn("object store: removing the manifest read staging dir failed",
				"dir", readDir, "error", err)
		}
	}()

	readCtx, cancel := context.WithTimeout(ctx, sessionManifestReadTimeout)
	defer cancel()
	if err := store.backend.Hydrate(readCtx, sessionManifestKeyPrefix(sessionID), readDir); err != nil {
		return manifest, false, fmt.Errorf("object manifest: reading %s: %w", sessionManifestKeyPrefix(sessionID), err)
	}

	data, err := os.ReadFile(filepath.Join(readDir, sessionManifestObjectName))
	if errors.Is(err, fs.ErrNotExist) {
		return manifest, false, nil
	}
	if err != nil {
		return manifest, false, fmt.Errorf("object manifest: reading staged manifest: %w", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, false, fmt.Errorf("object manifest: parsing %s: %w", sessionManifestKey(sessionID), err)
	}
	return manifest, true, nil
}

// pruneStagingToManifest removes, from a freshly hydrated staging tree,
// everything the committed manifest does not name.
//
// Runs on the staging directory rather than on the committed session root, so
// an object from an interrupted generation is never observable at the real
// session path even for an instant — the same reason scrubObjectStoreExcluded
// runs there.
//
// Returns the number of files kept, the number removed, and the number kept
// only by the content-addressed-blob exemption. The last one is reported
// separately because it is the number that says "the bucket is ahead of the
// committed generation in the one way that is safe", and conflating it with an
// ordinary keep would hide it.
//
// Empty directories left behind by a removal are deliberately not swept. A
// session tree already contains empty directories in the ordinary course of
// events — SessionWorkspace.PluginDir creates one as a side effect of returning
// a path — so nothing downstream can distinguish "empty because pruned" from
// "empty because nobody wrote there yet", and a recursive directory removal
// running over a tree that is about to become a live session is a much worse
// thing to get wrong than a stray empty directory is to leave.
func pruneStagingToManifest(root string, manifest sessionSnapshotManifest) (kept, removed, retainedBlobs int, err error) {
	committed := manifest.set()
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("relativising %s: %w", path, relErr)
		}
		slash := filepath.ToSlash(rel)
		if _, ok := committed[slash]; ok {
			kept++
			return nil
		}
		if objectStoreContentAddressedBlob(slash) {
			// See "The one exemption from pruning" above. Counted, not
			// silent: a session whose bucket is persistently ahead of its
			// committed generation by hundreds of blobs is worth being able
			// to see.
			kept++
			retainedBlobs++
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("removing uncommitted object %s: %w", slash, rmErr)
		}
		removed++
		return nil
	})
	if walkErr != nil {
		return kept, removed, retainedBlobs, walkErr
	}
	return kept, removed, retainedBlobs, nil
}
