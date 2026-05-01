package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReader_SkipMalformedLine(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)
	w.Append(&Envelope{Seq: 1, Type: "ok.first"})
	w.Append(&Envelope{Seq: 2, Type: "ok.second"})
	mustClose(t, w)

	// Corrupt the file by injecting a malformed line in the middle.
	path := filepath.Join(dir, activeSegmentName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Append a malformed line followed by a valid one.
	data = append(data, []byte("{not-json\n")...)
	good, _ := json.Marshal(Envelope{Seq: 3, Type: "ok.third"})
	data = append(data, good...)
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var types []string
	_ = r.Iter(func(e Envelope) bool {
		types = append(types, e.Type)
		return true
	})

	want := []string{"ok.first", "ok.second", "ok.third"}
	if len(types) != len(want) {
		t.Fatalf("got %d events, want %d (%v)", len(types), len(want), types)
	}
	for i, ty := range types {
		if ty != want[i] {
			t.Errorf("pos %d: got %q want %q", i, ty, want[i])
		}
	}
}

func TestReader_RotatedSegmentsInOrder(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, WriterOptions{
		FsyncMode:   FsyncNone,
		RotateBytes: 1, // rotate aggressively on every agent.turn.end
		BufferSize:  16,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Two turns, each flushed by an agent.turn.end. Rotation triggers on
	// the boundary, so seqs 1-2 land in events-001 and seqs 3-4 in active.
	w.Append(&Envelope{Seq: 1, Type: "agent.turn.start"})
	w.Append(&Envelope{Seq: 2, Type: "agent.turn.end"})
	// Give the drain a moment to write+rotate before queueing more, since
	// rotation runs on the same goroutine as writeOne under the writer
	// mutex but reads the file size for the trigger check.
	time.Sleep(20 * time.Millisecond)
	w.Append(&Envelope{Seq: 3, Type: "agent.turn.start"})
	w.Append(&Envelope{Seq: 4, Type: "agent.turn.end"})

	mustClose(t, w)

	// Verify rotated file present.
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
	if rotated == 0 {
		t.Fatalf("expected rotated segment, dir contents: %v", entryNames(entries))
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
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Errorf("seq order: pos %d got %d want %d (all=%v)", i, s, i+1, seqs)
		}
	}
}

func TestReader_LastTurnBoundary(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)
	w.Append(&Envelope{Seq: 1, Type: "agent.turn.start"})
	w.Append(&Envelope{Seq: 2, Type: "tool.invoke"})
	w.Append(&Envelope{Seq: 3, Type: "agent.turn.end"})
	w.Append(&Envelope{Seq: 4, Type: "agent.turn.start"})
	w.Append(&Envelope{Seq: 5, Type: "tool.invoke"})
	mustClose(t, w)

	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	last, ok, err := r.LastTurnBoundary()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || last != 3 {
		t.Errorf("last turn boundary: got (%d, %v), want (3, true)", last, ok)
	}
}

func entryNames(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
