package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// leaseJournalName is the file inside state_dir that holds the append-only
// lease journal.
const leaseJournalName = "leases.jsonl"

// brokerIDFileName is the file inside state_dir that holds this broker's
// generated identity, when the operator did not configure one. See
// resolveBrokerID for why it lives next to the journal.
const brokerIDFileName = "broker-id"

// compactEvery is how many appends may accumulate before the journal is
// rewritten in place, keeping only the leases that are still live.
//
// It is the "does not grow without bound" guarantee together with the
// rewrite-on-open pass: at any moment the file holds at most (live leases +
// compactEvery) records, regardless of how many leases have come and gone. A
// counter is used rather than a byte size or a timer because the journal is
// only ever appended to by this process, so counting our own writes needs no
// stat() and no background goroutine to own, start or stop.
const compactEvery = 512

// leaseRecordKind names the lifecycle transition a journal record describes.
type leaseRecordKind string

const (
	// leaseRecordCreated is written the moment a lease is minted, before any
	// process exists for it. It is deliberately the FIRST thing that happens to
	// a lease on disk: a record written later would leave a window in which a
	// spawned instance is unaccounted for, which is the exact orphan this
	// journal exists to prevent.
	leaseRecordCreated leaseRecordKind = "lease-created"

	// leaseRecordUpdated is written when a field that only becomes knowable
	// AFTER the lease exists is filled in — the instance's pid (recorded once
	// the process is spawned) and the engine session id (reported by the
	// instance over its dial-back connection).
	//
	// It exists because pid and session_id are required record fields and
	// neither one is knowable at mint time. An append-only journal expresses
	// that the only way it can: a later record for the same lease id supersedes
	// an earlier one (see fileLeaseStore.foldLocked).
	leaseRecordUpdated leaseRecordKind = "lease-updated"

	// leaseRecordReleased is written when a lease is torn down, from
	// Registry.Remove — the single point manual release, the idle sweeper and
	// crash detection all converge on.
	leaseRecordReleased leaseRecordKind = "lease-released"
)

