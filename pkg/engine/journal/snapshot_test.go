package journal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func env(seq uint64, typ string) *Envelope {
	return &Envelope{
		Seq:     seq,
		Ts:      time.Unix(0, int64(seq)),
		Type:    typ,
		EventID: fmt.Sprintf("evt-%d", seq),
		Payload: map[string]any{"pad": strings.Repeat("x", 512)},
	}
}

// seqsIn reads every JSONL record in one uncompressed segment and returns the
// seqs it holds.
func seqsIn(t *testing.T, path string) []uint64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Envelope
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
		out = append(out, e.Seq)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

// Barrier is the reason a turn-boundary snapshot sees the turn that triggered
// it: appends are asynchronous, so without it the file on disk trails whatever
// is still queued.
func TestBarrierMakesQueuedEnvelopesDurable(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)
	t.Cleanup(func() { mustClose(t, w) })

	const n = 50
	for i := uint64(1); i <= n; i++ {
		w.Append(env(i, "test.event"))
	}
	if err := w.Barrier(context.Background()); err != nil {
		t.Fatalf("Barrier: %v", err)
	}

	got := seqsIn(t, filepath.Join(dir, activeSegmentName))
	if len(got) != n {
		t.Fatalf("after Barrier the active segment holds %d envelopes, want %d", len(got), n)
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Fatalf("envelope %d has seq %d, want %d", i, seq, i+1)
		}
	}
}

// Barrier on a closed writer is a no-op rather than a hang or an error: Close
// already drained, flushed and synced, so its promise is already met. The
// shutdown snapshot relies on this.
func TestBarrierAfterClose(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)
	w.Append(env(1, "test.event"))
	mustClose(t, w)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Barrier(ctx); err != nil {
		t.Errorf("Barrier after Close = %v, want nil", err)
	}
}

func TestBarrierOnNilWriter(t *testing.T) {
	var w *Writer
	if err := w.Barrier(context.Background()); err != nil {
		t.Errorf("Barrier on a nil writer = %v, want nil", err)
	}
}

