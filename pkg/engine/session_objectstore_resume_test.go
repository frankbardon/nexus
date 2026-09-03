package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"

	_ "modernc.org/sqlite"
)

// This file asserts the headline behaviour of the whole object-store effort:
// a session survives the death of the process that produced it and resumes
// somewhere else with the same state. Everything below it in the stack —
// hydration (E1-S2), the shared in-memory backend (E1-S3), the turn-boundary
// snapshot and its WAL checkpoint (E1-S4) — exists to make this test pass, so
// it is deliberately written as an end-to-end assertion over the production
// Boot path rather than as a set of unit checks over the pieces.
//
// # How "kill the process" is simulated
//
// The first engine is booted, driven through a complete turn, and then simply
// abandoned: no Stop, no finalizeObjectStore, no shutdown snapshot, no journal
// Close, no SQLite checkpoint-on-close. Every handle it holds is still open
// when the second engine starts. That is the fidelity that matters, because it
// is what forces the *turn-boundary* snapshot to be the only thing that could
// have saved the session — if the shutdown path were doing the work, these
// tests would fail.
//
// The second engine is given a completely separate temporary directory as its
// data root, so there is no local tree, no session lock, no store.db and no
// blob directory to fall back on: everything it reads must have come back
// through the object store. Combined with writes made *after* the last turn
// boundary (which must be absent from the resumed session), that pins the
// recovery point exactly where the design says it is.
//
// What this cannot simulate in-process is an OS-level SIGKILL: the first
// engine's goroutines keep running and its dirty page cache is never dropped.
// Neither weakens the assertion — the resumed engine reads a different
// filesystem, so anything the dead engine still holds locally is unreachable
// by construction. E4-S4 repeats this against MinIO, where a real wire
// protocol is in the loop.

// resumePluginID is the fake plugin whose session-scope SQLite store stands in
// for real per-plugin state. Session scope specifically: it is the only scope
// that lives inside the session tree, and therefore the only one E1 syncs.
const resumePluginID = "nexus.test.resume"

// killedSession is everything the first process produced, captured at the
// moment of its last completed turn, plus the writes it made afterwards that
// the kill must have destroyed.
type killedSession struct {
	id         string
	sessionDir string

	// fingerprint is rel-path -> sha256 of the tree as of the last completed
	// turn, excluding paths the seam never syncs.
	fingerprint map[string]string
	journal     []byte

	history   []byte
	artifacts map[string][]byte
	blob      blobs.Handle
	blobBytes []byte
	dbRows    map[string]string

	// The three writes made after the turn boundary. A resumed session that
	// contains any of them would mean the recovery point is not where the
	// design claims.
	lostFile    string
	lostKey     string
	lostBlobSHA string
}

// resumeEngine builds an engine rooted at an otherwise-untouched directory and
// pointed at an already-registered backend. Strict failure policy on purpose:
// a snapshot or flush that quietly failed would otherwise leave these tests
// asserting against a stale bucket and passing for the wrong reason.
func resumeEngine(t *testing.T, backendName, dir string) *Engine {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(dir, "sessions")
	cfg.Core.Storage.Root = dir
	cfg.Core.ObjectStore = objectstore.Config{
		BackendName:   backendName,
		Bucket:        "test-bucket",
		FailurePolicy: objectstore.FailurePolicyStrict,
	}
	return newFromConfig(cfg)
}

// resumeHistory is the conversation the first process persisted. Written as
// context/conversation.jsonl because that is the file Boot's replayHistory
// reads on recall — using anything else would assert against a private
// convention instead of the one the engine actually resumes from.
func resumeHistory(t *testing.T) ([]byte, []events.Message) {
	t.Helper()
	msgs := []events.Message{
		{Role: "user", Content: "summarise the findings"},
		{Role: "assistant", Content: "three findings, all in files/report.md"},
	}
	var buf bytes.Buffer
	for _, m := range msgs {
		line, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal history message: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), msgs
}