// LeaseOwnerRecord is the persisted projection of a lease's owning Principal.
//
// It carries ID, Tenant and Scopes and deliberately OMITS Principal.Claims. ID
// is the only field ownership compares, Tenant and Scopes are what a scoped
// listing needs, and the raw claim set is the widest, least predictable surface
// on a Principal — an issuer is free to put anything in it, including material
// an operator would not choose to write to a file that outlives the process.
// Nothing after a restart reads it, so persisting it would buy exposure and no
// capability. The full claim set remains in the broker's slog audit trail.
type LeaseOwnerRecord struct {
	ID     string   `json:"id"`
	Tenant string   `json:"tenant,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

// LeaseRecord is one line of the lease journal.
//
// NO SECRET MAY EVER BE ADDED TO THIS STRUCT. Not the lease's spawn secret, not
// a client WebSocket ticket, not a bearer token. Everything here is bookkeeping
// an operator could read out of `GET /leases` anyway.
//
// The spawn secret is the interesting case, because restart recovery genuinely
// needs it: a restored lease must recognise the instance that reattaches to it.
// The answer is DERIVATION, not persistence — the secret is recomputed from a
// broker-held key plus the lease id (see spawnKey), so a restarted broker knows
// the value without a bearer credential ever touching this file. Writing the
// secret here instead would put a live credential in a journal whose whole point
// is to be readable, alongside the lease id it unlocks.
type LeaseRecord struct {
	// Kind is the lifecycle transition this record describes.
	Kind leaseRecordKind `json:"kind"`

	// LeaseID is the lease this record is about. It is the fold key: a later
	// record for the same id supersedes an earlier one.
	LeaseID string `json:"lease_id"`

	// Owner is the identity that claimed the lease. Without it a restored lease
	// would be one nobody can be shown to own.
	Owner LeaseOwnerRecord `json:"owner"`

	// SessionID is the engine session the instance is running, once it has
	// reported one. Empty until then.
	SessionID string `json:"session_id,omitempty"`

	// Binary is the `binaries:` registry entry the instance was spawned from —
	// the NAME, not the path, so the record still identifies the variant after an
	// operator repoints that entry at a new build. Paired with SessionID it is the
	// durable session → binary mapping a resume is checked against, which is why
	// it is written on every record the way SessionID is rather than only on the
	// mint.
	//
	// EMPTY IS A FIRST-CLASS VALUE and omitempty is load-bearing: a journal
	// written by a broker that predates this field carries no `binary` key at all,
	// deserializes cleanly to "", and must keep loading, folding and compacting
	// exactly as it did. "" therefore means "not recorded" — never "the binary
	// named empty string" — and readers must treat it as unknown.
	Binary string `json:"binary,omitempty"`

	// PID is the spawned instance's OS process id, once it has been spawned.
	// Zero until then.
	PID int `json:"pid,omitempty"`

	// BrokerID identifies the broker that owns this lease, and AdvertiseAddr is
	// the address its clients reach it on — verbatim as the operator wrote it in
	// broker.yaml, so a record round-trips the configured value rather than a
	// derived one.
	//
	// Both are forward-compatibility hooks for multi-broker cooperation, which is
	// explicitly NOT implemented here: nothing reads another broker's records,
	// and there is no shared store or routing. They are stamped now so that when
	// a shared backend arrives it needs no data migration.
	BrokerID      string `json:"broker_id"`
	AdvertiseAddr string `json:"advertise_addr,omitempty"`

	// CreatedAt is when the lease was minted; ReleasedAt is when it was torn
	// down, absent while the lease is live. Reason carries the teardown cause
	// ("manual release", "idle", "crash") on a released record.
	CreatedAt  time.Time  `json:"created_at"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

// LeaseStore persists lease lifecycle records so a broker restart can account
// for the instances it left running.
//
// It is an INTERFACE with exactly one implementation, which is a deliberate
// exception to this project's don't-genericize-early rule: multi-broker
// cooperation is a stated near-term goal, and a shared backend is the one thing
// this seam exists for. Every record already carries broker_id and
// advertise_addr so that swap is an implementation change, not a data migration.
//
// Implementations must be safe for concurrent use: leases are created and torn
// down from HTTP handlers, the idle sweeper and the crash watcher, all at once.
type LeaseStore interface {
	// Append persists one record. Its error is for the CALLER TO LOG, never to
	// fail a claim or a release on — durability must not become a new way for
	// the broker to refuse service.
	Append(rec LeaseRecord) error

	// Live returns the most recent record for every lease that has not been
	// released, in creation order. It is the read primitive restart recovery is
	// built on; recovery itself (liveness checks, slot re-holding, reattach) is
	// not this store's business.
	Live() ([]LeaseRecord, error)

	// Close releases the underlying handle. Appending after Close is an error,
	// not a panic.
	Close() error
}

// fileLeaseStore is the file-backed LeaseStore: an append-only JSONL journal
// under state_dir, with an in-memory fold of the live leases.
//
// Growth is bounded by two passes over the same rewrite primitive: the journal
// is compacted once when it is opened (so a restart never inherits a file full
// of long-released leases) and again every compactEvery appends. A compacted
// journal contains exactly the live leases, one record each.
type fileLeaseStore struct {
	logger *slog.Logger

	// path is the journal file. It lives in the per-broker state_dir, which is
	// what keeps two brokers from reading or corrupting each other's state: they
	// simply never name the same file.
	path string

	mu sync.Mutex

	// f is the append handle. Nil after Close, and briefly during a rewrite;
	// Append reports an error rather than panicking in either case.
	f *os.File

	// syncFile is the fsync seam: every durability barrier in this file goes
	// through it, and it is (*os.File).Sync in production.
	//
	// It exists because host-crash durability cannot be proven in-process — no
	// test can pull the power out from under a page cache — so the only honest
	// assertion is that the barrier is REACHED. Tests swap this for a recorder
	// and check which files were synced, and for a failing stub to prove the
	// failure policy. Never nil: openFileLeaseStore always seeds it.
	syncFile func(*os.File) error

	// live is the folded state: lease id → most recent record, for leases that
	// have not been released.
	live map[string]LeaseRecord

	// tombstones are lease ids seen released since the last compaction. They
	// exist because a lease's final `updated` record can, in a race, be appended
	// just after its `released` one (SetProcess/MarkSessionID read the lease
	// under the registry lock, then write outside it, and a teardown can land in
	// between). Without a tombstone that late record would resurrect a dead lease
	// in the fold. Compaction clears the set, which is also what bounds it.
	tombstones map[string]struct{}

	// sinceCompact counts appends since the last rewrite. See compactEvery.
	sinceCompact int
}

// openLeaseStore builds the broker's lease store from config and resolves the
// broker identity stamped on every record.
//
// An EMPTY state_dir disables persistence entirely: it returns a nil LeaseStore
// (a genuine nil interface, so a nil check on it is meaningful) and an empty
// broker id, and the broker then behaves exactly as it did before this existed.
//
// A state_dir that is set but unusable is a BOOT failure, not a warning. That is
// not in tension with "a write failure must not refuse service": nothing is
// being served yet, and an operator who asked for durability and silently did
// not get it would only discover the fact after a restart lost the state it was
// configured to keep.
func openLeaseStore(logger *slog.Logger, cfg Config) (LeaseStore, string, error) {
	if cfg.StateDir == "" {
		return nil, "", nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	// 0o700: the journal names sessions and principals. It is not a secret store,
	// but it is nobody else's business either.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("creating broker state_dir %q: %w", cfg.StateDir, err)
	}
	brokerID, err := resolveBrokerID(cfg.BrokerID, cfg.StateDir)
	if err != nil {
		return nil, "", err
	}
	store, err := openFileLeaseStore(logger, cfg.StateDir)
	if err != nil {
		return nil, "", err
	}
	return store, brokerID, nil
}

// resolveBrokerID decides the identity this broker stamps on its records.
//
// The id MUST be stable across restarts of the same broker, or restart recovery
// cannot tell its own records from another broker's once a shared store exists.
// Two sources, in order:
//
//  1. The `broker_id` config key, when set. An operator running a cluster wants
//     records tagged with a name that means something ("broker-eu-1"), and a
//     configured value is stable by definition.
//  2. Otherwise a random id generated once and persisted at
//     <state_dir>/broker-id, reused on every subsequent boot.
//
// Derivation from hostname+port was rejected: listen_addr is very often a
// wildcard (":8080"), so two brokers on different hosts would derive the SAME id
// — the one failure mode the id exists to prevent. state_dir is already
// per-broker, so a file inside it is unique by construction and needs no
// operator effort.
func resolveBrokerID(configured, stateDir string) (string, error) {
	if id := strings.TrimSpace(configured); id != "" {
		return id, nil
	}

	path := filepath.Join(stateDir, brokerIDFileName)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
		// An empty/truncated file is treated as absent and rewritten below: a
		// broker with no id can persist nothing usable, so refusing to boot over
		// a zero-byte file would be a worse outcome than minting a new one.
	case errors.Is(err, fs.ErrNotExist):
		// First boot with this state_dir.
	default:
		return "", fmt.Errorf("reading broker id %q: %w", path, err)
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating broker id: %w", err)
	}
	id := hex.EncodeToString(b[:])
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("persisting broker id %q: %w", path, err)
	}
	return id, nil
}

