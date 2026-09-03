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

// The table is only worth having if it is complete. The survey that produced
// it found twelve writers bypassing the SessionWorkspace helpers; a new one
// appearing without a decision is the exact regression this whole story exists
// to prevent, so the count is pinned rather than left open.
//
// Thirteen entries for twelve writers: the survey's journal row named two
// files (writer.go and rotate.go) and Source is the allowlist key an
// enforcement test matches raw os.* calls against, so each file needs its own
// row. Splitting the row is not adding a writer.
const sessionTreeWriterCount = 13

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
				"excluded-by-design", w.Source, w.Disposition)
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