// runToKill boots a session, fills it with every category of state the story
// names — history, artifacts, blobs, per-plugin SQLite — completes one turn,
// writes some more, and then abandons the engine mid-flight.
func runToKill(t *testing.T, backendName string) *killedSession {
	t.Helper()

	eng := resumeEngine(t, backendName, t.TempDir())

	var snapshots []events.SessionSnapshotResult
	eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
			snapshots = append(snapshots, r)
		}
	})

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	// Deliberately no t.Cleanup(Stop): see the file comment. Stopping here
	// would take a shutdown snapshot and make the turn-boundary snapshot
	// unnecessary, which is the one thing these tests exist to disprove.

	k := &killedSession{id: eng.Session.ID, sessionDir: eng.Session.RootDir}

	history, _ := resumeHistory(t)
	k.history = history
	if err := eng.Session.WriteFile("context/conversation.jsonl", history); err != nil {
		t.Fatalf("write history: %v", err)
	}

	k.artifacts = map[string][]byte{
		"files/report.md":             []byte("# Report\n\nthree findings\n"),
		"files/nested/deep/data.json": []byte(`{"finding":"the seam holds"}`),
		"context/scratch.txt":         []byte("working notes"),
	}
	for rel, body := range k.artifacts {
		if err := eng.Session.WriteFile(rel, body); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// Blobs go through the same constructor every blob-producing plugin uses
	// (blobs.New over SessionWorkspace.BlobsDir), so the test cannot pass
	// against a layout no plugin writes to.
	blobStore, err := blobs.New(eng.Session.BlobsDir(), 0)
	if err != nil {
		t.Fatalf("blobs.New: %v", err)
	}
	k.blobBytes = bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 128)
	k.blob, err = blobStore.Put(k.blobBytes, "image/png")
	if err != nil {
		t.Fatalf("blob put: %v", err)
	}

	// Enough rows to guarantee a WAL that has not been folded back into the
	// main database file — the state in which an un-checkpointed store.db
	// would be uploaded missing most of its content.
	st, err := eng.Storage.Open(storage.ScopeSession, resumePluginID)
	if err != nil {
		t.Fatalf("open session storage: %v", err)
	}
	k.dbRows = make(map[string]string, 500)
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("row-%04d", i)
		value := fmt.Sprintf("value-%04d-%s", i, strings.Repeat("x", 200))
		if err := st.Put(key, []byte(value)); err != nil {
			t.Fatalf("storage put: %v", err)
		}
		k.dbRows[key] = value
	}
	walPath := filepath.Join(eng.Session.RootDir, "plugins", resumePluginID, "store.db-wal")
	if _, err := os.Stat(walPath); err != nil {
		t.Fatalf("expected an un-checkpointed WAL beside the session store: %v", err)
	}

	// A real turn: the input that started it, then the boundary. The input
	// matters because Boot's crash-resume path re-fires the io.input of a turn
	// whose end is missing from the journal — see TestResumeDoesNotReRunTheCompletedTurn.
	if err := eng.Bus.Emit("io.input", events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "summarise the findings",
		SessionID:     k.id,
	}); err != nil {
		t.Fatalf("emit io.input: %v", err)
	}
	// journal.Coordinator calls a turn unfinished when it sees an
	// agent.turn.start with no matching end, so the start has to be in the
	// journal for the re-run assertion to have anything to detect.
	if err := eng.Bus.Emit("agent.turn.start", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion,
		TurnID:        "turn-1",
	}); err != nil {
		t.Fatalf("emit agent.turn.start: %v", err)
	}
	endTurn(t, eng, "turn-1")

	if len(snapshots) != 1 || !snapshots[0].OK {
		t.Fatalf("turn boundary produced snapshots %+v, want exactly one successful one", snapshots)
	}

	// The recovery point. Captured here, before the post-turn writes below, so
	// the restored tree can be compared against the exact state the last
	// completed turn left behind.
	k.fingerprint = treeFingerprint(t, k.sessionDir)
	k.journal = readTreeFile(t, k.sessionDir, "journal/events.jsonl")

	k.lostFile = "files/after-the-kill.md"
	if err := eng.Session.WriteFile(k.lostFile, []byte("written after the last completed turn")); err != nil {
		t.Fatalf("write post-turn file: %v", err)
	}
	k.lostKey = "row-after-the-kill"
	if err := st.Put(k.lostKey, []byte("uncommitted work")); err != nil {
		t.Fatalf("post-turn storage put: %v", err)
	}
	lost, err := blobStore.Put([]byte("blob written after the last completed turn"), "text/plain")
	if err != nil {
		t.Fatalf("post-turn blob put: %v", err)
	}
	k.lostBlobSHA = lost.SHA256

	// *** kill *** — the engine is dropped on the floor from here.
	return k
}