// openFileLeaseStore opens (or creates) the journal under stateDir, folds what
// is already there, and compacts it. A malformed or torn record in the existing
// file is skipped with a warning — a broker that refused to boot over one bad
// line would turn a partial loss into a total outage.
func openFileLeaseStore(logger *slog.Logger, stateDir string) (*fileLeaseStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating broker state_dir %q: %w", stateDir, err)
	}
	s := &fileLeaseStore{
		logger:     logger,
		path:       filepath.Join(stateDir, leaseJournalName),
		syncFile:   (*os.File).Sync,
		live:       make(map[string]LeaseRecord),
		tombstones: make(map[string]struct{}),
	}

	recs, skipped, err := readLeaseJournal(s.path)
	if err != nil {
		return nil, fmt.Errorf("reading lease journal: %w", err)
	}
	if skipped > 0 {
		s.logger.Warn("skipped unreadable lease journal records; the broker was most likely killed mid-write",
			"path", s.path, "skipped", skipped, "read", len(recs))
	}
	for _, rec := range recs {
		s.foldLocked(rec)
	}

	// Rewrite-on-open. This is half of the bounded-growth guarantee and it also
	// truncates any torn trailing record, so the file a running broker appends to
	// always starts from a clean, fully parseable state.
	if err := s.rewriteLocked(); err != nil {
		return nil, err
	}
	s.logger.Info("lease journal opened",
		"path", s.path, "live_leases", len(s.live), "skipped_records", skipped)
	return s, nil
}

