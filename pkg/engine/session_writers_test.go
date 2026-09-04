package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from this package to the module root so the table's
// repo-relative Source paths can be checked against the tree they name.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// The table is only worth having if it is complete. A writer appearing without
// a decision is the exact regression this whole story exists to prevent, so
// the count is pinned rather than left open.
//
// Sixteen entries. Thirteen came from the survey's twelve writers — the
// journal row named two files (writer.go and rotate.go) and Source is the
// allowlist key the enforcement scan matches raw os.* calls against, so each
// file needs its own row; splitting that row is not adding a writer. Three
// more arrived when TestPluginRawWritesAreAnnouncedOrAllowlisted first ran
// against the whole of plugins/ rather than the survey's notes and found
// writers the hand survey had missed: codeexec's temp GOPATH, oneshot's
// transcript, and the second cache in rag/ingest. That is the enforcement
// test doing its job on its first execution, and the reason the count is
// pinned to the scan rather than to a document.
const sessionTreeWriterCount = 16

func TestSessionTreeWriters_AllAreDecided(t *testing.T) {
	writers := SessionTreeWriters()
	if len(writers) != sessionTreeWriterCount {
		t.Fatalf("SessionTreeWriters() has %d entries, want %d — a new bypassing "+
			"writer needs a disposition, not a bigger table with a stale comment",
			len(writers), sessionTreeWriterCount)
	}

	valid := map[SessionWriterDisposition]bool{
		DispositionEmit:         true,
		DispositionTurnBoundary: true,
		DispositionExcluded:     true,
		DispositionWriteThrough: true,
	}
	seen := map[string]bool{}
	root := repoRoot(t)

	for _, w := range writers {
		if w.Source == "" {
			t.Errorf("entry with empty Source: %+v", w)
			continue
		}
		if seen[w.Source] {
			t.Errorf("duplicate Source %q", w.Source)
		}
		seen[w.Source] = true

		if !valid[w.Disposition] {
			t.Errorf("%s: disposition %q is not one of emit / turn-boundary-only / "+
				"excluded-by-design / write-through", w.Source, w.Disposition)
		}
		// "Undecided" is the state this table removes, and an empty Why is
		// indistinguishable from it.
		if len(strings.TrimSpace(w.Why)) < 40 {
			t.Errorf("%s: Why is too thin to be a decision — record the reasoning "+
				"and the rejected alternative, not the behaviour", w.Source)
		}
		if w.Writes == "" {
			t.Errorf("%s: Writes is empty; say what lands where", w.Source)
		}
		// A Source that no longer exists means the file was moved or deleted
		// and the disposition silently stopped describing anything.
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(w.Source))); err != nil {
			t.Errorf("%s: Source does not exist: %v", w.Source, err)
		}
	}
}

// The two exclusions are the load-bearing half of the table: everything else
// still reaches the store eventually, and these two must not. Pinned by name
// so a well-meaning "make it consistent" edit that downgrades them to
// turn-boundary-only fails here instead of shipping a corrupt store.db or a
// permanently locked resumed session.
func TestSessionTreeWriters_ExclusionsStayExcluded(t *testing.T) {
	want := map[string]string{
		"pkg/engine/storage/sqlite.go": "WAL",
		"pkg/engine/session_lock.go":   "PID",
	}
	found := map[string]bool{}

	for _, w := range SessionTreeWriters() {
		reason, ok := want[w.Source]
		if !ok {
			continue
		}
		found[w.Source] = true
		if w.Disposition != DispositionExcluded {
			t.Errorf("%s: disposition = %q, want %q — these bytes describe the host, "+
				"not the session", w.Source, w.Disposition, DispositionExcluded)
		}
		// The criterion is that the exclusion is commented with the *reason*,
		// not the behaviour; the reason in each case turns on one word.
		if !strings.Contains(w.Why, reason) {
			t.Errorf("%s: Why does not mention %q, so it records what happens "+
				"rather than why", w.Source, reason)
		}
	}
	for src := range want {
		if !found[src] {
			t.Errorf("%s is missing from SessionTreeWriters()", src)
		}
	}
}

// A writer marked "emit" that does not actually announce anything is worse
// than one marked turn-boundary-only, because the table then licenses a sync
// backend to trust real-time events that never arrive. Checked by looking for
// an Announce call in the file the entry names — coarse, but it catches the
// realistic failure, which is an announcement removed during a refactor while
// the table entry stays behind.
func TestSessionTreeWriters_EmitEntriesActuallyAnnounce(t *testing.T) {
	root := repoRoot(t)
	for _, w := range SessionTreeWriters() {
		if w.Disposition != DispositionEmit {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(w.Source)))
		if err != nil {
			t.Errorf("%s: %v", w.Source, err)
			continue
		}
		body := string(src)
		if !strings.Contains(body, "AnnounceWrite") &&
			!strings.Contains(body, "AnnounceAppend") &&
			!strings.Contains(body, "s.announce(") {
			t.Errorf("%s is marked %q but calls neither AnnounceWrite nor "+
				"AnnounceAppend", w.Source, DispositionEmit)
		}
	}
}

// A "write-through" row makes a stronger claim than any other: it says the
// bytes are already on their way to the store and that no subscriber will ever
// hear about them, so a reader who trusts the table stops looking for events.
// The claim is only safe where the object key is derived from the content —
// otherwise "push it the instant it lands" is a race with the turn-boundary
// snapshot rather than a duplicate upload. Pinned by name for the one writer
// that qualifies, and checked against the mechanism it names: a hook, not an
// emission.
func TestSessionTreeWriters_WriteThroughIsBlobsAndIsHookDriven(t *testing.T) {
	root := repoRoot(t)
	var found []SessionTreeWriter
	for _, w := range SessionTreeWriters() {
		if w.Disposition == DispositionWriteThrough {
			found = append(found, w)
		}
	}
	if len(found) != 1 || found[0].Source != "pkg/engine/blobs/blobs.go" {
		t.Fatalf("write-through rows = %+v, want exactly pkg/engine/blobs/blobs.go — "+
			"content-addressing is what makes a barrier-free push safe, and nothing "+
			"else under a session tree has it", found)
	}
	w := found[0]

	// The eviction decision is the one a future edit is most likely to get
	// wrong, because "keep the bucket in sync with local disk" sounds right
	// until you notice the LRU sweep exists to bound disk and a bucket has no
	// such bound.
	if !strings.Contains(w.Why, "eviction") {
		t.Errorf("%s: Why does not record the eviction decision; local LRU eviction "+
			"must not delete the remote object", w.Source)
	}

	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(w.Source)))
	if err != nil {
		t.Fatalf("%s: %v", w.Source, err)
	}
	body := string(src)
	if !strings.Contains(body, "PutHook") {
		t.Errorf("%s is marked %q but exposes no PutHook for the engine to push "+
			"through", w.Source, DispositionWriteThrough)
	}
	// The whole point of a hook rather than an event: the package must keep
	// working outside an engine, which it cannot do if it learns about a bus.
	// Checked as an import rather than as a mention, because the package doc
	// legitimately names engine symbols in prose.
	if strings.Contains(body, "github.com/frankbardon/nexus") {
		t.Errorf("%s imports from the Nexus tree; the blob store must stay a "+
			"standalone stdlib-only package, which is what a plain func hook "+
			"preserves and what an event bus would have cost", w.Source)
	}
}
