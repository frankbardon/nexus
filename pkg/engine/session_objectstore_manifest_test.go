package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"
)

// These tests cover the generation stamp and the per-object manifest: the two
// things that turn "an interrupted snapshot is detectable" into "an interrupted
// snapshot is survivable".
//
// The sharpest of them is TestSnapshotManifestNamesObjectsThisTurnDidNotUpload.
// Two stories warned in advance that the committed object set is not the set of
// objects uploaded this turn — immutable-skip does not re-upload sealed
// segments and blobs, and write-through pushes blobs outside any snapshot at
// all — and that a manifest built from the Put tally would make hydration
// faithfully reproduce a truncated session. That test is the guard against it.

// readManifest pulls the committed manifest straight out of the fake bucket.
func readManifest(t *testing.T, backend *objectstoretest.Memory, sessionID string) sessionSnapshotManifest {
	t.Helper()
	body, ok := backend.Get(sessionManifestKey(sessionID))
	if !ok {
		t.Fatalf("no object manifest at %q; keys = %v", sessionManifestKey(sessionID), backend.Keys())
	}
	var m sessionSnapshotManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal object manifest: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Key layout
// ---------------------------------------------------------------------------

// The manifest describes a session and must therefore not become part of one.
// A key under sessions/<id> would hydrate down into the local tree and then be
// re-uploaded by the next snapshot, so every resume would carry a fossilised
// listing of the generation before it — inside the very tree the listing is
// supposed to be authoritative about.
func TestManifestIsOutsideTheSessionPrefix(t *testing.T) {
	const id = "sess-1"
	key := sessionManifestKey(id)
	if _, under := objectstore.TrimKeyPrefix(key, sessionObjectKeyPrefix(id)); under {
		t.Errorf("%q is under the session prefix %q; it would hydrate into the tree",
			key, sessionObjectKeyPrefix(id))
	}
	// And it must be reachable by the only read Backend offers, which needs a
	// prefix whose objects are strictly beneath it.
	rel, under := objectstore.TrimKeyPrefix(key, sessionManifestKeyPrefix(id))
	if !under || rel != sessionManifestObjectName {
		t.Errorf("TrimKeyPrefix(%q, %q) = (%q, %v), want (%q, true) — Hydrate could not read it",
			key, sessionManifestKeyPrefix(id), rel, under, sessionManifestObjectName)
	}
	// A neighbouring session whose ID extends this one must not collide.
	if _, under := objectstore.TrimKeyPrefix(sessionManifestKey("sess-10"), sessionManifestKeyPrefix(id)); under {
		t.Error("sess-10's manifest is under sess-1's manifest prefix")
	}
}

// ---------------------------------------------------------------------------
// THE TRAP
// ---------------------------------------------------------------------------

// buildSnapshotManifest must read snapshotEntry.included, never a tally of
// successful uploads. Pinned at the function as well as end-to-end below,
// because this is the one mistake that would make every acceptance test in the
// story pass while silently truncating real sessions.
func TestBuildSnapshotManifestUsesIncludedNotUploaded(t *testing.T) {
	entries := []snapshotEntry{
		{rel: "files/report.md", included: true},
		// Skipped by immutable-skip: not uploaded this generation, and still
		// unambiguously part of the committed set.
		{rel: "journal/events-001.jsonl.zst", included: true, immutable: true, skipped: true},
		{rel: "blobs/aa/aa.bin", included: true, immutable: true, skipped: true},
		// Vanished from the live tree between the walk and the upload. The one
		// case that is genuinely not committed.
		{rel: "files/deleted-mid-snapshot.md", included: false},
	}
	got := buildSnapshotManifest("sess-1", 7, entries)

	want := []string{"blobs/aa/aa.bin", "files/report.md", "journal/events-001.jsonl.zst"}
	if !slicesEqualStrings(got.Objects, want) {
		t.Errorf("manifest objects = %v, want %v", got.Objects, want)
	}
	if got.Generation != 7 {
		t.Errorf("manifest generation = %d, want 7", got.Generation)
	}
	if got.SessionID != "sess-1" || got.KeyPrefix != sessionObjectKeyPrefix("sess-1") {
		t.Errorf("manifest identity = %q/%q", got.SessionID, got.KeyPrefix)
	}
	if got.SchemaVersion != sessionSnapshotManifestVersion {
		t.Errorf("manifest schema version = %d, want %d", got.SchemaVersion, sessionSnapshotManifestVersion)
	}
}

// The end-to-end half of the same guard, over the real snapshot path.
//
// A second turn re-uploads almost nothing: immutable-skip proves the blobs are
// already in the bucket at exactly the right size and does not re-transfer
// them. The manifest that second snapshot publishes must still name every one
// of them. If it named only what the turn transferred, a hydration honouring it
// would restore a session with no blobs at all.
func TestSnapshotManifestNamesObjectsThisTurnDidNotUpload(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	sessionID := eng.Session.ID

	// Through SessionWorkspace.BlobStore, not blobs.New: this is the door that
	// carries the write-through hook, so these bytes travel to the store
	// outside any snapshot as well as inside one.
	blobStore, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	var blobRels []string
	for i := 0; i < 4; i++ {
		h, err := blobStore.Put([]byte(fmt.Sprintf("blob payload %d", i)), "application/octet-stream")
		if err != nil {
			t.Fatalf("blob put: %v", err)
		}
		blobRels = append(blobRels,
			"blobs/"+h.SHA256[:2]+"/"+h.SHA256+".bin",
			"blobs/"+h.SHA256[:2]+"/"+h.SHA256+".meta")
	}
	if err := eng.Session.WriteFile("files/report.md", []byte("mutable, re-uploaded every turn")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var results []events.SessionSnapshotResult
	eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
			results = append(results, r)
		}
	})

	endTurn(t, eng, "turn-1")
	endTurn(t, eng, "turn-2")
	if len(results) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("snapshot results = %+v, want two successful ones", results)
	}
	// The premise: the second turn genuinely did not upload the blobs.
	if results[1].ObjectsSkipped < len(blobRels) {
		t.Fatalf("second snapshot skipped %d objects, want at least the %d blob files — "+
			"immutable-skip did not engage and the test proves nothing",
			results[1].ObjectsSkipped, len(blobRels))
	}

	manifest := readManifest(t, backend, sessionID)
	named := manifest.set()
	for _, rel := range blobRels {
		if _, ok := named[rel]; !ok {
			t.Errorf("manifest for generation %d omits %q, which the second snapshot skipped "+
				"rather than uploaded; a hydration honouring it would restore a session with no blobs",
				manifest.Generation, rel)
		}
	}

	// The whole committed set, not just the blobs: the manifest must describe
	// exactly the session objects the bucket holds.
	wantSet := map[string]struct{}{}
	for _, rel := range sessionKeys(backend, sessionID) {
		wantSet[rel] = struct{}{}
	}
	for rel := range named {
		if _, ok := wantSet[rel]; !ok {
			t.Errorf("manifest names %q but the bucket does not hold it", rel)
		}
	}
	for rel := range wantSet {
		if _, ok := named[rel]; !ok {
			t.Errorf("the bucket holds %q but the manifest does not name it", rel)
		}
	}
	if manifest.Objects == nil || len(manifest.Objects) < len(blobRels)+1 {
		t.Errorf("manifest holds %d objects, want at least %d", len(manifest.Objects), len(blobRels)+1)
	}
	if results[1].Objects != len(manifest.Objects) {
		t.Errorf("result reported %d committed objects, manifest names %d; the two must be the same set",
			results[1].Objects, len(manifest.Objects))
	}
}

