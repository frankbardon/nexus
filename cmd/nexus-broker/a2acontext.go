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
	"strings"
	"sync"
	"time"
)

// a2aContextIndexName is the file inside state_dir that holds the durable A2A
// context → engine session index.
//
// It is a THIRD file beside leases.jsonl and session-binaries.jsonl, and the
// separation is the same argument sessionbinary.go makes, one level up. The
// lease journal is compacted down to LIVE leases, so a released lease's session
// is dropped from it; the session→binary index is keyed by session id, which is
// precisely the thing an A2A client does not know. What a client holds is a
// contextId, and the whole point of this file is that a contextId outlives every
// lease that ever served it.
const a2aContextIndexName = "a2a-contexts.jsonl"

// maxA2AContextBindings is the most context → session pairs the index keeps.
// Once a rewrite finds more than this, the OLDEST bindings (by last-recorded
// time) are dropped until the count fits.
//
// The cap is required for the same reason the session→binary index needs one:
// nothing ever retires a binding. A conversation's whole value is that it can be
// resumed later, so no event marks one as finished, and deduplicating by context
// id bounds records per context rather than the number of contexts a broker
// meets over its lifetime.
//
// 4096 matches maxSessionBindings deliberately. The two indexes are populated by
// the same events at roughly the same rate — one A2A conversation is one engine
// session — so a broker that outgrows one has outgrown both, and a single number
// to reason about is worth more than a separately-tuned pair.
//
// Eviction is EXPLICITLY LOSSY, and the degradation is honest rather than
// silent-wrong: a pruned context is unknown, so the next message on it starts a
// FRESH session. The client is not told, which is the same treatment a dead
// lease gets — except that here the history really is left behind. That is the
// documented cost of an unbounded key space with no retirement event, and it is
// why the cap is generous.
const maxA2AContextBindings = 4096

// a2aContextCompactEvery is how many appends may accumulate before the index is
// rewritten in place, deduplicated and pruned. It matches
// sessionBinaryCompactEvery for the same reason the cap does: the two files see
// the same write rate.
const a2aContextCompactEvery = 256

// a2aContextRecord is one line of the context → session index.
//
// NO SECRET MAY EVER BE ADDED TO THIS STRUCT, for the same reason as LeaseRecord
// and sessionBinaryRecord: this file outlives the process and is written to be
// read back.
type a2aContextRecord struct {
	// OwnerID is the principal id of the caller the context belongs to, and it
	// is part of the KEY rather than a note on the record.
	//
	// That is a security property, not bookkeeping. An A2A contextId may be
	// CHOSEN BY THE CLIENT (specification section 3.4 lets a message carry one),
	// so without an owner in the key any caller could name another caller's
	// context and be handed that conversation's session — history, tool results
	// and all. Keying by owner makes a colliding contextId resolve to the
	// caller's OWN binding instead: no leak, no oracle, and no overwrite of the
	// real owner's entry.
	//
	// Empty is the anonymous owner, which is what every caller is when the broker
	// runs with no `auth:` block. Two anonymous callers therefore share a
	// namespace, which is exactly what "authentication is disabled" already means
	// for lease ownership everywhere else in this binary.
	OwnerID string `json:"owner_id,omitempty"`

	// Profile is the `agents:` profile the context was addressed to, and is also
	// part of the key: two profiles are two different agents with two different
	// configs, so one contextId under each names two unrelated conversations.
	Profile string `json:"profile"`

	// ContextID is the A2A contextId the client holds.
	ContextID string `json:"context_id"`

	// SessionID is the engine session serving that context — the value passed to
	// `nexus -recall` when the instance has to be started again.
	//
	// Empty is never written and never loaded: "" means "not recorded" everywhere
	// in the broker, and a record carrying it would occupy a capped slot while
	// answering nothing.
	SessionID string `json:"session_id"`

	// At is when the pairing was last recorded. It is what recency-based pruning
	// orders by, and it is the only reason this record has a timestamp.
	At time.Time `json:"at"`
}