// treeFingerprint hashes every file in a session tree that the seam is allowed
// to sync. Content hashes rather than sizes or mtimes: a restored file with
// the right length and the wrong bytes is exactly the failure a weaker
// comparison would wave through.
func treeFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(rel)
		if objectStoreExcluded(slash) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[slash] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprinting %s: %v", root, err)
	}
	return out
}

func readTreeFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return data
}

// readRestoredDBRows opens a store.db with nothing beside it — no -wal, no
// -shm — which is the only way a hydrated database is ever presented, and
// checks it is a real, queryable SQLite database before reading it.
func readRestoredDBRows(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening restored database: %v", err)
	}
	defer db.Close()

	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil {
		t.Fatalf("integrity_check on the restored database: %v", err)
	}
	if check != "ok" {
		t.Fatalf("integrity_check = %q; the restored database is corrupt", check)
	}

	rows, err := db.Query(`SELECT k, v FROM kv`)
	if err != nil {
		t.Fatalf("querying restored database: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatalf("scanning restored row: %v", err)
		}
		out[key] = string(value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating restored rows: %v", err)
	}
	return out
}

// TestKillAndResumeRestoresIdenticalSessionState is the story: run a session,
// complete a turn, kill the process, boot a fresh engine over a clean
// filesystem, and get the same session back — history, artifacts, blobs and
// per-plugin SQLite alike.
func TestKillAndResumeRestoresIdenticalSessionState(t *testing.T) {
	backendName := "memory-" + t.Name()
	objectstoretest.RegisterMemory(t, backendName, nil)

	k := runToKill(t, backendName)

	// A brand-new data root: no session tree, no lock, no databases, no
	// blobs. Everything the resumed engine reads has to come back over the
	// seam.
	freshRoot := t.TempDir()
	eng := resumeEngine(t, backendName, freshRoot)
	eng.RecallSessionID = k.id

	// Subscribed before Boot, because both events fire during it.
	var replayed []events.Message
	var replays int
	eng.Bus.Subscribe("io.history.replay", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.HistoryReplay); ok {
			replays++
			replayed = r.Messages
		}
	})
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot resuming %q: %v", k.id, err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if eng.Session == nil || eng.Session.ID != k.id {
		t.Fatalf("resumed session = %+v, want ID %q", eng.Session, k.id)
	}
	if eng.Session.RootDir == k.sessionDir {
		t.Fatalf("resumed session reused the dead process's directory %q; the test proves nothing", k.sessionDir)
	}

	// --- history -------------------------------------------------------
	if got := readTreeFile(t, eng.Session.RootDir, "context/conversation.jsonl"); !bytes.Equal(got, k.history) {
		t.Errorf("restored conversation.jsonl =\n%s\nwant\n%s", got, k.history)
	}
	_, wantMessages := resumeHistory(t)
	if replays != 1 {
		t.Errorf("io.history.replay fired %d times on resume, want 1", replays)
	}
	if len(replayed) != len(wantMessages) {
		t.Fatalf("replayed %d messages, want %d", len(replayed), len(wantMessages))
	}
	for i, want := range wantMessages {
		if replayed[i].Role != want.Role || replayed[i].Content != want.Content {
			t.Errorf("replayed message %d = %+v, want %+v", i, replayed[i], want)
		}
	}

	// --- artifacts -----------------------------------------------------
	for rel, want := range k.artifacts {
		got, err := eng.Session.ReadFile(rel)
		if err != nil {
			t.Errorf("reading restored %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("restored %s = %q, want %q", rel, got, want)
		}
	}

	// --- blobs ---------------------------------------------------------
	blobStore, err := blobs.New(eng.Session.BlobsDir(), 0)
	if err != nil {
		t.Fatalf("blobs.New on the restored session: %v", err)
	}
	gotBlob, gotMedia, err := blobStore.Get(k.blob.SHA256)
	if err != nil {
		t.Fatalf("restored blob %s: %v", k.blob.SHA256, err)
	}
	if !bytes.Equal(gotBlob, k.blobBytes) {
		t.Errorf("restored blob differs: %d bytes, want %d", len(gotBlob), len(k.blobBytes))
	}
	if gotMedia != "image/png" {
		t.Errorf("restored blob media type = %q, want %q", gotMedia, "image/png")
	}

	// --- per-plugin SQLite ---------------------------------------------
	//
	// Opened through the engine's own storage manager rather than
	// database/sql, so this asserts the restored file is usable by the
	// production path — WAL re-enabled, sidecars recreated locally — not
	// merely readable by a test.
	st, err := eng.Storage.Open(storage.ScopeSession, resumePluginID)
	if err != nil {
		t.Fatalf("opening restored session storage: %v", err)
	}
	keys, err := st.List("")
	if err != nil {
		t.Fatalf("listing restored storage: %v", err)
	}
	if len(keys) != len(k.dbRows) {
		t.Errorf("restored store holds %d rows, want %d", len(keys), len(k.dbRows))
	}
	for key, want := range k.dbRows {
		got, ok, err := st.Get(key)
		if err != nil {
			t.Fatalf("restored storage get %s: %v", key, err)
		}
		if !ok {
			t.Errorf("restored store is missing %q", key)
			continue
		}
		if string(got) != want {
			t.Errorf("restored store[%q] = %q, want %q", key, got, want)
		}
	}
	var check string
	if err := st.DB().QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil {
		t.Fatalf("integrity_check through the storage manager: %v", err)
	}
	if check != "ok" {
		t.Errorf("integrity_check = %q on the resumed store", check)
	}

	// --- metadata ------------------------------------------------------
	meta, err := eng.Session.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if meta.ID != k.id {
		t.Errorf("restored metadata ID = %q, want %q", meta.ID, k.id)
	}
	if meta.TurnCount != 1 {
		t.Errorf("restored TurnCount = %d, want 1 — the resumed session forgot the turn it completed", meta.TurnCount)
	}

	// --- the recovery point --------------------------------------------
	//
	// Everything written after the last turn boundary must be gone. If any of
	// it survived, something other than the turn-boundary snapshot persisted
	// the session and the "at most the in-flight turn" guarantee is not the
	// one being tested.
	if eng.Session.FileExists(k.lostFile) {
		t.Errorf("%s survived the kill; it was written after the last completed turn", k.lostFile)
	}
	if _, ok, _ := st.Get(k.lostKey); ok {
		t.Errorf("storage key %q survived the kill; it was written after the last completed turn", k.lostKey)
	}
	if _, _, err := blobStore.Get(k.lostBlobSHA); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("blob written after the last completed turn survived the kill (err = %v)", err)
	}
}