// ---------------------------------------------------------------------------
// The generation stamp
// ---------------------------------------------------------------------------

// The stamp increases per snapshot, the marker and the manifest agree on it,
// and the marker points at the manifest.
func TestCommitMarkerCarriesTheGenerationAndNamesTheManifest(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	sessionID := eng.Session.ID

	for i, turn := range []string{"turn-1", "turn-2", "turn-3"} {
		endTurn(t, eng, turn)
		marker := readMarker(t, backend, sessionID)
		manifest := readManifest(t, backend, sessionID)
		wantGen := uint64(i + 1)
		if marker.Generation != wantGen {
			t.Errorf("after %s marker generation = %d, want %d", turn, marker.Generation, wantGen)
		}
		if manifest.Generation != wantGen {
			t.Errorf("after %s manifest generation = %d, want %d", turn, manifest.Generation, wantGen)
		}
		if marker.ManifestKey != sessionManifestKey(sessionID) {
			t.Errorf("marker manifest key = %q, want %q", marker.ManifestKey, sessionManifestKey(sessionID))
		}
	}
}

// A generation stamp that restarted on every resume would be useless: two
// markers pulled from a bucket could not be ordered. The baseline is read back
// from the committed manifest.
func TestGenerationContinuesAcrossAResume(t *testing.T) {
	backendName := "memory-" + t.Name()
	backend := objectstoretest.RegisterMemory(t, backendName, nil)

	first := resumeEngine(t, backendName, t.TempDir())
	if err := first.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := first.Session.ID
	endTurn(t, first, "turn-1")
	endTurn(t, first, "turn-2")
	if err := first.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Two turns plus the shutdown snapshot.
	afterFirst := readMarker(t, backend, sessionID).Generation
	if afterFirst != 3 {
		t.Fatalf("generation after the first run = %d, want 3", afterFirst)
	}

	second := resumeEngine(t, backendName, t.TempDir())
	second.RecallSessionID = sessionID
	if err := second.Boot(context.Background()); err != nil {
		t.Fatalf("Boot resuming: %v", err)
	}
	t.Cleanup(func() { _ = second.Stop(context.Background()) })
	endTurn(t, second, "turn-3")

	marker := readMarker(t, backend, sessionID)
	if marker.Generation != afterFirst+1 {
		t.Errorf("generation after the resumed run's first turn = %d, want %d",
			marker.Generation, afterFirst+1)
	}
	if marker.Sequence != 1 {
		t.Errorf("sequence = %d after the resumed run's first snapshot, want 1 — "+
			"sequence is per-run and must not have been conflated with the generation", marker.Sequence)
	}
}

