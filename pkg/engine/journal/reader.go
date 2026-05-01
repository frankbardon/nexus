package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/klauspost/compress/zstd"
)

// Reader opens a journal directory and streams envelopes in seq order. It is
// read-only; concurrent open with an active Writer on the same dir is fine
// for the rotated segments but the active segment's tail may be incomplete.
type Reader struct {
	dir      string
	header   Header
	rotated  []string // absolute paths, sorted by index ascending
	hasActiv bool
}

// Open validates the header and indexes the segments. Iter does the actual
// streaming; this constructor is cheap.
func Open(dir string) (*Reader, error) {
	headerPath := filepath.Join(dir, headerName)
	data, err := os.ReadFile(headerPath)
	if err != nil {
		return nil, fmt.Errorf("reading journal header: %w", err)
	}
	var h Header
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("parsing journal header: %w", err)
	}
	if h.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("journal schema mismatch: header=%q reader=%q", h.SchemaVersion, SchemaVersion)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("listing journal dir: %w", err)
	}

	type rotated struct {
		idx  int
		path string
	}
	var rots []rotated
	hasActive := false

	for _, e := range entries {
		name := e.Name()
		if name == activeSegmentName {
			hasActive = true
			continue
		}
		m := rotatedRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		idx, perr := strconv.Atoi(m[1])
		if perr != nil {
			continue
		}
		rots = append(rots, rotated{idx: idx, path: filepath.Join(dir, name)})
	}
	sort.Slice(rots, func(i, j int) bool { return rots[i].idx < rots[j].idx })

	r := &Reader{
		dir:      dir,
		header:   h,
		hasActiv: hasActive,
	}
	for _, rot := range rots {
		r.rotated = append(r.rotated, rot.path)
	}
	return r, nil
}

// Header returns the parsed manifest. Useful for diagnostics.
func (r *Reader) Header() Header { return r.header }

// Iter streams every envelope in seq order. The yield func returns false to
// stop iteration; the underlying readers are closed before Iter returns.
//
// Malformed JSONL lines are skipped; partial trailing lines (no newline) are
// also skipped — the writer crashed mid-write and the next Append will
// either complete or overwrite that line on resume. Behavior matches Go's
// bufio.Scanner default.
func (r *Reader) Iter(yield func(Envelope) bool) error {
	for _, path := range r.rotated {
		stop, err := r.iterRotated(path, yield)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	if r.hasActiv {
		stop, err := r.iterActive(filepath.Join(r.dir, activeSegmentName), yield)
		if err != nil {
			return err
		}
		_ = stop
	}
	return nil
}

func (r *Reader) iterRotated(path string, yield func(Envelope) bool) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	dec, err := zstd.NewReader(f)
	if err != nil {
		return false, fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()

	return iterReader(dec, yield)
}

func (r *Reader) iterActive(path string, yield func(Envelope) bool) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	return iterReader(f, yield)
}

func iterReader(rd io.Reader, yield func(Envelope) bool) (bool, error) {
	br := bufio.NewReaderSize(rd, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			// Trim trailing newline; ReadBytes includes the delimiter on
			// success, but the final line may lack one if the writer was
			// killed mid-flush. Skip incomplete tails (err == io.EOF and
			// no newline).
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			} else if err == io.EOF {
				break
			}
			var env Envelope
			if jerr := json.Unmarshal(line, &env); jerr != nil {
				// Skip malformed lines rather than abort: the writer's
				// fallback path can leave a degraded but parseable
				// envelope, and skipping a single bad line is preferable
				// to refusing to replay the entire session.
				continue
			}
			if !yield(env) {
				return true, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return false, fmt.Errorf("read line: %w", err)
		}
	}
	return false, nil
}

// LastSeq returns the highest seq seen across all segments. Returns 0 if the
// journal is empty.
func (r *Reader) LastSeq() (uint64, error) {
	var last uint64
	err := r.Iter(func(e Envelope) bool {
		if e.Seq > last {
			last = e.Seq
		}
		return true
	})
	return last, err
}

// LastTurnBoundary returns the seq of the most recent agent.turn.end envelope
// and ok=true if found. Used by the Phase 2 coordinator to detect a partial
// turn at boot — the unfinished turn lives between this seq and LastSeq.
func (r *Reader) LastTurnBoundary() (uint64, bool, error) {
	var last uint64
	var found bool
	err := r.Iter(func(e Envelope) bool {
		if e.Type == "agent.turn.end" {
			last = e.Seq
			found = true
		}
		return true
	})
	return last, found, err
}
