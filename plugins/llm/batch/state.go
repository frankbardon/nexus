package batch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// batchState is the on-disk record for one in-flight batch. The coordinator
// writes one JSON file per batch into the configured data dir at submit time
// and deletes it once results are emitted. On boot the coordinator scans the
// data dir, loads every state file, and resumes a poller for each — that's
// what makes batches survive process restarts.
type batchState struct {
	Provider     string                `json:"provider"`
	BatchID      string                `json:"batch_id"`
	SubmittedAt  time.Time             `json:"submitted_at"`
	OriginalReqs []events.BatchRequest `json:"original_reqs"`
	Metadata     map[string]any        `json:"metadata,omitempty"`
}

// stateFilename returns the basename used for a given batch id. We sanitize
// the id to a filesystem-safe form so providers' own id schemes (which may
// contain ":" or "/" — e.g. Anthropic's `msgbatch_…`, OpenAI's `batch_…`)
// don't escape the data dir or create unintended subdirectories.
func stateFilename(batchID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, batchID)
	return safe + ".json"
}

// saveBatch writes (or overwrites) the state file for one batch. The caller
// is responsible for ensuring dir exists; we don't MkdirAll here so accidental
// typos in config don't silently create stray directories.
func saveBatch(dir string, b *batchState) error {
	if dir == "" {
		return fmt.Errorf("batch: empty state dir")
	}
	if b == nil || b.BatchID == "" {
		return fmt.Errorf("batch: cannot save state with empty batch id")
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("batch: marshal state: %w", err)
	}
	path := filepath.Join(dir, stateFilename(b.BatchID))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("batch: write tmp state file %q: %w", tmp, err)
	}
	// Atomic-ish swap so a crash mid-write doesn't leave a half-baked file
	// that loadBatches will choke on later.
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("batch: rename state file %q: %w", path, err)
	}
	return nil
}

// loadBatches scans dir for *.json files and decodes each into a batchState.
// Files that fail to decode are skipped (with the error returned aggregated)
// — better to recover the surviving batches than abort entirely.
func loadBatches(dir string) ([]*batchState, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("batch: read state dir %q: %w", dir, err)
	}

	var states []*batchState
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("batch: read state %q: %w", path, err)
			}
			continue
		}
		var s batchState
		if err := json.Unmarshal(data, &s); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("batch: decode state %q: %w", path, err)
			}
			continue
		}
		if s.BatchID == "" {
			continue
		}
		states = append(states, &s)
	}
	return states, firstErr
}

// deleteBatch removes the on-disk state file for a batch. Missing-file errors
// are treated as success — the caller may have cleaned up already.
func deleteBatch(dir, batchID string) error {
	if dir == "" || batchID == "" {
		return nil
	}
	path := filepath.Join(dir, stateFilename(batchID))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("batch: delete state %q: %w", path, err)
	}
	return nil
}