// ---------------------------------------------------------------------------
// The headline: an interrupted snapshot is survivable
// ---------------------------------------------------------------------------

// Formerly TestHydrationDoesNotValidateTheCommitMarker (E1-S5), which asserted
// the *unvalidated* behaviour: a bucket holding some objects from a complete
// snapshot and some from a half-finished one hydrated silently and looked
// exactly like a good session. E3-S5 updated the test in place rather than
// deleting it, because the scenario it seeds is the acceptance criterion.
//
// Seeded rather than driven through the engine on purpose: the point is a
// bucket in a state no successful run can produce, and constructing it by hand
// is what makes the assertion independent of the code path that would have
// produced it.
func TestHydrationRestoresOnlyTheCommittedGeneration(t *testing.T) {
	backendName := "memory-" + t.Name()
	backend := objectstoretest.RegisterMemory(t, backendName, nil)

	const sessionID = "sess-interrupted"
	dir := t.TempDir()
	writeSessionTree(t, dir, sessionID, map[string]string{
		"files/from-generation-1.md":  "committed",
		"context/conversation.jsonl":  `{"Role":"user","Content":"generation 1"}` + "\n",
		"plugins/nexus.x/notes.jsonl": "generation 1 notes\n",
		// The objects of a generation that never completed. Under the old
		// behaviour they were indistinguishable from committed state, and a
		// session hydrated from this bucket had files from turn N+1 sitting
		// beside a history from turn N.
		"files/from-half-finished-generation-2.md": "never committed",
		"journal/events-002.jsonl.zst":             "sealed segment from the dead generation",
	})
	if err := backend.SeedTree(sessionObjectKeyPrefix(sessionID), dir); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	committed := []string{
		"metadata/session.json",
		"files/from-generation-1.md",
		"context/conversation.jsonl",
		"plugins/nexus.x/notes.jsonl",
	}
	orphans := []string{
		"files/from-half-finished-generation-2.md",
		"journal/events-002.jsonl.zst",
	}
	seedManifest(t, backend, sessionID, 1, committed)
	seedMarker(t, backend, sessionID, 1)

	freshRoot := t.TempDir()
	eng := resumeEngine(t, backendName, freshRoot)
	if err := eng.openObjectStore(context.Background()); err != nil {
		t.Fatalf("openObjectStore: %v", err)
	}
	t.Cleanup(eng.releaseObjectStore)
	if err := eng.hydrateSessionTree(context.Background(), sessionID); err != nil {
		t.Fatalf("hydrateSessionTree: %v", err)
	}

	restored := filepath.Join(freshRoot, "sessions", sessionID)
	got := treeFingerprint(t, restored)
	for _, rel := range committed {
		if _, ok := got[rel]; !ok {
			t.Errorf("committed object %q was not restored; tree = %v", rel, sortedKeys(got))
		}
	}
	for _, rel := range orphans {
		if _, ok := got[rel]; ok {
			t.Errorf("%q came from a snapshot that never completed and must not have been restored", rel)
		}
	}
	if len(got) != len(committed) {
		t.Errorf("restored tree holds %d files, want exactly the %d committed ones: %v",
			len(got), len(committed), sortedKeys(got))
	}

	// The mutual-consistency claim, spelled out: the artifacts, the history and
	// the plugin state all belong to generation 1.
	for rel, want := range map[string]string{
		"files/from-generation-1.md":  "committed",
		"context/conversation.jsonl":  `{"Role":"user","Content":"generation 1"}` + "\n",
		"plugins/nexus.x/notes.jsonl": "generation 1 notes\n",
	} {
		if body := string(readTreeFile(t, restored, rel)); body != want {
			t.Errorf("restored %q = %q, want %q", rel, body, want)
		}
	}

	// Left in the bucket, not deleted. This effort never removes remote data;
	// reclamation is the operator's.
	for _, rel := range orphans {
		if _, ok := backend.Get(sessionObjectKeyPrefix(sessionID) + "/" + rel); !ok {
			t.Errorf("hydration deleted the orphaned object %q from the bucket", rel)
		}
	}
	if backend.Counts().Deletes != 0 {
		t.Errorf("hydration issued %d deletes; it must never delete remote data", backend.Counts().Deletes)
	}
}