// Append writes one record, fsyncs it, and folds it into the live view.
//
// THE FSYNC IS UNCONDITIONAL, and that is a decision rather than an oversight.
// A record left in the page cache survives `kill -9` — the process dies, the
// kernel still owns the bytes — but it does not survive a power loss or a hard
// reset, and the tail of the journal is exactly where the `lease-updated` record
// carrying the pid lives. Losing that one record is the single case recovery
// cannot repair: the next boot sees a lease with no pid, closes it out (see
// recover.go's rec.PID <= 0 branch), and the still-running instance becomes the
// orphan this journal exists to prevent.
//
// Syncing only the pid-bearing record was rejected. The volume is roughly three
// records per lease lifetime — created, updated, released — so the saving would
// be about two fsyncs per lease, while "which record matters" is a subtle
// invariant sitting in a struct field, waiting for a future contributor to add a
// fourth record kind and not know it belongs on the list.
func (s *fileLeaseStore) Append(rec LeaseRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding lease record: %w", err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return errors.New("lease journal is closed")
	}
	// One Write per record, straight to the file descriptor with no buffering in
	// front of it, so a record is either in the file or it is not — the only
	// partial record a reader can meet is the one the kernel was mid-way through,
	// which readLeaseJournal skips.
	if _, err := s.f.Write(line); err != nil {
		return fmt.Errorf("appending lease record: %w", err)
	}
	// The record is in the file either way, so it is folded and counted even when
	// the barrier fails: an fsync error means "this may not survive the host
	// dying", not "this was never written". The error is carried to the end and
	// returned so the caller logs it — Registry.appendRecord has no error return
	// precisely so no claim or release can ever fail on it.
	syncErr := s.syncFile(s.f)

	s.foldLocked(rec)

	s.sinceCompact++
	if s.sinceCompact >= compactEvery {
		if err := s.rewriteLocked(); err != nil {
			// The record IS written; compaction failing only means the file stays
			// larger than we would like. Reporting it as an append failure would
			// make a housekeeping problem look like a durability one.
			s.logger.Error("compacting lease journal failed; the journal will keep growing",
				"path", s.path, "error", err)
		}
	}
	if syncErr != nil {
		return fmt.Errorf("syncing lease journal: %w", syncErr)
	}
	return nil
}

// Live returns the live leases in creation order. Records are deep-copied so a
// caller cannot mutate the store's fold through a shared Scopes slice.
func (s *fileLeaseStore) Live() ([]LeaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveSortedLocked(), nil
}

// Close closes the append handle. It is idempotent.
func (s *fileLeaseStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	if err != nil {
		return fmt.Errorf("closing lease journal: %w", err)
	}
	return nil
}

// foldLocked applies one record to the live view. Caller MUST hold s.mu.
//
// The fold is what makes an append-only journal a state store: a later record
// for a lease id supersedes an earlier one, and a release removes the lease
// outright. Records for an already-released lease are ignored (see tombstones).
func (s *fileLeaseStore) foldLocked(rec LeaseRecord) {
	switch rec.Kind {
	case leaseRecordReleased:
		delete(s.live, rec.LeaseID)
		s.tombstones[rec.LeaseID] = struct{}{}
	case leaseRecordCreated, leaseRecordUpdated:
		if _, dead := s.tombstones[rec.LeaseID]; dead {
			return
		}
		s.live[rec.LeaseID] = rec
	default:
		// An unknown kind is from a newer broker writing into this state_dir.
		// Ignore it rather than guessing: state_dir is per-broker, so this should
		// not happen, and a wrong guess would resurrect or destroy a lease.
		s.logger.Warn("ignoring lease record with unknown kind",
			"path", s.path, "lease_id", rec.LeaseID, "kind", string(rec.Kind))
	}
}

