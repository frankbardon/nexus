package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// This file is what makes a per-plugin store.db safe to copy off the machine.
//
// A live handle is a WAL database: committed transactions live in store.db-wal
// until a checkpoint folds them back into store.db, and store.db-shm is a
// process-local index into that WAL. Neither sidecar is meaningful anywhere but
// the machine that produced it, so a remote copy has to be a *single* file that
// stands on its own — which the main database file alone is not, at any moment
// a writer has been active since the last checkpoint.
//
// Two things are therefore done, in this order, and they do different jobs:
//
//  1. PRAGMA wal_checkpoint(TRUNCATE) folds every committed frame back into
//     store.db and resets the WAL to zero length. This is what makes the
//     *live* file self-contained: anything that copies store.db byte-for-byte
//     without it — this process, an operator with cp, a sidecar agent — walks
//     away with a database missing every transaction still in the WAL, and the
//     result is a plausible, openable, silently stale database rather than an
//     error. It also bounds a -wal that a long session would otherwise let grow
//     without limit.
//
//  2. VACUUM INTO writes a fresh, self-consistent database to a new path from
//     inside a read transaction. This is what makes the *snapshot* untearable.
//     Copying store.db with io.Copy after the checkpoint was considered and
//     rejected: a plugin committing between the checkpoint and the last read
//     byte tears the copy, and because the WAL was just truncated the torn
//     result is a corrupt file, not a stale one. A corrupt file that uploads
//     successfully is strictly worse than a failed upload, since it replaces a
//     good remote copy. VACUUM INTO also compacts, so the uploaded object is
//     never larger than the live file.
//
// The driver is modernc.org/sqlite — pure Go, so there is no CGO backup API to
// lean on. VACUUM INTO is the portable equivalent and needs nothing but SQL.

// CheckpointResult reports what PRAGMA wal_checkpoint(TRUNCATE) did.
//
// SQLite returns three integers. Busy is the one that matters: a busy
// checkpoint could not reclaim the whole WAL because a reader was still holding
// frames, which means the main database file is *not* fully self-contained.
// The VACUUM INTO that follows still produces a correct snapshot in that case,
// so a busy checkpoint is reported and logged rather than treated as an error.
type CheckpointResult struct {
	// Busy is true when SQLite could not complete the checkpoint because
	// another connection held a read lock.
	Busy bool
	// WALFrames is the number of frames in the WAL, as reported by SQLite.
	WALFrames int
	// Checkpointed is the number of frames successfully moved back into the
	// main database file.
	Checkpointed int
}

// Snapshot describes one self-consistent, sidecar-free copy of a plugin's
// store.db.
type Snapshot struct {
	// PluginID owns the database.
	PluginID string
	// Scope is the resolved scope, after ScopeAgent collapses to ScopeApp on
	// an engine with no agent ID.
	Scope Scope
	// LivePath is the store.db the snapshot was taken from. Callers map it
	// back onto a tree-relative path or object key.
	LivePath string
	// Path is the snapshot file. Restorable on its own: no -wal, no -shm.
	Path string
	// Bytes is the snapshot's size on disk.
	Bytes int64
	// Checkpoint records the WAL checkpoint that preceded the snapshot.
	Checkpoint CheckpointResult
	// Duration is the wall time the checkpoint and the copy took together.
	// Surfaced because this cost is O(database size) and is paid on every
	// snapshot — the number an operator needs when a database grows.
	Duration time.Duration
}