// The same guarantee over a real run rather than a seeded bucket, and the
// acceptance criterion in full: generation N is built by a real engine — real
// journal, real per-plugin SQLite, real blobs — a partial generation N+1 is
// then placed in the bucket, and what hydrates on a fresh host must be exactly
// generation N with its history, its store.db and its files/ all agreeing.
//
// The N+1 objects are seeded rather than produced by a second failing snapshot,
// for the same reason the seeded test above exists: the state being reproduced
// is one no successful run can reach, and building it by hand is what keeps the
// assertion independent of the code that would otherwise have produced it.
// TestInterruptedSnapshotCanOverwriteACommittedObjectInPlace covers what a real
// failing snapshot additionally does, and what this design does not undo.
func TestInterruptedSnapshotRestoresTheCommittedGenerationIntact(t *testing.T) {
	backendName := "memory-" + t.Name()
	backend := objectstoretest.RegisterMemory(t, backendName, nil)

	eng := resumeEngine(t, backendName, t.TempDir())
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	// Deliberately no Stop: a clean shutdown would take a further snapshot and
	// commit a generation on top of the one under test.
	sessionID := eng.Session.ID

	history := []byte(`{"Role":"user","Content":"generation 1"}` + "\n")
	if err := eng.Session.WriteFile("context/conversation.jsonl", history); err != nil {
		t.Fatalf("write history: %v", err)
	}
	if err := eng.Session.WriteFile("files/generation-1.md", []byte("committed")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	blobStore, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	blob, err := blobStore.Put([]byte("generation 1 blob"), "text/plain")
	if err != nil {
		t.Fatalf("blob put: %v", err)
	}
	st, err := eng.Storage.Open(storage.ScopeSession, resumePluginID)
	if err != nil {
		t.Fatalf("open session storage: %v", err)
	}
	for i := 0; i < 200; i++ {
		if err := st.Put(fmt.Sprintf("row-%03d", i), []byte("generation 1")); err != nil {
			t.Fatalf("storage put: %v", err)
		}
	}
	if err := eng.Bus.Emit("agent.turn.start", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion,
		TurnID:        "turn-1",
	}); err != nil {
		t.Fatalf("emit agent.turn.start: %v", err)
	}
	endTurn(t, eng, "turn-1")

	manifest := readManifest(t, backend, sessionID)
	if manifest.Generation != 1 || len(manifest.Objects) == 0 {
		t.Fatalf("committed manifest = generation %d over %d objects, want generation 1 over some",
			manifest.Generation, len(manifest.Objects))
	}

	// The partial generation 2: objects a snapshot uploaded and then died
	// before it could publish a manifest for. Additive, which is the shape a
	// snapshot that never deletes leaves behind — files/ from the dead turn
	// sitting beside a journal and a store.db from the turn before it.
	orphans := map[string]string{
		"files/from-half-finished-generation-2.md": "never committed",
		"journal/events-002.jsonl.zst":             "sealed segment from the dead generation",
		"plugins/" + resumePluginID + "/scratch-2": "plugin state from the dead generation",
	}
	for rel, body := range orphans {
		if err := backend.Seed(sessionObjectKeyPrefix(sessionID)+"/"+rel, []byte(body)); err != nil {
			t.Fatalf("seeding orphan %s: %v", rel, err)
		}
	}

	// *** kill *** — the engine is abandoned from here, exactly as the resume
	// suite does it, and a fresh host with an empty filesystem picks the
	// session up.
	freshRoot := t.TempDir()
	resumed := resumeEngine(t, backendName, freshRoot)
	if err := resumed.openObjectStore(context.Background()); err != nil {
		t.Fatalf("openObjectStore: %v", err)
	}
	t.Cleanup(resumed.releaseObjectStore)
	if err := resumed.hydrateSessionTree(context.Background(), sessionID); err != nil {
		t.Fatalf("hydrateSessionTree: %v", err)
	}

	restored := filepath.Join(freshRoot, "sessions", sessionID)
	got := treeFingerprint(t, restored)

	// Exactly the committed set. Every path in the tree is one the manifest
	// names, and every path the manifest names is in the tree.
	named := manifest.set()
	for rel := range got {
		if _, ok := named[rel]; !ok {
			t.Errorf("hydrated %q, which generation %d does not name", rel, manifest.Generation)
		}
	}
	for rel := range named {
		if _, ok := got[rel]; !ok {
			t.Errorf("generation %d names %q but it was not restored", manifest.Generation, rel)
		}
	}
	for rel := range orphans {
		if _, ok := got[rel]; ok {
			t.Errorf("%q came from a snapshot that never completed and must not have been restored", rel)
		}
	}

	// Mutually consistent: history, store.db, files/ and blobs all belong to
	// generation 1.
	if body := readTreeFile(t, restored, "context/conversation.jsonl"); !bytes.Equal(body, history) {
		t.Errorf("restored history = %q, want %q", body, history)
	}
	if body := string(readTreeFile(t, restored, "files/generation-1.md")); body != "committed" {
		t.Errorf("restored artifact = %q, want %q", body, "committed")
	}
	rows := readRestoredDBRows(t, filepath.Join(restored, "plugins", resumePluginID, "store.db"))
	if len(rows) != 200 {
		t.Errorf("restored store.db holds %d rows, want the 200 generation 1 committed", len(rows))
	}
	restoredBlobs, err := blobs.New(filepath.Join(restored, "blobs"), 0)
	if err != nil {
		t.Fatalf("blobs.New on the restored session: %v", err)
	}
	if body, _, err := restoredBlobs.Get(blob.SHA256); err != nil {
		t.Errorf("restored blob: %v", err)
	} else if string(body) != "generation 1 blob" {
		t.Errorf("restored blob = %q", body)
	}
	journal := readTreeFile(t, restored, "journal/events.jsonl")
	if !bytes.Contains(journal, []byte(`"agent.turn.end"`)) {
		t.Error("restored journal does not contain the turn boundary generation 1 was taken on")
	}

	// The orphans are left in the bucket. This effort never deletes remote
	// data — reclamation is the operator's, the same stance the broker takes
	// toward PVCs.
	for rel := range orphans {
		if _, ok := backend.Get(sessionObjectKeyPrefix(sessionID) + "/" + rel); !ok {
			t.Errorf("hydration deleted the orphaned object %q from the bucket", rel)
		}
	}
	if got := backend.Counts().Deletes; got != 0 {
		t.Errorf("hydration issued %d deletes; it must never delete remote data", got)
	}
}