// a2aContextKeySep separates the three key components. NUL is used because it
// cannot occur in a profile name (validateProfileName permits only unreserved
// URL characters) and cannot occur in a JSON string a principal id was decoded
// from without being escaped, so no two distinct triples can fold onto one key.
const a2aContextKeySep = "\x00"

// key is the index's lookup key: owner, profile and context, in that order.
func (r a2aContextRecord) key() string {
	return a2aContextKey(r.OwnerID, r.Profile, r.ContextID)
}

// a2aContextKey builds the composite key a binding is stored under.
func a2aContextKey(ownerID, profile, contextID string) string {
	return strings.Join([]string{ownerID, profile, contextID}, a2aContextKeySep)
}

// a2aContextIndex is the durable A2A context → engine session map: an
// append-only JSONL file under state_dir with an in-memory fold, independent of
// the lease journal and unaffected by its compaction.
//
// A NIL *a2aContextIndex IS A FULLY SUPPORTED VALUE and is what an unset
// state_dir produces. Every method is nil-receiver safe, so no caller needs a
// branch — a broker with no state_dir still resumes a context for as long as its
// lease lives, and forgets every context across a restart. It is a concrete
// pointer rather than an interface to avoid the typed-nil trap; there is one
// implementation and no second backend in sight.
type a2aContextIndex struct {
	logger *slog.Logger

	// path is the index file inside the per-broker state_dir.
	path string

	// now is the clock stamped on records and used to order pruning. Tests swap
	// it for a deterministic one so recency eviction needs no sleeps.
	now func() time.Time

	mu sync.Mutex

	// f is the append handle. Nil after Close, and briefly during a rewrite;
	// record() logs rather than panicking in either case.
	f *os.File

	// bindings is the folded state: composite key → most recent record.
	bindings map[string]a2aContextRecord

	// sinceCompact counts appends since the last rewrite.
	sinceCompact int
}

// openA2AContextIndex builds the broker's A2A context index from config.
//
// An EMPTY state_dir disables it entirely: it returns a nil index, nothing is
// written and no file is created. A broker configured that way still routes a
// second message on a context to the instance it started for the first one —
// that binding is in memory — and simply starts a fresh session after a restart.
//
// Like openSessionBinaryIndex and unlike openLeaseStore, an unusable index is
// NOT a boot failure. Losing the lease journal loses track of running processes;
// losing this one loses conversation continuity across a restart, which is the
// behaviour a broker with no state_dir has by configuration. Refusing to serve
// over it would trade a degraded feature for a total outage. The caller logs and
// carries on with a nil index.
func openA2AContextIndex(logger *slog.Logger, cfg Config) (*a2aContextIndex, error) {
	if cfg.StateDir == "" {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	// 0o700 to match the lease journal and the session→binary index: this file
	// names engine sessions and principal ids. It is not a secret store, but it
	// is nobody else's business either.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating broker state_dir %q: %w", cfg.StateDir, err)
	}

	idx := &a2aContextIndex{
		logger:   logger,
		path:     filepath.Join(cfg.StateDir, a2aContextIndexName),
		now:      time.Now,
		bindings: make(map[string]a2aContextRecord),
	}

	recs, skipped, err := readA2AContextIndex(idx.path)
	if err != nil {
		return nil, fmt.Errorf("reading a2a context index: %w", err)
	}
	if skipped > 0 {
		idx.logger.Warn("skipped unreadable a2a context index records; the broker was most likely killed mid-write",
			"path", idx.path, "skipped", skipped, "read", len(recs))
	}
	for _, rec := range recs {
		idx.bindings[rec.key()] = rec
	}

	// Rewrite-on-open. It deduplicates, applies the entry cap and truncates any
	// torn trailing record, so the file a running broker appends to always starts
	// from a clean, fully parseable, already-bounded state.
	if err := idx.rewriteLocked(); err != nil {
		return nil, err
	}
	idx.logger.Info("a2a context index opened",
		"path", idx.path, "bindings", len(idx.bindings), "skipped_records", skipped)
	return idx, nil
}

