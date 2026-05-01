package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/klauspost/compress/zstd"
)

// rotatedRe matches rotated segment names: events-NNN.jsonl.zst (3+ digits).
var rotatedRe = regexp.MustCompile(`^events-(\d{3,})\.jsonl\.zst$`)

// rotateActiveSegment is the default rotateCb. It compresses the current
// events.jsonl into the next events-NNN.jsonl.zst slot, truncates the active
// segment to zero, and reopens the buffered writer. Caller must hold w.mu.
func rotateActiveSegment(w *Writer) error {
	if w.activeBuf != nil {
		if err := w.activeBuf.Flush(); err != nil {
			return fmt.Errorf("flush before rotate: %w", err)
		}
	}
	if w.activeFile != nil {
		if err := w.activeFile.Sync(); err != nil {
			return fmt.Errorf("sync before rotate: %w", err)
		}
	}

	idx, err := nextSegmentIndex(w.dir)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("events-%03d.jsonl.zst", idx)
	rotatedPath := filepath.Join(w.dir, name)

	src := filepath.Join(w.dir, activeSegmentName)
	if err := compressFile(src, rotatedPath); err != nil {
		return fmt.Errorf("compress rotated segment: %w", err)
	}

	// Truncate the active segment in place; reopening would race readers.
	if err := w.activeFile.Truncate(0); err != nil {
		return fmt.Errorf("truncate active segment: %w", err)
	}
	if _, err := w.activeFile.Seek(0, 0); err != nil {
		return fmt.Errorf("seek active segment: %w", err)
	}
	w.activeBytes = 0
	// Reset the bufio writer; the underlying *os.File is reused.
	w.activeBuf.Reset(w.activeFile)
	return nil
}

func nextSegmentIndex(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("listing journal dir: %w", err)
	}
	max := 0
	for _, e := range entries {
		m := rotatedRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, perr := strconv.Atoi(m[1])
		if perr != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max + 1, nil
}

func compressFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	enc, err := zstd.NewWriter(out, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return err
	}
	if _, err := enc.ReadFrom(in); err != nil {
		_ = enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return out.Sync()
}
