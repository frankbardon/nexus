package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// sessionBinaryIndexName is the file inside state_dir that holds the durable
// session → binary index.
//
// It is a SEPARATE file from leases.jsonl on purpose, and that separation is the
// entire reason this type exists. The lease journal is lease-lifecycle state: it
// is compacted down to the leases that are still live, so a released lease's
// record — and with it the binary that lease was running — is deliberately
// dropped. A resume happens strictly AFTER the original lease was released, so a
// binding that lived only in the lease journal would always be gone by the time
// anything wanted to read it. Storing the pairing under the lease journal's
// retention rules would therefore mean storing it nowhere useful.
const sessionBinaryIndexName = "session-binaries.jsonl"

// maxSessionBindings is the most session → binary pairs the index keeps. Once a
// rewrite finds more than this, the OLDEST bindings (by last-recorded time) are
// dropped until the count fits.
//
// An entry cap is required here, and rewrite-alone — the lease journal's answer —
// is not sufficient. The lease journal shrinks on rewrite because leases are
// released and released leases are dropped; this index has no such event. A
// binding's whole purpose is to outlive the lease that produced it, so nothing
// ever retires one, and deduplicating by session id bounds only the number of
// records per session, not the number of distinct sessions a broker meets over
// its lifetime. Without an explicit cap the file would grow forever, one line per
// session ever spawned.
//
// 4096 is chosen as "far more sessions than a single broker realistically has in
// flight as resumable, while still a trivially small file": at roughly 120 bytes
// per record a full index is under 500 KiB, cheap to read at every boot and to
// hold in memory.
//
// Eviction is by recency and it is EXPLICITLY LOSSY. The index is best-effort by
// design: if pruning drops a very old session's binding, BinaryForSession reports
// unknown for it and the caller falls through to "no opinion, proceed" — exactly
// the behaviour that session had before this index existed. Nothing here promises
// a binding is kept forever, and no caller may rely on one being found.
const maxSessionBindings = 4096

// sessionBinaryCompactEvery is how many appends may accumulate before the index
// is rewritten in place, deduplicated and pruned to maxSessionBindings.
//
// It is a counter rather than a byte size or a timer for the same reason as the
// lease journal's compactEvery: the file is only ever appended to by this
// process, so counting our own writes needs no stat() and no background goroutine
// to own, start or stop. Together with the rewrite-on-open pass it bounds the file
// at (maxSessionBindings + sessionBinaryCompactEvery) records at any instant.
//
// It is lower than the lease journal's 512 because an append here is rarer — one
// per session whose binary changes or is seen for the first time, not one per
// lease transition — so a higher threshold would mean many brokers never
// compacting at all.
const sessionBinaryCompactEvery = 256

// sessionBinaryRecord is one line of the session → binary index.
//
// NO SECRET MAY EVER BE ADDED TO THIS STRUCT, for the same reason as LeaseRecord:
// this file outlives the process and is written to be read back.
type sessionBinaryRecord struct {
	// SessionID is the engine session id the instance reported (or that a resume
	// claim named). It is the fold key: a later record for the same id supersedes
	// an earlier one.
	SessionID string `json:"session_id"`

	// Binary is the `binaries:` registry entry NAME the session was spawned from —
	// the name, not the path, so the binding still identifies the variant after an
	// operator repoints that entry at a new build.
	//
	// Empty is never written and never loaded: "" means "not recorded" everywhere
	// in the broker, so a record carrying it would be a binding that says nothing
	// while occupying one of maxSessionBindings slots.
	Binary string `json:"binary"`

	// At is when the pairing was last recorded. It is what recency-based pruning
	// orders by, and it is the only reason this record has a timestamp at all —
	// nothing reads it for display.
	At time.Time `json:"at"`
}