// record persists the pairing of an A2A context with the engine session serving
// it.
//
// It has NO ERROR RETURN by construction, mirroring Registry.appendRecord and
// sessionBinaryIndex.record: a write failure is logged and otherwise ignored,
// because a durability index must never become a new way for a turn to fail. The
// instance is already running; refusing the turn over a disk problem would turn
// a degraded resume into an outage.
//
// Empty inputs are dropped rather than stored. An empty context id or session id
// is the broker's spelling of "not recorded", and writing either would put a
// binding that answers nothing into a capped store. An empty OWNER is legitimate
// — it is the anonymous principal — so it is not checked.
//
// A pairing already recorded with the same session refreshes its recency in
// memory but does NOT append: every turn on a live context would otherwise write
// a line for a value that has not changed. The refreshed timestamp reaches disk
// at the next rewrite, which is the only place recency is consulted anyway.
//
// A nil index does nothing at all.
func (x *a2aContextIndex) record(ownerID, profile, contextID, sessionID string) {
	if x == nil || profile == "" || contextID == "" || sessionID == "" {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	rec := a2aContextRecord{
		OwnerID:   ownerID,
		Profile:   profile,
		ContextID: contextID,
		SessionID: sessionID,
		At:        x.now(),
	}
	key := rec.key()
	if prev, ok := x.bindings[key]; ok && prev.SessionID == sessionID {
		x.bindings[key] = rec
		return
	}
	x.bindings[key] = rec

	if x.f == nil {
		x.logger.Error("the a2a context index is closed; the binding was not persisted",
			"path", x.path, "profile", profile, "context_id", contextID)
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		x.logger.Error("encoding an a2a context record failed; the binding was not persisted",
			"path", x.path, "profile", profile, "context_id", contextID, "error", err)
		return
	}
	line = append(line, '\n')
	// One Write per record, straight to the file descriptor with no buffering in
	// front of it, so a record is either in the file or it is not — the only
	// partial record a reader can meet is the one the kernel was mid-way through,
	// which readA2AContextIndex skips.
	if _, err := x.f.Write(line); err != nil {
		x.logger.Error("appending an a2a context record failed; a message on this context after a restart will start a fresh conversation",
			"path", x.path, "profile", profile, "context_id", contextID, "error", err)
		return
	}

	x.sinceCompact++
	if x.sinceCompact >= a2aContextCompactEvery {
		if err := x.rewriteLocked(); err != nil {
			// The record IS written; a failed rewrite only means the file stays
			// larger than we would like. Logging it as a rewrite problem rather than
			// a write one keeps a housekeeping failure from reading as a durability
			// failure.
			x.logger.Error("compacting the a2a context index failed; the index will keep growing",
				"path", x.path, "error", err)
		}
	}
}

// lookup returns the engine session recorded for a caller's context and whether
// one is known.
//
// (id, true) means a binding was found; ("", false) means the index has no
// opinion — an unknown context, a pruned one, or no index at all. A nil index
// always reports unknown, which is what makes a broker with no state_dir start a
// fresh conversation after every restart.
func (x *a2aContextIndex) lookup(ownerID, profile, contextID string) (string, bool) {
	if x == nil || profile == "" || contextID == "" {
		return "", false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	rec, ok := x.bindings[a2aContextKey(ownerID, profile, contextID)]
	if !ok || rec.SessionID == "" {
		return "", false
	}
	return rec.SessionID, true
}

// Close closes the append handle. It is idempotent and nil-receiver safe.
func (x *a2aContextIndex) Close() error {
	if x == nil {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.f == nil {
		return nil
	}
	err := x.f.Close()
	x.f = nil
	if err != nil {
		return fmt.Errorf("closing a2a context index: %w", err)
	}
	return nil
}

// prunedLocked returns the folded bindings in recording order, oldest first,
// after dropping everything past maxA2AContextBindings. Caller MUST hold x.mu.
//
// Ordering is oldest-first so the written file reads chronologically, while the
// PRUNE takes from the front — the oldest contexts are the ones least likely to
// be resumed, and dropping them degrades to "unknown", never to a wrong answer.
func (x *a2aContextIndex) prunedLocked() []a2aContextRecord {
	out := make([]a2aContextRecord, 0, len(x.bindings))
	for _, rec := range x.bindings {
		out = append(out, rec)
	}
	// Key as tiebreak so the rewritten file is byte-stable for equal timestamps —
	// tests use an injected clock that hands out identical instants.
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].key() < out[j].key()
		}
		return out[i].At.Before(out[j].At)
	})
	if len(out) > maxA2AContextBindings {
		out = out[len(out)-maxA2AContextBindings:]
	}
	return out
}