// The boundary of the design, pinned so nobody has to rediscover it.
//
// A snapshot overwrites in place, and the manifest names *paths*, not versions.
// So an interrupted snapshot that got as far as re-uploading a mutable object —
// context/conversation.jsonl, the active journal segment, a per-plugin
// store.db — has already replaced the committed generation's bytes at that key,
// and no listing of paths can bring them back. Hydration restores exactly the
// committed *set*; within that set, an object the dead generation overwrote
// carries the dead generation's bytes.
//
// The fix would be per-generation object keys, which is the "generation
// directories" alternative E1-S4 costed and rejected: a second full copy of
// every session in the bucket, forever, to close a window measured in the
// milliseconds between one Put and the next. It is recorded here rather than
// built, and it is why E4-S4's mid-flush kill against MinIO is worth running.
func TestInterruptedSnapshotCanOverwriteACommittedObjectInPlace(t *testing.T) {
	backendName := "memory-" + t.Name()
	backend := objectstoretest.RegisterMemory(t, backendName, nil)

	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(dir, "sessions")
	cfg.Core.Storage.Root = dir
	cfg.Core.ObjectStore = objectstore.Config{
		BackendName: backendName,
		Bucket:      "test-bucket",
		// Degrade, not strict: strict would raise core.error and close the turn
		// gate, and this test is about what the bucket is left holding.
		FailurePolicy: objectstore.FailurePolicyDegrade,
	}
	eng := newFromConfig(cfg)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID

	if err := eng.Session.WriteFile("files/generation-1.md", []byte("committed")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := eng.Storage.Open(storage.ScopeSession, resumePluginID)
	if err != nil {
		t.Fatalf("open session storage: %v", err)
	}
	if err := st.Put("generation", []byte("1")); err != nil {
		t.Fatalf("storage put: %v", err)
	}
	endTurn(t, eng, "turn-1")
	if got := readManifest(t, backend, sessionID).Generation; got != 1 {
		t.Fatalf("generation after the first turn = %d, want 1", got)
	}

	// Flush fails for ever, so the objects of turn 2 reach the bucket — Put is
	// what stores them — but nothing ever publishes a manifest or a marker for
	// them, and the retry worker cannot quietly repair it behind the test.
	backend.SetFlushError(errors.New("bucket unreachable"))
	if err := eng.Session.WriteFile("files/generation-2.md", []byte("never committed")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Put("generation", []byte("2")); err != nil {
		t.Fatalf("storage put: %v", err)
	}
	var failed int
	eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok && !r.OK {
			failed++
		}
	})
	endTurn(t, eng, "turn-2")
	if failed == 0 {
		t.Fatal("the second turn's snapshot succeeded; the interruption was not simulated")
	}
	if got := readManifest(t, backend, sessionID).Generation; got != 1 {
		t.Fatalf("manifest generation = %d after the failed snapshot, want 1 — "+
			"the manifest advanced past a snapshot that never completed", got)
	}

	// *** kill ***, then the store comes back.
	backend.SetFlushError(nil)
	freshRoot := t.TempDir()
	resumed := resumeEngine(t, backendName, freshRoot)
	if err := resumed.openObjectStore(context.Background()); err != nil {
		t.Fatalf("openObjectStore: %v", err)
	}
	t.Cleanup(resumed.releaseObjectStore)
	if err := resumed.hydrateSessionTree(context.Background(), sessionID); err != nil {
		t.Fatalf("hydrateSessionTree: %v", err)
	}
	restored := filepath.Join(freshRoot, "sessions", sessionID)

	// What the manifest DOES fix: the dead generation added a key the committed
	// set does not name, and it is not materialised.
	if _, err := os.Stat(filepath.Join(restored, "files", "generation-2.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an artifact from the interrupted snapshot was restored (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(restored, "files", "generation-1.md")); err != nil {
		t.Errorf("the committed generation's artifact is missing: %v", err)
	}

	// What it does NOT fix, asserted rather than hoped for: store.db is a key
	// generation 1 names, so it is restored — carrying the bytes generation 2
	// overwrote it with. A future change that makes this row read "1" is an
	// improvement, and should update this test and the note above with it.
	rows := readRestoredDBRows(t, filepath.Join(restored, "plugins", resumePluginID, "store.db"))
	if rows["generation"] != "2" {
		t.Errorf("restored store.db says generation %q, want %q — "+
			"this test pins the in-place overwrite boundary, not a guarantee", rows["generation"], "2")
	}
}

// ---------------------------------------------------------------------------
// The exemptions and the fallbacks
// ---------------------------------------------------------------------------

// A bucket written by a build older than the manifest — or by a session that
// has never completed a snapshot — has no manifest at all. Hydration must fall
// back to materialising everything under the prefix, which is byte-for-byte the
// behaviour that shipped before, rather than restoring an empty tree.
func TestHydrationWithoutAManifestRestoresEveryObject(t *testing.T) {
	backendName := "memory-" + t.Name()
	backend := objectstoretest.RegisterMemory(t, backendName, nil)

	const sessionID = "sess-no-manifest"
	dir := t.TempDir()
	writeSessionTree(t, dir, sessionID, map[string]string{
		"files/a.md":                 "one",
		"files/b.md":                 "two",
		"context/conversation.jsonl": "{}\n",
	})
	if err := backend.SeedTree(sessionObjectKeyPrefix(sessionID), dir); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	freshRoot := t.TempDir()
	eng := resumeEngine(t, backendName, freshRoot)
	if err := eng.openObjectStore(context.Background()); err != nil {
		t.Fatalf("openObjectStore: %v", err)
	}
	t.Cleanup(eng.releaseObjectStore)
	if err := eng.hydrateSessionTree(context.Background(), sessionID); err != nil {
		t.Fatalf("hydrateSessionTree: %v", err)
	}

	restored := filepath.Join(freshRoot, "sessions", sessionID)
	for _, rel := range []string{"files/a.md", "files/b.md", "context/conversation.jsonl", "metadata/session.json"} {
		if _, err := os.Stat(filepath.Join(restored, filepath.FromSlash(rel))); err != nil {
			t.Errorf("a bucket with no manifest must hydrate whole, but %q is missing: %v", rel, err)
		}
	}
}

// Content-addressed blobs are exempt from the prune, and the exemption is a
// correctness requirement rather than a bandwidth saving.
//
// blobs.Store sweeps the LOCAL tree under an LRU byte budget while a snapshot
// never deletes remotely, so a blob referenced by a nexus-blob: URI in the
// committed history can legitimately be in the bucket and absent from the
// committed manifest. Pruning it would break a URI that resolves today. The
// same exemption covers the write-through push, which is the only other writer
// that puts objects in the bucket outside a snapshot — and it writes nothing
// but blobs.
func TestHydrationKeepsBlobsTheManifestDoesNotName(t *testing.T) {
	backendName := "memory-" + t.Name()
	backend := objectstoretest.RegisterMemory(t, backendName, nil)

	const sessionID = "sess-swept-blob"
	const sha = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	blobRel := "blobs/" + sha[:2] + "/" + sha + ".bin"
	metaRel := "blobs/" + sha[:2] + "/" + sha + ".meta"

	dir := t.TempDir()
	writeSessionTree(t, dir, sessionID, map[string]string{
		"files/report.md": "committed",
		blobRel:           "blob bytes swept from local disk before the snapshot walked the tree",
		metaRel:           `{"media_type":"application/octet-stream"}`,
		// A sealed journal segment is immutable too, and is deliberately NOT
		// exempt: one from an interrupted generation carries events the
		// committed history does not.
		"journal/events-009.jsonl.zst": "sealed segment from a dead generation",
	})
	if err := backend.SeedTree(sessionObjectKeyPrefix(sessionID), dir); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	seedManifest(t, backend, sessionID, 4, []string{"metadata/session.json", "files/report.md"})
	seedMarker(t, backend, sessionID, 4)

	freshRoot := t.TempDir()
	eng := resumeEngine(t, backendName, freshRoot)
	if err := eng.openObjectStore(context.Background()); err != nil {
		t.Fatalf("openObjectStore: %v", err)
	}
	t.Cleanup(eng.releaseObjectStore)
	if err := eng.hydrateSessionTree(context.Background(), sessionID); err != nil {
		t.Fatalf("hydrateSessionTree: %v", err)
	}

	restored := filepath.Join(freshRoot, "sessions", sessionID)
	for _, rel := range []string{blobRel, metaRel} {
		if _, err := os.Stat(filepath.Join(restored, filepath.FromSlash(rel))); err != nil {
			t.Errorf("content-addressed blob %q was pruned; a nexus-blob: URI in the restored "+
				"history now resolves to nothing: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(restored, "journal", "events-009.jsonl.zst")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a sealed journal segment the manifest does not name was restored (stat err = %v); "+
			"the exemption must be blobs only", err)
	}
}

// pruneStagingToManifest is the mechanism, tested directly so its two exemption
// rules are pinned independently of a whole hydration.
func TestPruneStagingToManifest(t *testing.T) {
	root := t.TempDir()
	files := []string{
		"files/keep.md",
		"files/drop.md",
		"context/conversation.jsonl",
		"blobs/aa/" + strings.Repeat("a", 64) + ".bin",
		"blobs/aa/" + strings.Repeat("a", 64) + ".meta",
		"blobs/aa/" + strings.Repeat("a", 64) + ".bin.tmp-1234",
		"journal/events-003.jsonl.zst",
	}
	for _, rel := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	manifest := sessionSnapshotManifest{Objects: []string{"files/keep.md", "context/conversation.jsonl"}}
	kept, removed, retainedBlobs, err := pruneStagingToManifest(root, manifest)
	if err != nil {
		t.Fatalf("pruneStagingToManifest: %v", err)
	}
	if kept != 4 || removed != 3 || retainedBlobs != 2 {
		t.Errorf("kept/removed/retainedBlobs = %d/%d/%d, want 4/3/2", kept, removed, retainedBlobs)
	}
	for _, rel := range []string{
		"files/keep.md",
		"context/conversation.jsonl",
		"blobs/aa/" + strings.Repeat("a", 64) + ".bin",
		"blobs/aa/" + strings.Repeat("a", 64) + ".meta",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%q should have been kept: %v", rel, err)
		}
	}
	for _, rel := range []string{
		"files/drop.md",
		"journal/events-003.jsonl.zst",
		// A half-written blob temp file is not content-addressed and gets no
		// promise; objectStoreContentAddressedBlob rejects it by name.
		"blobs/aa/" + strings.Repeat("a", 64) + ".bin.tmp-1234",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q should have been pruned (stat err = %v)", rel, err)
		}
	}
}