// sessionBinaryIndex is the durable session → binary map: an append-only JSONL
// file under state_dir with an in-memory fold, deliberately independent of the
// lease journal and unaffected by its compaction.
//
// A NIL *sessionBinaryIndex IS A FULLY SUPPORTED VALUE and is what an unset
// state_dir produces. Every method is nil-receiver safe, so no caller needs a
// branch. It is a concrete pointer rather than an interface specifically to avoid
// the typed-nil trap that a `var x SomeInterface = (*impl)(nil)` assignment
// creates — there is one implementation and no second backend in sight, so the
// seam LeaseStore has would buy nothing here.
type sessionBinaryIndex struct {
	logger *slog.Logger

	// path is the index file inside the per-broker state_dir. Two brokers never
	// name the same file because state_dir is per-broker.
	path string

	// now is the clock stamped on records and used to order pruning. Tests swap it
	// for a deterministic one so recency eviction can be exercised without sleeps.
	now func() time.Time

	mu sync.Mutex

	// f is the append handle. Nil after Close, and briefly during a rewrite;
	// record() reports the problem by logging rather than panicking in either case.
	f *os.File

	// bindings is the folded state: session id → most recent record.
	bindings map[string]sessionBinaryRecord

	// sinceCompact counts appends since the last rewrite. See
	// sessionBinaryCompactEvery.
	sinceCompact int
}

// openSessionBinaryIndex builds the broker's session → binary index from config.
//
// An EMPTY state_dir disables it entirely: it returns a nil index, nothing is
// written, no file is created, and BinaryForSession falls back to live leases
// alone — precisely the behaviour the broker had before this index existed.
//
// Unlike openLeaseStore, an unusable index is NOT a boot failure, and the
// asymmetry is deliberate. The lease journal is how a restart accounts for
// processes it left running; losing it silently loses instances. This index backs
// an ADVISORY check whose documented answer for an unknown session is "no
// opinion, proceed" — so a broker that cannot open it is a broker that behaves as
// it did one story ago, which is a far better outcome than refusing to serve. The
// caller logs and carries on with a nil index.
func openSessionBinaryIndex(logger *slog.Logger, cfg Config) (*sessionBinaryIndex, error) {
	if cfg.StateDir == "" {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	// 0o700 to match the lease journal: this file names engine sessions. It is not
	// a secret store, but it is nobody else's business either.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating broker state_dir %q: %w", cfg.StateDir, err)
	}

	idx := &sessionBinaryIndex{
		logger:   logger,
		path:     filepath.Join(cfg.StateDir, sessionBinaryIndexName),
		now:      time.Now,
		bindings: make(map[string]sessionBinaryRecord),
	}

	recs, skipped, err := readSessionBinaryIndex(idx.path)
	if err != nil {
		return nil, fmt.Errorf("reading session binary index: %w", err)
	}
	if skipped > 0 {
		idx.logger.Warn("skipped unreadable session binary index records; the broker was most likely killed mid-write",
			"path", idx.path, "skipped", skipped, "read", len(recs))
	}
	for _, rec := range recs {
		idx.bindings[rec.SessionID] = rec
	}

	// Rewrite-on-open. It deduplicates, applies the entry cap, and truncates any
	// torn trailing record, so the file a running broker appends to always starts
	// from a clean, fully parseable, already-bounded state.
	if err := idx.rewriteLocked(); err != nil {
		return nil, err
	}
	idx.logger.Info("session binary index opened",
		"path", idx.path, "bindings", len(idx.bindings), "skipped_records", skipped)
	return idx, nil
}

