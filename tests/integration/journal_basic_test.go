//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestJournalBasic_AllEventsRecorded verifies that the always-on journal
// captures every dispatched event for a 2-turn mock dialogue, that seq is
// gap-free monotonic, and that parent_seq points back to a real prior seq
// when set.
func TestJournalBasic_AllEventsRecorded(t *testing.T) {
	h := testharness.New(t, "configs/test-journal-basic.yaml",
		testharness.WithTimeout(20*time.Second),
		testharness.WithKeepSession(),
	)
	h.Run()

	sessionDir := h.SessionDir()
	if sessionDir == "" {
		t.Fatal("no session dir")
	}
	t.Cleanup(func() { os.RemoveAll(sessionDir) })
	journalDir := filepath.Join(sessionDir, "journal")

	r, err := journal.Open(journalDir)
	if err != nil {
		t.Fatalf("open journal at %s: %v", journalDir, err)
	}

	var envs []journal.Envelope
	if err := r.Iter(func(e journal.Envelope) bool {
		envs = append(envs, e)
		return true
	}); err != nil {
		t.Fatalf("iter journal: %v", err)
	}
	if len(envs) == 0 {
		t.Fatal("journal is empty — writer did not subscribe")
	}

	// Seq monotonicity, gap-free.
	for i, env := range envs {
		want := uint64(i + 1)
		if env.Seq != want {
			t.Errorf("seq gap at pos %d: got %d want %d (type=%s)", i, env.Seq, want, env.Type)
		}
	}

	// parent_seq, when set, must reference a prior seq.
	seen := make(map[uint64]bool, len(envs))
	for _, env := range envs {
		if env.ParentSeq != 0 && !seen[env.ParentSeq] {
			t.Errorf("envelope seq=%d type=%s has parent_seq=%d that has not been seen", env.Seq, env.Type, env.ParentSeq)
		}
		seen[env.Seq] = true
	}

	// Sanity: at least one llm.request, one llm.response, two io.input, two
	// io.output (assistant role) — the dialogue dispatched these on the bus.
	counts := map[string]int{}
	for _, env := range envs {
		counts[env.Type]++
	}
	// Mock IO short-circuits before:llm.request, so llm.request itself
	// never fires; llm.response does. The before:llm.request envelope
	// should carry vetoed=true.
	for _, mustHave := range []string{
		"io.session.start",
		"io.input",
		"before:llm.request",
		"llm.response",
		"io.output",
		"agent.turn.start",
		"agent.turn.end",
	} {
		if counts[mustHave] == 0 {
			t.Errorf("expected at least one %q in journal, got 0 (counts=%v)", mustHave, counts)
		}
	}

	// Vetoable events emit wildcard envelopes too; mock IO vetoes
	// before:llm.request, so at least one envelope must be vetoed=true.
	vetoCount := 0
	for _, env := range envs {
		if env.Vetoed {
			vetoCount++
		}
	}
	if vetoCount == 0 {
		t.Error("expected at least one vetoed=true envelope (before:llm.request) but found none")
	}

	// Header round-trip.
	if h := r.Header(); h.SchemaVersion != journal.SchemaVersion {
		t.Errorf("header schema_version=%q want %q", h.SchemaVersion, journal.SchemaVersion)
	}
}
