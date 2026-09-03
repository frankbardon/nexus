package journal

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// This file exists for one reason: the journal directory is the only part of a
// session tree that *rewrites itself* while something else is trying to copy
// it.
//
// Rotation (rotate.go) compresses events.jsonl into the next
// events-NNN.jsonl.zst slot and then truncates the active segment to zero, and
// it runs on the drain goroutine the instant an agent.turn.end envelope lands.
// A turn-boundary snapshot runs on the bus goroutine reacting to that same
// agent.turn.end. The two therefore race by construction, and the race has two
// distinct bad outcomes:
//
//   - Read events.jsonl *after* the truncate but list the directory *before*
//     the new .zst appeared, and the turn's events exist in neither uploaded
//     object: a missing segment.
//   - List the new .zst *and* read a not-yet-truncated events.jsonl, and the
//     same events land twice: a duplicated segment. Replay would then see
//     every one of those envelopes a second time.
//
// Snapshot resolves both by capturing a single consistent instant under the
// writer's file mutex — the same mutex rotation holds — and by drawing the
// obvious line between the two kinds of file in the directory. Rotated
// segments and header.json are immutable once written, so they are reported by
// their live paths and may be read at leisure afterwards. events.jsonl is the
// one mutable file, so it is copied while the lock is held.
//
// The rejected alternative was to hold the writer's mutex for the whole
// upload. That is trivially correct and blocks every bus dispatch behind a
// network round trip.

// SnapshotFile names one file in a consistent instant of a journal directory.
type SnapshotFile struct {
	// Name is the base name within the journal directory. Callers turn it
	// into a destination path or object key themselves; the journal package
	// knows nothing about either.
	Name string
	// Path is where the bytes actually live. For an immutable segment this is
	// the live file; for the active segment it is the staged copy.
	Path string
	// Staged reports whether Path is a copy Snapshot made (and the caller is
	// responsible for cleaning up) rather than a file in the journal dir.
	Staged bool
	// Size is the file size at capture time.
	Size int64
}

// Snapshot captures a consistent instant of the journal's segment files,
// staging the mutable active segment into stageDir.
//
// Subdirectories are ignored: the tool-result cache lives at journal/cache/
// and is ordinary, independently-written data that a caller walking the
// session tree picks up on its own. Only the top-level segment files — which
// rotation mutates as a set — need this treatment.
//
// The caller owns stageDir and must remove it when done. A nil Writer yields
// no files and no error, so a caller need not special-case a session that has
// no journal.
func (w *Writer) Snapshot(stageDir string) ([]SnapshotFile, error) {
	if w == nil {
		return nil, nil
	}
	if stageDir == "" {
		return nil, fmt.Errorf("journal snapshot: empty staging dir")
	}

	// Held across the whole capture, including the copy of the active
	// segment, because that is exactly the window rotation must not run in.
	// The active segment is bounded by WriterOptions.RotateBytes (4 MiB by
	// default), so the stall is a few milliseconds of local copy, not a
	// network round trip.
	w.mu.Lock()
	defer w.mu.Unlock()

	// Push anything sitting in the bufio writer to the file first: the copy
	// below reads the file, not the buffer.
	if w.activeBuf != nil {
		if err := w.activeBuf.Flush(); err != nil {
			return nil, fmt.Errorf("journal snapshot: flushing active segment: %w", err)
		}
	}
	if w.activeFile != nil {
		if err := w.activeFile.Sync(); err != nil {
			return nil, fmt.Errorf("journal snapshot: syncing active segment: %w", err)
		}
	}

	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, fmt.Errorf("journal snapshot: listing %s: %w", w.dir, err)
	}

	var out []SnapshotFile
	for _, e := range entries {
		if e.IsDir() || e.Name() == activeSegmentName {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("journal snapshot: stat %s: %w", e.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		out = append(out, SnapshotFile{
			Name: e.Name(),
			Path: filepath.Join(w.dir, e.Name()),
			Size: info.Size(),
		})
	}

	staged, size, err := stageActiveSegment(w.dir, stageDir)
	if err != nil {
		return nil, err
	}
	if staged != "" {
		out = append(out, SnapshotFile{
			Name:   activeSegmentName,
			Path:   staged,
			Staged: true,
			Size:   size,
		})
	}
	return out, nil
}

// stageActiveSegment copies events.jsonl into stageDir. Returns an empty path
// when the active segment does not exist, which is the normal state of a
// journal directory that has been closed and swept.
func stageActiveSegment(journalDir, stageDir string) (string, int64, error) {
	src := filepath.Join(journalDir, activeSegmentName)
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", 0, nil
		}
		return "", 0, fmt.Errorf("journal snapshot: opening %s: %w", src, err)
	}
	defer in.Close()

	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return "", 0, fmt.Errorf("journal snapshot: creating staging dir %s: %w", stageDir, err)
	}
	dst := filepath.Join(stageDir, activeSegmentName)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", 0, fmt.Errorf("journal snapshot: creating %s: %w", dst, err)
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return "", 0, fmt.Errorf("journal snapshot: copying active segment: %w", copyErr)
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("journal snapshot: closing %s: %w", dst, closeErr)
	}
	return dst, n, nil
}
