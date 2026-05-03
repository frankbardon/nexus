package journal

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestRewindTruncatesAndArchives writes a small journal, rewinds it to a
// mid-stream seq, and verifies the live journal contains only the kept
// envelopes while the archive holds the original.
func TestRewindTruncatesAndArchives(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")

	w, err := NewWriter(journalDir, WriterOptions{
		FsyncMode:  FsyncEveryEvent,
		BufferSize: 4,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i := 1; i <= 6; i++ {
		w.Append(&Envelope{
			Seq:  uint64(i),
			Ts:   time.Unix(int64(1700000000+i), 0).UTC(),
			Type: "test.event",
		})
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, err := Rewind(journalDir, 3)
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if res.TruncatedSeq != 3 {
		t.Fatalf("TruncatedSeq = %d; want 3", res.TruncatedSeq)
	}
	if res.EventsKept != 3 {
		t.Fatalf("EventsKept = %d; want 3", res.EventsKept)
	}
	if res.EventsArchived != 6 {
		t.Fatalf("EventsArchived = %d; want 6", res.EventsArchived)
	}
	if res.ArchiveName == "" {
		t.Fatal("expected non-empty ArchiveName")
	}

	// Live journal should now hold only seqs 1..3.
	r, err := Open(journalDir)
	if err != nil {
		t.Fatalf("Open after rewind: %v", err)
	}
	var liveSeqs []uint64
	if err := r.Iter(func(env Envelope) bool {
		liveSeqs = append(liveSeqs, env.Seq)
		return true
	}); err != nil {
		t.Fatalf("Iter: %v", err)
	}
	if want := []uint64{1, 2, 3}; !equalU64(liveSeqs, want) {
		t.Fatalf("live seqs = %v; want %v", liveSeqs, want)
	}

	// Archive should hold the original 1..6.
	archived, err := Open(filepath.Join(journalDir, archiveDirName, res.ArchiveName))
	if err != nil {
		t.Fatalf("Open archive: %v", err)
	}
	var archivedSeqs []uint64
	if err := archived.Iter(func(env Envelope) bool {
		archivedSeqs = append(archivedSeqs, env.Seq)
		return true
	}); err != nil {
		t.Fatalf("archived Iter: %v", err)
	}
	if want := []uint64{1, 2, 3, 4, 5, 6}; !equalU64(archivedSeqs, want) {
		t.Fatalf("archived seqs = %v; want %v", archivedSeqs, want)
	}

	// Archives listing should include the rewind archive.
	names, err := ListArchives(journalDir)
	if err != nil {
		t.Fatalf("ListArchives: %v", err)
	}
	if len(names) != 1 || names[0] != res.ArchiveName {
		t.Fatalf("ListArchives = %v; want [%s]", names, res.ArchiveName)
	}
}

// TestRestoreReversesRewind rewinds, restores, and confirms the live
// journal matches the original. Verifies that the pre-restore archive is
// also created so a second flip would still be reversible.
func TestRestoreReversesRewind(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")

	w, err := NewWriter(journalDir, WriterOptions{FsyncMode: FsyncEveryEvent, BufferSize: 4})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i := 1; i <= 5; i++ {
		w.Append(&Envelope{Seq: uint64(i), Ts: time.Unix(int64(1700000000+i), 0).UTC(), Type: "test.event"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, err := Rewind(journalDir, 2)
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}

	if err := Restore(journalDir, res.ArchiveName); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	r, err := Open(journalDir)
	if err != nil {
		t.Fatalf("Open after restore: %v", err)
	}
	var seqs []uint64
	if err := r.Iter(func(env Envelope) bool {
		seqs = append(seqs, env.Seq)
		return true
	}); err != nil {
		t.Fatalf("Iter: %v", err)
	}
	if want := []uint64{1, 2, 3, 4, 5}; !equalU64(seqs, want) {
		t.Fatalf("live seqs = %v; want %v", seqs, want)
	}

	// Two archives expected: the rewind snapshot and the pre-restore rotation.
	names, err := ListArchives(journalDir)
	if err != nil {
		t.Fatalf("ListArchives: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("ListArchives = %v; want 2 entries", names)
	}
}

// TestRewindRejectsMissingSeq surfaces the "no envelopes <= toSeq" guard.
func TestRewindRejectsMissingSeq(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	w, err := NewWriter(journalDir, WriterOptions{FsyncMode: FsyncEveryEvent, BufferSize: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Append(&Envelope{Seq: 5, Ts: time.Now().UTC(), Type: "test"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := Rewind(journalDir, 4); err == nil {
		t.Fatal("expected error when no envelopes <= toSeq, got nil")
	}
}

func equalU64(a, b []uint64) bool {
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
