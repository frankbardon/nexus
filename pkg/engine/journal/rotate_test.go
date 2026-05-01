package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotate_GrowsSegmentCount(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, WriterOptions{
		FsyncMode:   FsyncNone,
		RotateBytes: 1, // every turn boundary rotates
		BufferSize:  16,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Three turns, each ends with agent.turn.end. Active segment must
	// grow into events-001, events-002, events-003 over time.
	for i := uint64(1); i <= 6; i += 2 {
		w.Append(&Envelope{Seq: i, Type: "agent.turn.start"})
		w.Append(&Envelope{Seq: i + 1, Type: "agent.turn.end"})
		// Wait for drain to actually rotate before next batch — the
		// rotateBytes check fires from the same goroutine that writes,
		// but envelopes are queued asynchronously.
		time.Sleep(15 * time.Millisecond)
	}
	if err := w.Close(testCtx(t, 5*time.Second)); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	rotated := 0
	for _, e := range entries {
		if rotatedRe.MatchString(e.Name()) {
			rotated++
		}
	}
	if rotated < 2 {
		t.Errorf("expected >=2 rotated segments, got %d (entries: %v)", rotated, entryNames(entries))
	}
}

func TestRotate_PreservesTotalReadOrder(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, WriterOptions{
		FsyncMode:   FsyncNone,
		RotateBytes: 1,
		BufferSize:  16,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	for i := uint64(1); i <= 8; i += 2 {
		w.Append(&Envelope{Seq: i, Type: "agent.turn.start"})
		w.Append(&Envelope{Seq: i + 1, Type: "agent.turn.end"})
		time.Sleep(15 * time.Millisecond)
	}
	if err := w.Close(testCtx(t, 5*time.Second)); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var seqs []uint64
	_ = r.Iter(func(e Envelope) bool {
		seqs = append(seqs, e.Seq)
		return true
	})
	if len(seqs) != 8 {
		t.Fatalf("want 8 envelopes across all segments, got %d (%v)", len(seqs), seqs)
	}
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Errorf("read order: pos %d seq=%d", i, s)
		}
	}

	_ = filepath.Join // silence import
}