// liveSortedLocked returns the folded live records in creation order, deep
// copied. Caller MUST hold s.mu.
func (s *fileLeaseStore) liveSortedLocked() []LeaseRecord {
	out := make([]LeaseRecord, 0, len(s.live))
	for _, rec := range s.live {
		if rec.Owner.Scopes != nil {
			scopes := make([]string, len(rec.Owner.Scopes))
			copy(scopes, rec.Owner.Scopes)
			rec.Owner.Scopes = scopes
		}
		out = append(out, rec)
	}
	// Creation order, with the lease id as tiebreak so the output (and therefore
	// a compacted file) is byte-stable for equal timestamps — tests use an
	// injected clock that hands out identical instants.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].LeaseID < out[j].LeaseID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// rewriteLocked replaces the journal with exactly the live leases, one record
// each. Caller MUST hold s.mu.
//
// It writes a temp file and renames it over the journal, so a crash mid-rewrite
// leaves the previous journal intact rather than a half-written one: rename is
// atomic within a directory, and the temp file is in the same directory for
// exactly that reason.
//
// ATOMIC IS NOT THE SAME AS DURABLE, which is why there are two barriers here
// and not none. Without them a host that dies just after the rename can come
// back to a directory entry pointing at a file whose contents never left the
// page cache — an EMPTY journal where the whole live set used to be, which is
// strictly worse than the pre-rewrite file the atomicity was protecting. So:
//
//   - the temp file is fsynced BEFORE the rename, and a failure there aborts the
//     rewrite with the old journal still in place — the safe state;
//   - the DIRECTORY is fsynced after the rename, so the rename itself is on
//     stable storage and cannot be replayed away.
func (s *fileLeaseStore) rewriteLocked() error {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating lease journal temp file: %w", err)
	}
	var buf bytes.Buffer
	for _, rec := range s.liveSortedLocked() {
		line, err := json.Marshal(rec)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("encoding lease record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("writing lease journal temp file: %w", err)
	}
	if err := s.syncFile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("syncing lease journal temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing lease journal temp file: %w", err)
	}

	// The append handle has to let go of the old inode before the rename, and be
	// reopened on the new one after it, or subsequent appends would land in a
	// file nothing reads.
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	if err := os.Rename(tmp, s.path); err != nil {
		// The old journal is still in place; reopen it so appends keep working.
		s.reopenLocked()
		_ = os.Remove(tmp)
		return fmt.Errorf("replacing lease journal: %w", err)
	}
	// The rename has already happened and the new journal is on disk, so a
	// directory that will not sync is LOGGED, not returned: the only thing at risk
	// is the rename surviving a power loss, and the caller's two handlers for a
	// rewrite error are "fail the boot" and "warn that the journal keeps growing",
	// neither of which describes this.
	if err := s.syncDirLocked(); err != nil {
		s.logger.Warn("syncing the lease journal directory failed; the rewrite may not survive a host crash",
			"path", s.path, "error", err)
	}
	if err := s.reopenLocked(); err != nil {
		return err
	}
	// A compacted journal holds no released leases, so nothing can be resurrected
	// by a late record for one — the tombstones have done their job.
	s.tombstones = make(map[string]struct{})
	s.sinceCompact = 0
	return nil
}

// syncDirLocked fsyncs the directory holding the journal. Caller MUST hold s.mu.
//
// Renaming a file changes the DIRECTORY, and a directory's own metadata is
// cached like everything else — fsyncing the renamed file would not persist the
// entry that points at it. This is the standard POSIX write-temp/rename/sync-dir
// sequence, and the last step is the one that is easiest to leave out and
// hardest to notice missing.
func (s *fileLeaseStore) syncDirLocked() error {
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return fmt.Errorf("opening lease journal directory: %w", err)
	}
	syncErr := s.syncFile(dir)
	_ = dir.Close()
	if syncErr != nil {
		return fmt.Errorf("syncing lease journal directory: %w", syncErr)
	}
	return nil
}

// reopenLocked opens the journal for appending. Caller MUST hold s.mu.
func (s *fileLeaseStore) reopenLocked() error {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening lease journal %q: %w", s.path, err)
	}
	s.f = f
	return nil
}

// readLeaseJournal parses a journal file, returning the records it could read
// and how many segments it had to skip. A missing file is not an error: an
// unused state_dir is the normal first-boot state.
//
// TOLERANCE IS THE POINT. A broker killed mid-write leaves a final line with no
// terminating newline; a full stop over that would make one interrupted write
// cost every lease in the file. So:
//
//   - a segment that does not parse is skipped and counted;
//   - the final segment is skipped even if it parses, whenever the file does not
//     end in a newline — a torn record is not made trustworthy by happening to be
//     valid JSON up to the cut.
func readLeaseJournal(path string) ([]LeaseRecord, int, error) {
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
	// A well-terminated file ends with an empty trailing segment; drop it so it
	// is not mistaken for a record.
	if !torn {
		segments = segments[:len(segments)-1]
	}

	var (
		recs    []LeaseRecord
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
		var rec LeaseRecord
		if err := json.Unmarshal(seg, &rec); err != nil || rec.LeaseID == "" {
			skipped++
			continue
		}
		recs = append(recs, rec)
	}
	return recs, skipped, nil
}

// ownerRecord projects a Principal onto its persisted form. Claims are
// deliberately dropped — see LeaseOwnerRecord.
func ownerRecord(p nexusauth.Principal) LeaseOwnerRecord {
	out := LeaseOwnerRecord{ID: p.ID, Tenant: p.Tenant}
	if p.Scopes != nil {
		out.Scopes = make([]string, len(p.Scopes))
		copy(out.Scopes, p.Scopes)
	}
	return out
}