// TestResumeDoesNotReRunTheCompletedTurn guards the ordering argument in
// installObjectStoreSnapshots across a real round trip. If the snapshot ran
// before the journal had been handed the agent.turn.end envelope, the restored
// journal would end mid-turn, Boot's crash-resume would re-fire the io.input,
// and every resume would silently re-run a turn the user already watched
// complete.
func TestResumeDoesNotReRunTheCompletedTurn(t *testing.T) {
	backendName := "memory-" + t.Name()
	objectstoretest.RegisterMemory(t, backendName, nil)

	k := runToKill(t, backendName)

	eng := resumeEngine(t, backendName, t.TempDir())
	eng.RecallSessionID = k.id

	var inputs []string
	eng.Bus.Subscribe("io.input", func(ev Event[any]) {
		if in, ok := ev.Payload.(events.UserInput); ok {
			inputs = append(inputs, in.Content)
		}
	})

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot resuming %q: %v", k.id, err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if len(inputs) != 0 {
		t.Errorf("resume re-fired io.input %v; the restored journal does not end at the completed turn boundary", inputs)
	}
}

// TestHydratedTreeMatchesTheKilledTree is the equality half of the acceptance
// criteria: not "the session opens" but "the bytes are the same". Run against
// the hydration step alone, before any engine has booted over the tree, so the
// comparison is not polluted by the resume's own writes (a fresh lock, the
// io.session.start envelope, the turn counter).
func TestHydratedTreeMatchesTheKilledTree(t *testing.T) {
	backendName := "memory-" + t.Name()
	objectstoretest.RegisterMemory(t, backendName, nil)

	k := runToKill(t, backendName)

	freshRoot := t.TempDir()
	eng := resumeEngine(t, backendName, freshRoot)
	if err := eng.openObjectStore(context.Background()); err != nil {
		t.Fatalf("openObjectStore: %v", err)
	}
	t.Cleanup(eng.releaseObjectStore)
	if err := eng.hydrateSessionTree(context.Background(), k.id); err != nil {
		t.Fatalf("hydrateSessionTree: %v", err)
	}

	restored := filepath.Join(freshRoot, "sessions", k.id)
	got := treeFingerprint(t, restored)

	dbRel := "plugins/" + resumePluginID + "/store.db"
	const journalRel = "journal/events.jsonl"

	for rel, want := range k.fingerprint {
		gotSum, ok := got[rel]
		if !ok {
			t.Errorf("hydrated tree is missing %q", rel)
			continue
		}
		switch rel {
		case dbRel:
			// Compared by content below, not by bytes: what was uploaded is a
			// checkpointed VACUUM INTO copy, so it is deliberately not
			// byte-identical to the live file it was taken from — that is the
			// whole point of the checkpoint.
		case journalRel:
			// The live journal kept growing after the snapshot's barrier (the
			// session.snapshot.result envelope the snapshot itself produced),
			// so the restored segment must be a prefix of it rather than equal
			// to it. A restored journal that is not a prefix means events were
			// lost or reordered.
		default:
			if gotSum != want {
				t.Errorf("hydrated %q has different content (sha %s, want %s)", rel, gotSum, want)
			}
		}
	}
	for rel := range got {
		if _, ok := k.fingerprint[rel]; !ok {
			t.Errorf("hydrated tree has a file the killed session did not: %q", rel)
		}
	}

	// Belt and braces on the categories the story names by hand, so a
	// fingerprint helper that silently walked nothing cannot pass this test.
	for _, rel := range []string{"context/conversation.jsonl", "files/report.md", "files/nested/deep/data.json", dbRel, journalRel} {
		if _, ok := got[rel]; !ok {
			t.Fatalf("hydrated tree has no %q; the comparison above was vacuous. tree = %v", rel, sortedKeys(got))
		}
	}
	if _, ok := got["blobs/"+k.blob.SHA256[:2]+"/"+k.blob.SHA256+".bin"]; !ok {
		var blobFiles []string
		for rel := range got {
			if strings.HasPrefix(rel, "blobs/") {
				blobFiles = append(blobFiles, rel)
			}
		}
		t.Errorf("hydrated tree has no blob for %s; blobs present = %v", k.blob.SHA256, blobFiles)
	}

	restoredJournal := readTreeFile(t, restored, journalRel)
	if !bytes.HasPrefix(k.journal, restoredJournal) {
		t.Errorf("restored journal is not a prefix of the killed one (%d vs %d bytes)",
			len(restoredJournal), len(k.journal))
	}
	if !bytes.Contains(restoredJournal, []byte(`"agent.turn.end"`)) {
		t.Error("restored journal does not contain the turn boundary it was snapshotted on")
	}

	if rows := readRestoredDBRows(t, filepath.Join(restored, filepath.FromSlash(dbRel))); len(rows) != len(k.dbRows) {
		t.Errorf("restored database holds %d rows, want %d — the WAL checkpoint did not survive the round trip", len(rows), len(k.dbRows))
	} else {
		for key, want := range k.dbRows {
			if rows[key] != want {
				t.Errorf("restored database row %q = %q, want %q", key, rows[key], want)
			}
		}
	}

	// The commit marker is a sibling key, never a member of the tree.
	for rel := range got {
		if strings.HasSuffix(rel, sessionSnapshotMarkerSuffix) {
			t.Errorf("commit marker %q hydrated into the session tree", rel)
		}
	}
	// Nothing excluded came down either — the hydration scrub is the last line
	// of defence for a bucket written by an older build.
	if _, err := os.Stat(filepath.Join(restored, sessionLockFilename)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("session.lock present in the hydrated tree (stat err = %v)", err)
	}
}