// record persists the pairing of an engine session with the `binaries:` entry it
// is running.
//
// It has NO ERROR RETURN by construction, mirroring Registry.appendRecord: a
// write failure is logged and otherwise ignored, because a best-effort advisory
// index must never become a new way for a claim or a session report to fail.
//
// Empty inputs are dropped rather than stored. An empty session id would collide
// with every not-yet-reported lease, and an empty binary is the broker's spelling
// of "not recorded" — writing either would put a binding that answers nothing into
// a capped store.
//
// THIS INDEX IS DELIBERATELY NOT FSYNCED, unlike the lease journal, and the
// asymmetry is the point rather than an omission. Losing the tail of
// leases.jsonl to a host crash loses the pid of a running instance, and a lease
// with no pid is closed out at the next boot while its process keeps running —
// an orphan, the one failure recovery cannot repair. Losing the tail of THIS
// file loses a binding, and a missing binding is the documented "unknown session
// means no opinion, proceed" answer that every caller already handles: the
// resume goes ahead exactly as it did before this index existed. A barrier
// cannot buy correctness for a store whose worst case is already correct, so it
// would be pure cost. Do not add one without first changing what a missing
// binding means.
//
// A pairing that is already recorded with the same binary refreshes its recency
// in memory but does NOT append: re-reporting the same session on every resume
// would otherwise write a line per resume for a value that has not changed. The
// refreshed timestamp reaches disk at the next rewrite, which is the only place
// recency is consulted anyway.
//
// A nil index does nothing at all.
func (s *sessionBinaryIndex) record(sessionID, binary string) {
	if s == nil || sessionID == "" || binary == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec := sessionBinaryRecord{SessionID: sessionID, Binary: binary, At: s.now()}
	if prev, ok := s.bindings[sessionID]; ok && prev.Binary == binary {
		s.bindings[sessionID] = rec
		return
	}
	s.bindings[sessionID] = rec

	if s.f == nil {
		s.logger.Error("session binary index is closed; the binding was not persisted",
			"path", s.path, "session_id", sessionID, "binary", binary)
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		s.logger.Error("encoding session binary record failed; the binding was not persisted",
			"path", s.path, "session_id", sessionID, "binary", binary, "error", err)
		return
	}
	line = append(line, '\n')
	// One Write per record, straight to the file descriptor with no buffering in
	// front of it, so a record is either in the file or it is not — the only partial
	// record a reader can meet is the one the kernel was mid-way through, which
	// readSessionBinaryIndex skips.
	if _, err := s.f.Write(line); err != nil {
		s.logger.Error("appending session binary record failed; resume will not know this session's binary after a restart",
			"path", s.path, "session_id", sessionID, "binary", binary, "error", err)
		return
	}

	s.sinceCompact++
	if s.sinceCompact >= sessionBinaryCompactEvery {
		if err := s.rewriteLocked(); err != nil {
			// The record IS written; a failed rewrite only means the file stays larger
			// than we would like. Logging it as a rewrite problem rather than a write
			// one keeps a housekeeping failure from reading as a durability failure.
			s.logger.Error("compacting the session binary index failed; the index will keep growing",
				"path", s.path, "error", err)
		}
	}
}

// lookup returns the binary recorded for a session and whether one is known.
//
// (name, true) means a binding was found; ("", false) means the index has no
// opinion — an unknown session, an empty id, or no index at all. A nil index
// always reports unknown, which is what makes a broker with no state_dir behave
// exactly as it did before this file existed.
func (s *sessionBinaryIndex) lookup(sessionID string) (string, bool) {
	if s == nil || sessionID == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.bindings[sessionID]
	if !ok || rec.Binary == "" {
		return "", false
	}
	return rec.Binary, true
}

// Close closes the append handle. It is idempotent and nil-receiver safe.
func (s *sessionBinaryIndex) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	if err != nil {
		return fmt.Errorf("closing session binary index: %w", err)
	}
	return nil
}

// prunedLocked returns the folded bindings in recording order, oldest first,
// after dropping everything past maxSessionBindings. Caller MUST hold s.mu.
//
// Ordering is oldest-first so the written file reads chronologically, while the
// PRUNE takes from the front — the oldest bindings are the ones least likely to
// be resumed, and dropping them degrades to "unknown", never to a wrong answer.
func (s *sessionBinaryIndex) prunedLocked() []sessionBinaryRecord {
	out := make([]sessionBinaryRecord, 0, len(s.bindings))
	for _, rec := range s.bindings {
		out = append(out, rec)
	}
	// Session id as tiebreak so the output (and therefore the rewritten file) is
	// byte-stable for equal timestamps — tests use an injected clock that hands out
	// identical instants.
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].At.Before(out[j].At)
	})
	if len(out) > maxSessionBindings {
		out = out[len(out)-maxSessionBindings:]
	}
	return out
}