// Snapshot reports the immutable files by their live paths and stages the one
// mutable file, and it ignores subdirectories — journal/cache/ is ordinary tree
// data that a caller walks on its own.
func TestSnapshotSeparatesImmutableFromMutable(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)
	t.Cleanup(func() { mustClose(t, w) })

	if err := os.MkdirAll(filepath.Join(dir, "cache", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache", "sub", "x.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := uint64(1); i <= 10; i++ {
		w.Append(env(i, "test.event"))
	}
	if err := w.Barrier(context.Background()); err != nil {
		t.Fatalf("Barrier: %v", err)
	}

	stage := filepath.Join(t.TempDir(), "journal")
	files, err := w.Snapshot(stage)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	byName := map[string]SnapshotFile{}
	for _, f := range files {
		byName[f.Name] = f
	}
	if len(byName) != 2 {
		t.Fatalf("Snapshot returned %v, want exactly header.json and events.jsonl", byName)
	}

	header, ok := byName[headerName]
	if !ok {
		t.Fatal("Snapshot omitted header.json")
	}
	if header.Staged {
		t.Error("header.json was staged; it is immutable and should be read in place")
	}
	if header.Path != filepath.Join(dir, headerName) {
		t.Errorf("header path = %q, want the live file", header.Path)
	}

	active, ok := byName[activeSegmentName]
	if !ok {
		t.Fatal("Snapshot omitted the active segment")
	}
	if !active.Staged {
		t.Error("the active segment was not staged; a rotation could truncate it under the reader")
	}
	if !strings.HasPrefix(active.Path, stage) {
		t.Errorf("staged active segment at %q, want it under %q", active.Path, stage)
	}
	if got := len(seqsIn(t, active.Path)); got != 10 {
		t.Errorf("staged active segment holds %d envelopes, want 10", got)
	}
	if active.Size <= 0 {
		t.Errorf("Size = %d, want the captured size", active.Size)
	}
}

// The case FR-19 is about: a rotation between two snapshots must leave every
// envelope in the union of the captured instants exactly once — neither lost to
// a truncate that happened between listing and reading, nor duplicated because
// both the new .zst and the pre-truncate active segment were captured.
func TestSnapshotAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	// Small rotate threshold so one turn boundary is enough to trigger it.
	w, err := NewWriter(dir, WriterOptions{
		FsyncMode:     FsyncNone,
		RotateBytes:   4 << 10,
		BufferSize:    64,
		SchemaVersion: SchemaVersion,
		SessionID:     "rotate-session",
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { mustClose(t, w) })

	var seq uint64
	appendTurn := func(n int) {
		for i := 0; i < n; i++ {
			seq++
			w.Append(env(seq, "test.event"))
		}
		seq++
		w.Append(env(seq, "agent.turn.end"))
	}

	// First turn: enough bytes to push the active segment past the threshold,
	// so the agent.turn.end that closes it triggers a rotation.
	appendTurn(20)
	if err := w.Barrier(context.Background()); err != nil {
		t.Fatalf("Barrier: %v", err)
	}

	stage1 := filepath.Join(t.TempDir(), "j1")
	first, err := w.Snapshot(stage1)
	if err != nil {
		t.Fatalf("Snapshot 1: %v", err)
	}

	appendTurn(20)
	if err := w.Barrier(context.Background()); err != nil {
		t.Fatalf("Barrier: %v", err)
	}
	stage2 := filepath.Join(t.TempDir(), "j2")
	second, err := w.Snapshot(stage2)
	if err != nil {
		t.Fatalf("Snapshot 2: %v", err)
	}

	// A rotation really did happen between the two instants; otherwise this
	// test proves nothing.
	rotated := 0
	for _, f := range second {
		if rotatedRe.MatchString(f.Name) {
			rotated++
		}
	}
	if rotated == 0 {
		t.Fatal("no rotated segment appeared; the rotation this test exists for did not happen")
	}

	// Simulate the remote object store: later snapshots overwrite the objects
	// of earlier ones, keyed by name.
	remote := map[string]string{}
	for _, f := range first {
		remote[f.Name] = f.Path
	}
	for _, f := range second {
		remote[f.Name] = f.Path
	}

	seen := map[uint64]int{}
	for name, path := range remote {
		if name == headerName {
			continue
		}
		var seqs []uint64
		if strings.HasSuffix(name, ".zst") {
			seqs = seqsInCompressed(t, path)
		} else {
			seqs = seqsIn(t, path)
		}
		for _, s := range seqs {
			seen[s]++
		}
	}

	for s := uint64(1); s <= seq; s++ {
		switch seen[s] {
		case 1:
		case 0:
			t.Errorf("seq %d is missing from the remote journal", s)
		default:
			t.Errorf("seq %d appears %d times in the remote journal", s, seen[s])
		}
	}
}

func TestSnapshotOnNilWriter(t *testing.T) {
	var w *Writer
	files, err := w.Snapshot("anywhere")
	if err != nil || files != nil {
		t.Errorf("Snapshot on a nil writer = (%v, %v), want (nil, nil)", files, err)
	}
}

func TestSnapshotRequiresStagingDir(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)
	t.Cleanup(func() { mustClose(t, w) })
	if _, err := w.Snapshot(""); err == nil {
		t.Error("Snapshot with no staging dir = nil, want an error")
	}
}

// seqsInCompressed is seqsIn for a rotated (zstd) segment.
func seqsInCompressed(t *testing.T, path string) []uint64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatalf("zstd reader %s: %v", path, err)
	}
	defer dec.Close()

	var out []uint64
	sc := bufio.NewScanner(dec.IOReadCloser())
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Envelope
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
		out = append(out, e.Seq)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}