// sortedKeys renders a fingerprint map for a failure message. Sorted so a
// diff between two failing runs is readable.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Session isolation in the bucket
// ---------------------------------------------------------------------------

// TestHydrationDoesNotBleedBetweenSimilarSessionIDs is the engine-level half of
// a guarantee E1-S3 only pins per backend. objectstore.TrimKeyPrefix matches
// whole key segments, so "sessions/s1" must not match "sessions/s10" — but
// nothing until now asserted that the engine's own key scheme feeds it prefixes
// for which that is true. It would not be: a scheme that appended the ID
// without a separator, or a hydrate call that passed "sessions/" and filtered
// locally, would both pass every test in E1-S3 and leak one session's files
// into another's tree here.
func TestHydrationDoesNotBleedBetweenSimilarSessionIDs(t *testing.T) {
	backendName := "memory-" + t.Name()
	backend := objectstoretest.RegisterMemory(t, backendName, nil)

	// Three sessions whose IDs are prefixes of, or prefixed by, each other.
	for _, s := range []struct {
		id     string
		marker string
	}{
		{"s1", "belongs-to-s1"},
		{"s10", "belongs-to-s10"},
		{"s1-extra", "belongs-to-s1-extra"},
	} {
		dir := t.TempDir()
		writeSessionTree(t, dir, s.id, map[string]string{
			"files/owner.md":              s.marker,
			"plugins/nexus.x/notes.jsonl": s.marker,
			"context/conversation.jsonl":  `{"Role":"user","Content":"` + s.marker + `"}` + "\n",
		})
		if err := backend.SeedTree(sessionObjectKeyPrefix(s.id), dir); err != nil {
			t.Fatalf("seeding %s: %v", s.id, err)
		}
		// Each session's commit marker is a sibling key that shares the same
		// leading characters as the next session's tree.
		if err := backend.Seed(sessionSnapshotMarkerKey(s.id), []byte(`{"session_id":"`+s.id+`"}`)); err != nil {
			t.Fatalf("seeding marker for %s: %v", s.id, err)
		}
	}

	freshRoot := t.TempDir()
	eng := resumeEngine(t, backendName, freshRoot)
	if err := eng.openObjectStore(context.Background()); err != nil {
		t.Fatalf("openObjectStore: %v", err)
	}
	t.Cleanup(eng.releaseObjectStore)
	if err := eng.hydrateSessionTree(context.Background(), "s1"); err != nil {
		t.Fatalf("hydrateSessionTree: %v", err)
	}

	restored := filepath.Join(freshRoot, "sessions", "s1")
	got := treeFingerprint(t, restored)
	if len(got) == 0 {
		t.Fatal("hydrating s1 produced an empty tree")
	}
	for rel := range got {
		body := string(readTreeFile(t, restored, rel))
		if strings.Contains(body, "belongs-to-s10") || strings.Contains(body, "belongs-to-s1-extra") {
			t.Errorf("%q in session s1 carries another session's content: %q", rel, body)
		}
		if strings.HasSuffix(rel, sessionSnapshotMarkerSuffix) {
			t.Errorf("a commit marker hydrated into the session tree as %q", rel)
		}
		// A prefix match that was not segment-aware strips "sessions/s1" off
		// "sessions/s10/files/owner.md" and lands it at "0/files/owner.md".
		if strings.HasPrefix(rel, "0/") || strings.HasPrefix(rel, "-extra/") {
			t.Errorf("neighbouring session's object hydrated into s1 as %q", rel)
		}
	}
	if body := string(readTreeFile(t, restored, "files/owner.md")); body != "belongs-to-s1" {
		t.Errorf("files/owner.md = %q, want %q", body, "belongs-to-s1")
	}

	// The neighbours are untouched in the bucket — hydration must not consume
	// or move objects.
	if _, ok := backend.Get(sessionObjectKeyPrefix("s10") + "/files/owner.md"); !ok {
		t.Error("hydrating s1 removed s10's objects from the store")
	}
}