// rewriteLocked replaces the index with exactly the surviving bindings, one
// record each, and resyncs the in-memory fold to match. Caller MUST hold s.mu.
//
// It writes a temp file and renames it over the index, so a crash mid-rewrite
// leaves the previous index intact rather than a half-written one: rename is
// atomic within a directory, and the temp file is in the same directory for
// exactly that reason.
func (s *sessionBinaryIndex) rewriteLocked() error {
	kept := s.prunedLocked()

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating session binary index temp file: %w", err)
	}
	var buf bytes.Buffer
	for _, rec := range kept {
		line, err := json.Marshal(rec)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("encoding session binary record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("writing session binary index temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing session binary index temp file: %w", err)
	}

	// The append handle has to let go of the old inode before the rename, and be
	// reopened on the new one after it, or subsequent appends would land in a file
	// nothing reads.
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	if err := os.Rename(tmp, s.path); err != nil {
		// The old index is still in place; reopen it so appends keep working. The
		// in-memory fold is deliberately NOT pruned in this branch — it still matches
		// what is on disk, which is more than the pruned set.
		s.reopenLocked()
		_ = os.Remove(tmp)
		return fmt.Errorf("replacing session binary index: %w", err)
	}
	if err := s.reopenLocked(); err != nil {
		return err
	}

	// Resync the fold to what was actually written. Keeping evicted bindings in
	// memory would make a lookup answer from data no restart could reproduce, so
	// the in-memory and on-disk views would disagree about what this broker knows.
	next := make(map[string]sessionBinaryRecord, len(kept))
	for _, rec := range kept {
		next[rec.SessionID] = rec
	}
	s.bindings = next
	s.sinceCompact = 0
	return nil
}

// reopenLocked opens the index for appending. Caller MUST hold s.mu.
func (s *sessionBinaryIndex) reopenLocked() error {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening session binary index %q: %w", s.path, err)
	}
	s.f = f
	return nil
}

// readSessionBinaryIndex parses an index file, returning the records it could
// read and how many segments it had to skip. A missing file is not an error: an
// unused state_dir is the normal first-boot state.
//
// TOLERANCE IS THE POINT, exactly as in readLeaseJournal. A broker killed
// mid-write leaves a final line with no terminating newline, and a full stop over
// that would make one interrupted write cost every binding in the file — and,
// worse, would keep the broker from booting at all over data that is only ever
// advisory. So:
//
//   - a segment that does not parse is skipped and counted;
//   - a record missing a session id or a binary is skipped and counted; both are
//     required, and a record without them is either corrupt or says nothing;
//   - the final segment is skipped even if it parses, whenever the file does not
//     end in a newline — a torn record is not made trustworthy by happening to be
//     valid JSON up to the cut.
//
// A file written by an older broker, or by a newer one carrying extra keys, loads
// cleanly: unknown JSON keys are ignored by encoding/json and the two fields this
// reader requires have been required since the file was introduced.
func readSessionBinaryIndex(path string) ([]sessionBinaryRecord, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("reading %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, 0, nil
	}

	torn := data[len(data)-1] != '\n'
	segments := bytes.Split(data, []byte{'\n'})
	// A well-terminated file ends with an empty trailing segment; drop it so it is
	// not mistaken for a record.
	if !torn {
		segments = segments[:len(segments)-1]
	}

	var (
		recs    []sessionBinaryRecord
		skipped int
	)
	for i, seg := range segments {
		if len(bytes.TrimSpace(seg)) == 0 {
			continue
		}
		if torn && i == len(segments)-1 {
			skipped++
			continue
		}
		var rec sessionBinaryRecord
		if err := json.Unmarshal(seg, &rec); err != nil || rec.SessionID == "" || rec.Binary == "" {
			skipped++
			continue
		}
		recs = append(recs, rec)
	}
	return recs, skipped, nil
}
