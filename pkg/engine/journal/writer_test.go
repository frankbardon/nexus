package journal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestWriter(t *testing.T, dir string, mode FsyncMode) *Writer {
	t.Helper()
	w, err := NewWriter(dir, WriterOptions{
		FsyncMode:     mode,
		RotateBytes:   1 << 30, // disable rotation in basic tests
		BufferSize:    32,
		SchemaVersion: SchemaVersion,
		SessionID:     "test-session",
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	return w
}

func mustClose(t *testing.T, w *Writer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestWriter_AppendAndReopen(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)

	for i := uint64(1); i <= 5; i++ {
		w.Append(&Envelope{
			Seq:     i,
			Ts:      time.Unix(0, int64(i)),
			Type:    "test.event",
			EventID: "evt-" + string(rune('0'+i)),
			Payload: map[string]int{"i": int(i)},
		})
	}
	mustClose(t, w)

	// Reopen and read back.
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var got []Envelope
	if err := r.Iter(func(e Envelope) bool {
		got = append(got, e)
		return true
	}); err != nil {
		t.Fatalf("Iter: %v", err)
	}

	if len(got) != 5 {
		t.Fatalf("expected 5 envelopes, got %d", len(got))
	}
	for i, env := range got {
		want := uint64(i + 1)
		if env.Seq != want {
			t.Errorf("envelope %d: want seq=%d got %d", i, want, env.Seq)
		}
	}
}

func TestWriter_HeaderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncTurnBoundary)
	mustClose(t, w)

	data, err := os.ReadFile(filepath.Join(dir, "header.json"))
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	var h Header
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if h.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version=%q", h.SchemaVersion)
	}
	if h.FsyncMode != string(FsyncTurnBoundary) {
		t.Errorf("fsync_mode=%q", h.FsyncMode)
	}
	if h.SessionID != "test-session" {
		t.Errorf("session_id=%q", h.SessionID)
	}
}

func TestWriter_RejectsSchemaMismatch(t *testing.T) {
	dir := t.TempDir()
	// Write a header with a different schema_version.
	hdr := Header{SchemaVersion: "999", CreatedAt: time.Now()}
	data, _ := json.Marshal(hdr)
	if err := os.WriteFile(filepath.Join(dir, "header.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(dir, WriterOptions{SchemaVersion: SchemaVersion}); err == nil {
		t.Fatal("expected schema mismatch error")
	}
}

func TestWriter_OutOfOrderArrival(t *testing.T) {
	// Wildcards-after-typed dispatch order means a nested emit's envelope
	// arrives on the writer channel before its parent's. The drain
	// reorders by seq before writing. Verify by appending out-of-order.
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)

	w.Append(&Envelope{Seq: 2, Type: "child"})
	w.Append(&Envelope{Seq: 1, Type: "parent"})
	w.Append(&Envelope{Seq: 3, Type: "next"})
	mustClose(t, w)

	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var seqs []uint64
	_ = r.Iter(func(e Envelope) bool {
		seqs = append(seqs, e.Seq)
		return true
	})
	if len(seqs) != 3 {
		t.Fatalf("expected 3 envelopes, got %d", len(seqs))
	}
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Errorf("ordering: pos %d seq=%d (want %d)", i, s, i+1)
		}
	}
}

func TestWriter_AppendAfterClose(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)
	mustClose(t, w)

	// Should not panic; should silently drop.
	w.Append(&Envelope{Seq: 1, Type: "after-close"})
}