// rewriteLocked replaces the index with exactly the surviving bindings, one
// record each, and resyncs the in-memory fold to match. Caller MUST hold x.mu.
//
// It writes a temp file and renames it over the index, so a crash mid-rewrite
// leaves the previous index intact rather than a half-written one: rename is
// atomic within a directory, and the temp file is in the same directory for
// exactly that reason.
func (x *a2aContextIndex) rewriteLocked() error {
	kept := x.prunedLocked()

	tmp := x.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating a2a context index temp file: %w", err)
	}
	var buf bytes.Buffer
	for _, rec := range kept {
		line, err := json.Marshal(rec)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("encoding an a2a context record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("writing a2a context index temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing a2a context index temp file: %w", err)
	}

	// The append handle has to let go of the old inode before the rename, and be
	// reopened on the new one after it, or subsequent appends would land in a file
	// nothing reads.
	if x.f != nil {
		_ = x.f.Close()
		x.f = nil
	}
	if err := os.Rename(tmp, x.path); err != nil {
		// The old index is still in place; reopen it so appends keep working. The
		// in-memory fold is deliberately NOT pruned in this branch — it still
		// matches what is on disk, which is more than the pruned set.
		_ = x.reopenLocked()
		_ = os.Remove(tmp)
		return fmt.Errorf("replacing a2a context index: %w", err)
	}
	if err := x.reopenLocked(); err != nil {
		return err
	}

	// Resync the fold to what was actually written. Keeping evicted bindings in
	// memory would make a lookup answer from data no restart could reproduce, so
	// the in-memory and on-disk views would disagree about what this broker knows.
	next := make(map[string]a2aContextRecord, len(kept))
	for _, rec := range kept {
		next[rec.key()] = rec
	}
	x.bindings = next
	x.sinceCompact = 0
	return nil
}

// reopenLocked opens the index for appending. Caller MUST hold x.mu.
func (x *a2aContextIndex) reopenLocked() error {
	f, err := os.OpenFile(x.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening a2a context index %q: %w", x.path, err)
	}
	x.f = f
	return nil
}

// readA2AContextIndex parses an index file, returning the records it could read
// and how many segments it had to skip. A missing file is not an error: an
// unused state_dir is the normal first-boot state.
//
// TOLERANCE IS THE POINT, exactly as in readLeaseJournal and
// readSessionBinaryIndex. A broker killed mid-write leaves a final line with no
// terminating newline, and a full stop over that would cost every binding in the
// file. So:
//
//   - a segment that does not parse is skipped and counted;
//   - a record missing a profile, a context id or a session id is skipped and
//     counted; all three are required, and a record without them says nothing;
//   - the final segment is skipped even if it parses, whenever the file does not
//     end in a newline — a torn record is not made trustworthy by happening to be
//     valid JSON up to the cut.
//
// An owner id is deliberately NOT required: the empty string is the anonymous
// principal, which is every caller on a broker with no `auth:` block.
func readA2AContextIndex(path string) ([]a2aContextRecord, int, error) {
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
		recs    []a2aContextRecord
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
		var rec a2aContextRecord
		if err := json.Unmarshal(seg, &rec); err != nil ||
			rec.Profile == "" || rec.ContextID == "" || rec.SessionID == "" {
			skipped++
			continue
		}
		recs = append(recs, rec)
	}
	return recs, skipped, nil
}
