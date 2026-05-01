package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweep_RemovesAgedJournal(t *testing.T) {
	root := t.TempDir()

	// Aged session journal — set mtime to 60 days ago.
	agedJournal := filepath.Join(root, "session-aged", "journal")
	if err := os.MkdirAll(agedJournal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agedJournal, "events.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(agedJournal, old, old); err != nil {
		t.Fatal(err)
	}

	// Recent session journal — left alone.
	freshJournal := filepath.Join(root, "session-fresh", "journal")
	if err := os.MkdirAll(freshJournal, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Sweep(root, 30); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, err := os.Stat(agedJournal); !os.IsNotExist(err) {
		t.Errorf("aged journal still exists: err=%v", err)
	}
	if _, err := os.Stat(freshJournal); err != nil {
		t.Errorf("fresh journal removed: %v", err)
	}
}

func TestSweep_NoOpZeroRetention(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "session-x", "journal")
	_ = os.MkdirAll(dir, 0o755)
	old := time.Now().Add(-365 * 24 * time.Hour)
	_ = os.Chtimes(dir, old, old)

	if err := Sweep(root, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir removed despite retain=0: %v", err)
	}
}

func TestSweep_MissingRoot(t *testing.T) {
	if err := Sweep(filepath.Join(os.TempDir(), "nexus-journal-not-real"), 30); err != nil {
		t.Errorf("expected nil for missing root, got %v", err)
	}
}