// TestHydrationDoesNotValidateTheCommitMarker documents current behaviour
// rather than desired behaviour.
//
// E1-S4 writes sessions/<id>.snapshot.json only after every other object is
// durable, so the marker always names the last COMPLETE snapshot. Nothing
// reads it back: hydrateSessionTree pulls whatever objects exist under the
// prefix, so a tree left mixed by an interrupted snapshot — some objects from
// snapshot N, some from a half-finished N+1 — hydrates silently and looks
// exactly like a good session.
//
// The assertion below is deliberately of the "this is what happens today"
// kind. Closing the gap is a production change (compare the tree against the
// marker, refuse or roll back when they disagree) and is out of scope for a
// test story; see the FOLLOWUPS note on E1-S5.
func TestHydrationDoesNotValidateTheCommitMarker(t *testing.T) {
	backendName := "memory-" + t.Name()
	backend := objectstoretest.RegisterMemory(t, backendName, nil)

	const sessionID = "sess-interrupted"
	dir := t.TempDir()
	writeSessionTree(t, dir, sessionID, map[string]string{
		"files/from-snapshot-1.md": "committed",
		// An object from a snapshot that never completed. A marker-validating
		// hydration would reject or ignore this file; today it is
		// indistinguishable from committed state.
		"files/from-half-finished-snapshot-2.md": "never committed",
	})
	if err := backend.SeedTree(sessionObjectKeyPrefix(sessionID), dir); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	// A marker describing the *previous*, smaller snapshot: fewer objects,
	// an earlier sequence, a different turn.
	marker := sessionSnapshotMarker{
		SchemaVersion: sessionSnapshotMarkerVersion,
		SessionID:     sessionID,
		KeyPrefix:     sessionObjectKeyPrefix(sessionID),
		Sequence:      1,
		Trigger:       snapshotTriggerTurn,
		TurnID:        "turn-1",
		Objects:       1,
	}
	body, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if err := backend.Seed(sessionSnapshotMarkerKey(sessionID), body); err != nil {
		t.Fatalf("seeding marker: %v", err)
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
	for _, rel := range []string{"files/from-snapshot-1.md", "files/from-half-finished-snapshot-2.md"} {
		if _, err := os.Stat(filepath.Join(restored, filepath.FromSlash(rel))); err != nil {
			t.Errorf("current behaviour is to hydrate every object under the prefix, but %q is missing: %v", rel, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Zero-impact default
// ---------------------------------------------------------------------------

// TestDefaultPathNeverTouchesARegisteredBackend is the other half of the
// story: an engine that names no backend must behave as though this subsystem
// does not exist, even when a backend is sitting in the process-global
// registry waiting to be selected.
//
// Registering one and then never naming it is the sharp version of the test.
// "No backend registered" would pass trivially; this fails if any code path
// resolves a default, falls back to "the only registered backend", or opens
// the registry speculatively at boot.
func TestDefaultPathNeverTouchesARegisteredBackend(t *testing.T) {
	backend := objectstoretest.NewMemory()
	var opens atomic.Int32
	name := "memory-" + t.Name()
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		opens.Add(1)
		return backend, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })

	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if eng.objectStore != nil {
		t.Fatal("a backend handle was installed for a config that names none")
	}

	// A full session's worth of the events the seam hangs off.
	if err := eng.Session.WriteFile("files/report.md", []byte("local only")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := eng.Storage.Open(storage.ScopeSession, resumePluginID)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	if err := st.Put("k", []byte("v")); err != nil {
		t.Fatalf("storage put: %v", err)
	}
	endTurn(t, eng, "turn-1")
	endTurn(t, eng, "turn-2")
	if err := eng.Bus.Emit("session.snapshot.request", events.SessionSnapshotRequest{
		SchemaVersion: events.SessionSnapshotRequestVersion,
		Reason:        "explicit request with no backend configured",
	}); err != nil {
		t.Errorf("session.snapshot.request with no backend = %v, want nil", err)
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := opens.Load(); got != 0 {
		t.Errorf("objectstore backend factory invoked %d times on the default path, want 0", got)
	}
	if got := backend.Counts(); got != (objectstoretest.Counts{}) {
		t.Errorf("registered-but-unselected backend saw traffic: %+v", got)
	}
	if backend.Len() != 0 {
		t.Errorf("registered-but-unselected backend holds %d objects", backend.Len())
	}
	if eng.objectStore != nil {
		t.Error("backend handle present after Stop on the default path")
	}
	// The session is still on local disk and complete, exactly as before the
	// seam existed.
	if _, err := os.Stat(filepath.Join(eng.Session.RootDir, "files", "report.md")); err != nil {
		t.Errorf("default-path session lost its artifact: %v", err)
	}
}

// TestShippedConfigsLeaveTheObjectStoreDisabled keeps the default *shipped*
// experience aligned with the default *code* path. A profile that quietly
// enabled a backend would change the behaviour of every suite that boots from
// configs/, which is precisely the "no changes to their expectations" clause
// of this story.
func TestShippedConfigsLeaveTheObjectStoreDisabled(t *testing.T) {
	// Tests run with the package directory as their working directory.
	configDir := filepath.Join("..", "..", "configs")
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatalf("read configs dir: %v", err)
	}
	seen := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		seen++
		t.Run(entry.Name(), func(t *testing.T) {
			cfg, err := LoadConfig(filepath.Join(configDir, entry.Name()))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Core.ObjectStore.Enabled() {
				t.Errorf("object store enabled (%+v); shipped profiles must leave the seam inert",
					cfg.Core.ObjectStore)
			}
		})
	}
	if seen == 0 {
		t.Fatalf("no YAML profiles found under %s; the check was vacuous", configDir)
	}
}
