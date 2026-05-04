package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// archiveDirName is the per-session subdirectory that holds rewind
// snapshots. One subdirectory per rewind, named with an RFC3339 timestamp.
const archiveDirName = "archive"

// RewindResult describes the outcome of a successful Rewind call.
type RewindResult struct {
	// ArchiveName is the directory name (not full path) of the archived
	// pre-rewind journal under <journalDir>/archive/. Pass it back to
	// Restore to swap it in.
	ArchiveName string
	// TruncatedSeq is the highest seq that survived the rewind. Equals
	// the requested ToSeq when the journal contained an envelope with
	// that seq; otherwise it is the largest seq <= ToSeq that was found.
	TruncatedSeq uint64
	// EventsKept is how many envelopes ended up in the new live journal.
	EventsKept int
	// EventsArchived is how many envelopes were in the pre-rewind
	// journal (kept + dropped).
	EventsArchived int
}

// Rewind rewrites journalDir so its live journal ends at the envelope
// with seq == toSeq (inclusive). The original journal is preserved
// verbatim under <journalDir>/archive/<timestamp>/, where it can be
// inspected or swapped back in via Restore.
//
// The session must not have an active Writer attached when Rewind runs;
// concurrent writes against a journal being rewound produce undefined
// state. Engine callers tear down the Writer before invoking.
//
// Rewind is idempotent in the sense that calling it twice with the same
// toSeq leaves the live journal in the same shape; each call simply
// produces another archive snapshot.
func Rewind(journalDir string, toSeq uint64) (RewindResult, error) {
	res := RewindResult{}
	if journalDir == "" {
		return res, fmt.Errorf("rewind: empty journal dir")
	}
	if toSeq == 0 {
		return res, fmt.Errorf("rewind: toSeq must be >= 1")
	}

	// Read existing envelopes up to toSeq before we touch anything on disk.
	// If the read fails halfway through we abort without disturbing state.
	r, err := Open(journalDir)
	if err != nil {
		return res, fmt.Errorf("rewind: open journal: %w", err)
	}
	header := r.Header()

	var kept []Envelope
	var totalSeen int
	var maxSurviving uint64
	iterErr := r.Iter(func(env Envelope) bool {
		totalSeen++
		if env.Seq <= toSeq {
			kept = append(kept, env)
			if env.Seq > maxSurviving {
				maxSurviving = env.Seq
			}
		}
		return true
	})
	if iterErr != nil {
		return res, fmt.Errorf("rewind: scan journal: %w", iterErr)
	}
	if len(kept) == 0 {
		return res, fmt.Errorf("rewind: no envelopes with seq <= %d", toSeq)
	}

	// Sort by seq just in case rotated segments arrived out-of-order on
	// disk — Iter walks files in index order but defends here.
	sort.Slice(kept, func(i, j int) bool { return kept[i].Seq < kept[j].Seq })

	archiveRoot := filepath.Join(journalDir, archiveDirName)
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		return res, fmt.Errorf("rewind: create archive root: %w", err)
	}

	archiveName := time.Now().UTC().Format("20060102T150405Z")
	archivePath := filepath.Join(archiveRoot, archiveName)
	// Avoid clobbering an archive from the same second.
	for i := 1; ; i++ {
		if _, err := os.Stat(archivePath); os.IsNotExist(err) {
			break
		}
		archiveName = fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405Z"), i)
		archivePath = filepath.Join(archiveRoot, archiveName)
	}

	// Move every existing journal entry (header + active + rotated) into
	// the archive subdirectory. The archive subdir is preserved.
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		return res, fmt.Errorf("rewind: list journal dir: %w", err)
	}
	if err := os.MkdirAll(archivePath, 0o755); err != nil {
		return res, fmt.Errorf("rewind: create archive dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == archiveDirName {
			continue
		}
		src := filepath.Join(journalDir, name)
		dst := filepath.Join(archivePath, name)
		if err := os.Rename(src, dst); err != nil {
			return res, fmt.Errorf("rewind: archive %s: %w", name, err)
		}
	}

	// Re-create header.json + a fresh events.jsonl from kept envelopes.
	if err := writeHeader(journalDir, header); err != nil {
		return res, fmt.Errorf("rewind: write header: %w", err)
	}
	if err := writeEnvelopes(filepath.Join(journalDir, activeSegmentName), kept); err != nil {
		return res, fmt.Errorf("rewind: write truncated journal: %w", err)
	}

	res.ArchiveName = archiveName
	res.TruncatedSeq = maxSurviving
	res.EventsKept = len(kept)
	res.EventsArchived = totalSeen
	return res, nil
}

// Restore swaps the live journal for a previously archived snapshot.
// The current live journal is itself archived first (under a fresh
// timestamp) so the operation is reversible.
//
// archiveName is the directory name (not full path) reported by Rewind
// or visible under <journalDir>/archive/.
func Restore(journalDir, archiveName string) error {
	if journalDir == "" {
		return fmt.Errorf("restore: empty journal dir")
	}
	if archiveName == "" || strings.ContainsAny(archiveName, "/\\") {
		return fmt.Errorf("restore: invalid archive name %q", archiveName)
	}
	src := filepath.Join(journalDir, archiveDirName, archiveName)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("restore: archive not found: %w", err)
	}

	// Archive the current live journal so the caller can flip back if
	// the restore points the wrong direction.
	rotateName := "pre-restore-" + time.Now().UTC().Format("20060102T150405Z")
	rotatePath := filepath.Join(journalDir, archiveDirName, rotateName)
	if err := os.MkdirAll(rotatePath, 0o755); err != nil {
		return fmt.Errorf("restore: create rotate dir: %w", err)
	}

	entries, err := os.ReadDir(journalDir)
	if err != nil {
		return fmt.Errorf("restore: list journal dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == archiveDirName {
			continue
		}
		if err := os.Rename(filepath.Join(journalDir, name), filepath.Join(rotatePath, name)); err != nil {
			return fmt.Errorf("restore: rotate %s: %w", name, err)
		}
	}

	// Copy the chosen archive's contents into the live journal dir.
	srcEntries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("restore: list archive: %w", err)
	}
	for _, e := range srcEntries {
		name := e.Name()
		from := filepath.Join(src, name)
		to := filepath.Join(journalDir, name)
		if err := copyPath(from, to); err != nil {
			return fmt.Errorf("restore: copy %s: %w", name, err)
		}
	}
	return nil
}

// ListArchives returns the archive directory names under journalDir,
// sorted ascending by directory name (timestamp). An empty slice is
// returned when no archive directory has ever been created.
func ListArchives(journalDir string) ([]string, error) {
	if journalDir == "" {
		return nil, fmt.Errorf("list archives: empty journal dir")
	}
	root := filepath.Join(journalDir, archiveDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// writeHeader writes header.json from the supplied Header, overwriting
// any existing file.
func writeHeader(journalDir string, h Header) error {
	if h.SchemaVersion == "" {
		h.SchemaVersion = SchemaVersion
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(journalDir, headerName), data, 0o644)
}

// writeEnvelopes serializes envs to path, one JSONL line per envelope.
// Truncates any existing file. fsyncs before returning so the rewind is
// durable when the function returns.
func writeEnvelopes(path string, envs []Envelope) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	for i := range envs {
		if err := enc.Encode(envs[i]); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

// copyPath copies a regular file or recursively copies a directory tree
// from src to dst. The destination is created if absent and overwritten
// if it exists. Used by Restore to materialize archived contents back
// into the live journal directory.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := bufio.NewReader(in).WriteTo(out); err != nil {
		return err
	}
	return out.Sync()
}