// Snapshot writes a self-consistent copy of every open handle at scope into
// destDir, one per plugin, at <destDir>/<pluginID>/store.db.
//
// Only handles this Manager has actually opened are covered. That is the right
// set by construction: a store.db in the tree with no handle has no writer in
// this process, so it is already static and a caller can copy it directly.
//
// A partial failure returns the snapshots taken so far alongside the error, so
// a caller can clean them up. It deliberately does not press on past a failure:
// a snapshot missing one plugin's database is not a snapshot.
func (m *Manager) Snapshot(scope Scope, destDir string) ([]Snapshot, error) {
	if destDir == "" {
		return nil, fmt.Errorf("storage: snapshot needs a destination directory")
	}
	// Mirror Open's collapse so Snapshot(ScopeAgent) on an engine with no
	// agent ID finds the handles Open filed under ScopeApp, rather than
	// silently returning nothing.
	if scope == ScopeAgent && m.agentID == "" {
		scope = ScopeApp
	}

	// Take the handle list under the lock and do the I/O outside it. A
	// snapshot is O(database size); holding m.mu across it would block every
	// plugin calling Storage() for the duration. Handles are only removed by
	// Manager.Close, which the engine calls from the same goroutine that
	// drives snapshots, so a handle captured here cannot close underneath us.
	type target struct {
		pluginID string
		store    *sqliteStore
	}
	m.mu.Lock()
	targets := make([]target, 0, len(m.pool))
	for k, st := range m.pool {
		if k.scope != scope {
			continue
		}
		targets = append(targets, target{pluginID: k.pluginID, store: st})
	}
	m.mu.Unlock()

	out := make([]Snapshot, 0, len(targets))
	for _, t := range targets {
		dir := filepath.Join(destDir, t.pluginID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return out, fmt.Errorf("storage: snapshot mkdir %q: %w", dir, err)
		}
		dest := filepath.Join(dir, "store.db")

		started := time.Now()
		ck, err := t.store.snapshotTo(dest)
		if err != nil {
			return out, fmt.Errorf("storage: snapshot %s (%s): %w", t.pluginID, scope, err)
		}
		info, err := os.Stat(dest)
		if err != nil {
			return out, fmt.Errorf("storage: snapshot %s (%s): stat %q: %w", t.pluginID, scope, dest, err)
		}
		out = append(out, Snapshot{
			PluginID:   t.pluginID,
			Scope:      scope,
			LivePath:   t.store.path,
			Path:       dest,
			Bytes:      info.Size(),
			Checkpoint: ck,
			Duration:   time.Since(started),
		})
	}
	return out, nil
}

// Checkpoint folds the WAL of every open handle at scope back into its main
// database file, without producing a copy.
//
// Exported separately from Snapshot because the checkpoint is useful on its
// own: it bounds the size of a store.db-wal that a long-lived session would
// otherwise let grow unchecked, and it leaves the *local* database file
// restorable in place. Snapshot always runs it first, so callers taking a
// snapshot do not need this as well.
func (m *Manager) Checkpoint(scope Scope) (map[string]CheckpointResult, error) {
	if scope == ScopeAgent && m.agentID == "" {
		scope = ScopeApp
	}
	type target struct {
		pluginID string
		store    *sqliteStore
	}
	m.mu.Lock()
	targets := make([]target, 0, len(m.pool))
	for k, st := range m.pool {
		if k.scope == scope {
			targets = append(targets, target{pluginID: k.pluginID, store: st})
		}
	}
	m.mu.Unlock()

	out := make(map[string]CheckpointResult, len(targets))
	for _, t := range targets {
		ck, err := t.store.checkpoint()
		if err != nil {
			return out, fmt.Errorf("storage: checkpoint %s (%s): %w", t.pluginID, scope, err)
		}
		out[t.pluginID] = ck
	}
	return out, nil
}

// checkpoint runs PRAGMA wal_checkpoint(TRUNCATE) on the handle.
//
// TRUNCATE rather than PASSIVE or FULL: PASSIVE gives up the moment a reader is
// present and would leave frames behind with no way for the caller to tell, and
// FULL folds the WAL back but leaves the file at its high-water mark, so a
// session that once wrote a large batch keeps paying for it forever. TRUNCATE
// does both jobs and is the only mode that leaves store.db genuinely alone on
// disk. It blocks on readers, which the 5s busy timeout bounds.
//
// The pragma returns a row, so this is a query and not an Exec: discarding the
// result would throw away the one signal that says the checkpoint did not
// actually finish.
func (s *sqliteStore) checkpoint() (CheckpointResult, error) {
	var busy, walFrames, checkpointed int
	if err := s.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &walFrames, &checkpointed); err != nil {
		return CheckpointResult{}, fmt.Errorf("wal_checkpoint(TRUNCATE) on %q: %w", s.path, err)
	}
	return CheckpointResult{
		Busy:         busy != 0,
		WALFrames:    walFrames,
		Checkpointed: checkpointed,
	}, nil
}

// snapshotTo checkpoints the WAL and then writes a fresh database to dest.
//
// dest must not already exist — VACUUM INTO refuses to overwrite, which is a
// feature here: it makes an accidental snapshot-over-a-live-database
// impossible.
func (s *sqliteStore) snapshotTo(dest string) (CheckpointResult, error) {
	ck, err := s.checkpoint()
	if err != nil {
		return ck, err
	}
	// Bound as a parameter rather than interpolated into the statement: a
	// destination path is caller-supplied, and a path containing a quote would
	// otherwise be a SQL injection into a DDL statement that runs as this
	// process.
	if _, err := s.db.Exec(`VACUUM INTO ?`, dest); err != nil {
		return ck, fmt.Errorf("VACUUM INTO %q from %q: %w", dest, s.path, err)
	}
	return ck, nil
}
