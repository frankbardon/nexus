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

// TestJournalExclude_CoreTickSuppressed runs a multi-turn dialogue with the
// engine heartbeat firing every 100ms. The journal must contain zero
// core.tick envelopes and the on-disk seq sequence must remain gap-free,
// proving the bus-side seq elision keeps the writer's reorder buffer happy
// even when the suppressed event type interleaves heavily with normal
// dispatch.
func TestJournalExclude_CoreTickSuppressed(t *testing.T) {
	h := testharness.New(t, "configs/test-journal-exclude.yaml",
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
		t.Fatal("journal is empty")
	}

	for i, env := range envs {
		want := uint64(i + 1)
		if env.Seq != want {
			t.Errorf("seq gap at pos %d: got %d want %d (type=%s) — exclusion broke contiguity",
				i, env.Seq, want, env.Type)
		}
	}

	for _, env := range envs {
		if env.Type == "core.tick" {
			t.Fatalf("core.tick envelope leaked into journal (seq=%d)", env.Seq)
		}
	}
}