// The manifest must never become an input to the next snapshot, and must never
// appear inside a hydrated tree. Both follow from the key layout, but the key
// layout is one string concatenation away from being wrong.
func TestManifestNeverEntersTheSessionTree(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	sessionID := eng.Session.ID

	if err := eng.Session.WriteFile("files/report.md", []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	endTurn(t, eng, "turn-1")
	endTurn(t, eng, "turn-2")

	for _, rel := range sessionKeys(backend, sessionID) {
		if strings.Contains(rel, sessionManifestObjectName) || strings.Contains(rel, sessionManifestKeySuffix) {
			t.Errorf("the manifest leaked into the session prefix as %q", rel)
		}
	}
	manifest := readManifest(t, backend, sessionID)
	for _, rel := range manifest.Objects {
		if strings.Contains(rel, sessionManifestObjectName) {
			t.Errorf("the manifest lists itself as %q", rel)
		}
	}
	// It is also not on local disk anywhere under the session.
	err := filepath.Walk(eng.Session.RootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == sessionManifestObjectName {
			t.Errorf("the manifest was staged inside the session tree at %q", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the session tree: %v", err)
	}
}

// Zero-impact default: with no backend configured nothing above runs at all.
// The manifest adds no object, no key and no code path to a session that has
// never heard of an object store.
func TestNoManifestWithoutABackend(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if err := eng.Session.WriteFile("files/report.md", []byte("local only")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	endTurn(t, eng, "turn-1")
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == sessionManifestObjectName {
			t.Errorf("a manifest was written at %q with no backend configured", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the data root: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Manifest cost
// ---------------------------------------------------------------------------

// The measurement E1-S4's design note asked for and this story's acceptance
// criteria require: how many bytes does the manifest add to a session of the
// size E1-S4 benchmarked (91 MiB / 1007 objects)?
//
// A test rather than a benchmark because it is an assertion, not a timing: the
// manifest must stay proportional to *path* length and nothing else, so a
// future change that starts recording sizes, digests or mtimes per object shows
// up here as a failure rather than as a slowly growing bucket bill.
func TestManifestSizeAgainstTheBenchmarkSession(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 1000-object session tree")
	}
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	// The same shape as BenchmarkSessionSnapshot's "large" case: 1000 files
	// plus the journal, metadata and a per-plugin store. Contents are tiny —
	// the manifest holds paths, so the payload size is irrelevant to what is
	// being measured, and writing 91 MiB in a unit test is not.
	for i := 0; i < 1000; i++ {
		if err := eng.Session.WriteFile(fmt.Sprintf("files/f%05d.bin", i), []byte("x")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	endTurn(t, eng, "turn-1")

	manifest := readManifest(t, backend, eng.Session.ID)
	if len(manifest.Objects) < 1000 {
		t.Fatalf("manifest names %d objects, want at least 1000", len(manifest.Objects))
	}
	body, ok := backend.Get(sessionManifestKey(eng.Session.ID))
	if !ok {
		t.Fatal("no manifest object in the bucket")
	}
	perObject := float64(len(body)) / float64(len(manifest.Objects))
	t.Logf("manifest for %d objects = %d bytes (%.1f bytes/object)",
		len(manifest.Objects), len(body), perObject)

	// A path plus its quotes and comma. The ceiling is deliberately loose
	// enough that a longer plugin ID or a deeper files/ layout does not fail
	// the build, and tight enough that adding a size or a digest per object
	// cannot slip through: the cheapest such addition roughly doubles this.
	const maxBytesPerObject = 64
	if perObject > maxBytesPerObject {
		t.Errorf("manifest costs %.1f bytes/object, want at most %d — "+
			"the manifest is a set of paths, not an index", perObject, maxBytesPerObject)
	}
	// And against the tree it describes: E1-S4's benchmark session is 91 MiB.
	const benchmarkTreeBytes = 91 << 20
	share := float64(len(body)) / benchmarkTreeBytes
	t.Logf("manifest is %.4f%% of E1-S4's 91 MiB benchmark session, re-uploaded once per turn", share*100)
	if share > 0.001 {
		t.Errorf("manifest is %.4f%% of the benchmark session; it was supposed to be negligible", share*100)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func seedManifest(t *testing.T, backend *objectstoretest.Memory, sessionID string, generation uint64, objects []string) {
	t.Helper()
	sorted := append([]string(nil), objects...)
	sort.Strings(sorted)
	body, err := json.Marshal(sessionSnapshotManifest{
		SchemaVersion: sessionSnapshotManifestVersion,
		SessionID:     sessionID,
		KeyPrefix:     sessionObjectKeyPrefix(sessionID),
		Generation:    generation,
		Objects:       sorted,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := backend.Seed(sessionManifestKey(sessionID), body); err != nil {
		t.Fatalf("seeding manifest: %v", err)
	}
}

func seedMarker(t *testing.T, backend *objectstoretest.Memory, sessionID string, generation uint64) {
	t.Helper()
	body, err := json.Marshal(sessionSnapshotMarker{
		SchemaVersion: sessionSnapshotMarkerVersion,
		SessionID:     sessionID,
		KeyPrefix:     sessionObjectKeyPrefix(sessionID),
		Generation:    generation,
		ManifestKey:   sessionManifestKey(sessionID),
		Sequence:      generation,
		Trigger:       snapshotTriggerTurn,
	})
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if err := backend.Seed(sessionSnapshotMarkerKey(sessionID), body); err != nil {
		t.Fatalf("seeding marker: %v", err)
	}
}

func slicesEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
