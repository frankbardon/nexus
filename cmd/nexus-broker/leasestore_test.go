package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// newPersistentRegistry wires a registry to a file-backed lease store rooted at
// dir, returning both plus the journal path. It is the fixture every durability
// test builds on, so they all exercise the SAME wiring main.go uses rather than
// poking the store directly.
func newPersistentRegistry(t *testing.T, dir, brokerID, advertiseAddr string) (*Registry, *fileLeaseStore, string) {
	t.Helper()
	store, err := openFileLeaseStore(testLogger(), dir)
	if err != nil {
		t.Fatalf("openFileLeaseStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reg := NewRegistry(testLogger(), 0)
	reg.useLeaseStore(store, brokerID, advertiseAddr)
	return reg, store, filepath.Join(dir, leaseJournalName)
}

// readJournalOrFail parses a journal and fails the test on an unreadable file.
func readJournalOrFail(t *testing.T, path string) ([]LeaseRecord, int) {
	t.Helper()
	recs, skipped, err := readLeaseJournal(path)
	if err != nil {
		t.Fatalf("readLeaseJournal(%s): %v", path, err)
	}
	return recs, skipped
}

// kindsOf reduces a record slice to its kinds, for order assertions.
func kindsOf(recs []LeaseRecord) []string {
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		out = append(out, string(rec.Kind))
	}
	return out
}

// TestLeaseStore_CreateReleaseRoundTrip is the base case: a lease minted,
// spawned, session-reported and released leaves a journal that tells the whole
// story, and a live view that is empty once it is gone.
func TestLeaseStore_CreateReleaseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	reg, store, journal := newPersistentRegistry(t, dir, "broker-eu-1", "wss://broker-eu-1.example.com")

	id, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	reg.SetProcess(id, newFakeProcess(4242))
	reg.MarkSessionID(id, "sess-abc")

	// While live: exactly one folded record, carrying every required field.
	live, err := store.Live()
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("Live() = %d records, want 1", len(live))
	}
	rec := live[0]
	if rec.LeaseID != id {
		t.Errorf("LeaseID = %q, want %q", rec.LeaseID, id)
	}
	if rec.Owner.ID != "ci-runner" || rec.Owner.Tenant != "acme" {
		t.Errorf("owner = %+v, want the claiming principal", rec.Owner)
	}
	if len(rec.Owner.Scopes) != 2 {
		t.Errorf("owner scopes = %v, want both granted scopes", rec.Owner.Scopes)
	}
	if rec.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want sess-abc", rec.SessionID)
	}
	if rec.PID != 4242 {
		t.Errorf("PID = %d, want 4242", rec.PID)
	}
	if rec.BrokerID != "broker-eu-1" {
		t.Errorf("BrokerID = %q, want broker-eu-1", rec.BrokerID)
	}
	// The RAW configured advertise_addr, not a derived host: a record must
	// round-trip what the operator wrote in broker.yaml.
	if rec.AdvertiseAddr != "wss://broker-eu-1.example.com" {
		t.Errorf("AdvertiseAddr = %q, want the raw configured value", rec.AdvertiseAddr)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if rec.ReleasedAt != nil {
		t.Errorf("ReleasedAt = %v on a live lease, want nil", rec.ReleasedAt)
	}

	// Release through the shared teardown path.
	if err := reg.releaseLease(id, "manual release", 10*time.Millisecond); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}

	live, _ = store.Live()
	if len(live) != 0 {
		t.Fatalf("Live() = %d records after release, want 0", len(live))
	}

	recs, skipped := readJournalOrFail(t, journal)
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	// created → updated (pid) → updated (session) → released, in write order.
	gotKinds := strings.Join(kindsOf(recs), ",")
	wantKinds := "lease-created,lease-updated,lease-updated,lease-released"
	if gotKinds != wantKinds {
		t.Fatalf("journal kinds = %q, want %q", gotKinds, wantKinds)
	}
	final := recs[len(recs)-1]
	if final.ReleasedAt == nil || final.ReleasedAt.IsZero() {
		t.Error("released record carries no released_at")
	}
	if final.Reason != "manual release" {
		t.Errorf("released reason = %q, want manual release", final.Reason)
	}
	if final.PID != 4242 || final.SessionID != "sess-abc" {
		t.Errorf("released record lost pid/session: %+v", final)
	}
}

// TestLeaseStore_ReleaseRecordedOnEveryTeardownPath is the reason the hook lives
// in Remove rather than releaseLease: crash teardown (watchExit) calls Remove
// DIRECTLY and never enters releaseLease, so a hook there would leave a crashed
// instance recorded on disk as still running.
func TestLeaseStore_ReleaseRecordedOnEveryTeardownPath(t *testing.T) {
	cases := []struct {
		name     string
		teardown func(t *testing.T, reg *Registry, id string)
		want     string
	}{
		{
			name: "manual release",
			teardown: func(t *testing.T, reg *Registry, id string) {
				if err := reg.releaseLease(id, "manual release", 10*time.Millisecond); err != nil {
					t.Fatalf("releaseLease: %v", err)
				}
			},
			want: "manual release",
		},
		{
			name: "idle sweep",
			teardown: func(t *testing.T, reg *Registry, id string) {
				if err := reg.releaseLease(id, reasonIdle, 10*time.Millisecond); err != nil {
					t.Fatalf("releaseLease: %v", err)
				}
			},
			want: reasonIdle,
		},
		{
			name: "crash",
			teardown: func(t *testing.T, reg *Registry, id string) {
				p := newFakeProcess(99)
				reg.SetProcess(id, p)
				close(p.exited)
				// watchExit is the crash path and it calls Remove directly.
				reg.watchExit(id)
			},
			want: reasonCrash,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			reg, store, journal := newPersistentRegistry(t, dir, "b1", "")
			id, err := reg.NewLease(anonymousOwner())
			if err != nil {
				t.Fatalf("NewLease: %v", err)
			}
			tc.teardown(t, reg, id)

			live, _ := store.Live()
			if len(live) != 0 {
				t.Fatalf("Live() = %d after %s teardown, want 0", len(live), tc.name)
			}
			recs, _ := readJournalOrFail(t, journal)
			final := recs[len(recs)-1]
			if final.Kind != leaseRecordReleased {
				t.Fatalf("final record kind = %q, want lease-released", final.Kind)
			}
			if final.Reason != tc.want {
				t.Errorf("released reason = %q, want %q", final.Reason, tc.want)
			}
		})
	}
}

// TestLeaseStore_TruncatedTrailingRecordTolerated covers the broker being killed
// mid-write: the torn final line is skipped with a count, and every complete
// record before it survives. One interrupted write must not cost the file.
func TestLeaseStore_TruncatedTrailingRecordTolerated(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, leaseJournalName)

	good := `{"kind":"lease-created","lease_id":"aaa","owner":{"id":"u1"},"broker_id":"b1","created_at":"2026-01-01T00:00:00Z"}`
	// A second record cut off mid-way, exactly as a kill -9 during Write leaves
	// it: no terminating newline, and invalid JSON.
	torn := `{"kind":"lease-created","lease_id":"bbb","owner":{"id":"u`
	if err := os.WriteFile(journal, []byte(good+"\n"+torn), 0o600); err != nil {
		t.Fatalf("seeding journal: %v", err)
	}

	recs, skipped := readJournalOrFail(t, journal)
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(recs) != 1 || recs[0].LeaseID != "aaa" {
		t.Fatalf("records = %+v, want only the complete one", recs)
	}

	// Opening the store must succeed — not fail the whole file — and the complete
	// lease must come back live.
	store, err := openFileLeaseStore(testLogger(), dir)
	if err != nil {
		t.Fatalf("openFileLeaseStore over a torn journal: %v", err)
	}
	defer func() { _ = store.Close() }()
	live, _ := store.Live()
	if len(live) != 1 || live[0].LeaseID != "aaa" {
		t.Fatalf("Live() = %+v, want the one complete lease", live)
	}
}

// TestLeaseStore_TruncatedButParseableTrailingRecordSkipped covers the subtler
// half of the same failure: a write cut at a point where the prefix still parses
// as JSON. A torn record is not made trustworthy by being valid up to the cut,
// so the missing newline alone must disqualify it.
func TestLeaseStore_TruncatedButParseableTrailingRecordSkipped(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, leaseJournalName)
	line := `{"kind":"lease-created","lease_id":"aaa","owner":{"id":"u1"},"broker_id":"b1","created_at":"2026-01-01T00:00:00Z"}`
	// Well-formed JSON, but no trailing newline: the writer never finished.
	if err := os.WriteFile(journal, []byte(line), 0o600); err != nil {
		t.Fatalf("seeding journal: %v", err)
	}
	recs, skipped := readJournalOrFail(t, journal)
	if skipped != 1 || len(recs) != 0 {
		t.Fatalf("records = %+v, skipped = %d; want the unterminated record skipped", recs, skipped)
	}
}

// TestLeaseStore_TwoStateDirsIsolated proves the per-broker rule: two brokers
// with distinct state_dirs neither read nor corrupt each other's state.
func TestLeaseStore_TwoStateDirsIsolated(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	regA, storeA, journalA := newPersistentRegistry(t, dirA, "broker-a", "a.example:8080")
	regB, storeB, journalB := newPersistentRegistry(t, dirB, "broker-b", "b.example:8080")

	idA, err := regA.NewLease(nexusauth.Principal{ID: "alice"})
	if err != nil {
		t.Fatalf("NewLease A: %v", err)
	}
	idB, err := regB.NewLease(nexusauth.Principal{ID: "bob"})
	if err != nil {
		t.Fatalf("NewLease B: %v", err)
	}

	// Each store sees only its own lease.
	liveA, _ := storeA.Live()
	liveB, _ := storeB.Live()
	if len(liveA) != 1 || liveA[0].LeaseID != idA || liveA[0].BrokerID != "broker-a" {
		t.Fatalf("store A live = %+v, want only broker-a's lease", liveA)
	}
	if len(liveB) != 1 || liveB[0].LeaseID != idB || liveB[0].BrokerID != "broker-b" {
		t.Fatalf("store B live = %+v, want only broker-b's lease", liveB)
	}

	// Releasing on A must not touch B's journal.
	if err := regA.releaseLease(idA, "manual release", 10*time.Millisecond); err != nil {
		t.Fatalf("releaseLease A: %v", err)
	}
	liveB, _ = storeB.Live()
	if len(liveB) != 1 || liveB[0].LeaseID != idB {
		t.Fatalf("store B live = %+v after A released, want B untouched", liveB)
	}

	// And neither file mentions the other broker's lease id at the byte level.
	rawA, err := os.ReadFile(journalA)
	if err != nil {
		t.Fatalf("reading journal A: %v", err)
	}
	rawB, err := os.ReadFile(journalB)
	if err != nil {
		t.Fatalf("reading journal B: %v", err)
	}
	if bytes.Contains(rawA, []byte(idB)) {
		t.Error("journal A mentions broker B's lease id")
	}
	if bytes.Contains(rawB, []byte(idA)) {
		t.Error("journal B mentions broker A's lease id")
	}
}

// failingLeaseStore is a LeaseStore whose every Append fails. It is how the
// non-fatal-write rule is proven: durability must never become a new way for the
// broker to refuse service.
type failingLeaseStore struct {
	mu      sync.Mutex
	appends int
}

func (s *failingLeaseStore) Append(LeaseRecord) error {
	s.mu.Lock()
	s.appends++
	s.mu.Unlock()
	return errors.New("disk on fire")
}

func (s *failingLeaseStore) Live() ([]LeaseRecord, error) { return nil, nil }

func (s *failingLeaseStore) Close() error { return nil }

func (s *failingLeaseStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appends
}

// TestLeaseStore_WriteFailureIsNotFatal: every journal write fails, and the
// lease lifecycle proceeds exactly as if there were no store at all.
func TestLeaseStore_WriteFailureIsNotFatal(t *testing.T) {
	store := &failingLeaseStore{}
	reg := NewRegistry(testLogger(), 0)
	reg.useLeaseStore(store, "b1", "")

	id, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease failed on a failing store: %v", err)
	}
	if !reg.Has(id) {
		t.Fatal("lease was not minted despite the write failure being non-fatal")
	}
	reg.SetProcess(id, newFakeProcess(7))

	if err := reg.releaseLease(id, "manual release", 10*time.Millisecond); err != nil {
		t.Fatalf("releaseLease failed on a failing store: %v", err)
	}
	if reg.Has(id) {
		t.Fatal("lease survived teardown despite the write failure being non-fatal")
	}
	if store.count() == 0 {
		t.Fatal("no append was attempted; the test proves nothing")
	}
}

// TestLeaseStore_PersistenceDisabled: an unset state_dir writes nothing, creates
// nothing, and leaves the lease lifecycle byte-identical to the pre-durability
// broker.
func TestLeaseStore_PersistenceDisabled(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.StateDir != "" {
		t.Fatalf("DefaultConfig().StateDir = %q, want empty (persistence off by default)", cfg.StateDir)
	}
	store, brokerID, err := openLeaseStore(testLogger(), cfg)
	if err != nil {
		t.Fatalf("openLeaseStore with no state_dir: %v", err)
	}
	// A genuine nil interface, not a typed nil — the registry's nil check depends
	// on it, and a typed nil would silently re-enable a store that writes nowhere.
	if store != nil {
		t.Fatalf("openLeaseStore returned %v, want a nil LeaseStore", store)
	}
	if brokerID != "" {
		t.Errorf("broker id = %q with persistence off, want empty", brokerID)
	}

	// Nothing is created anywhere: run the full lifecycle against a registry
	// wired exactly as main.go wires it when state_dir is unset, with a temp dir
	// standing in as the place a stray write would land.
	dir := t.TempDir()
	reg := NewRegistry(testLogger(), 0)
	reg.useLeaseStore(store, brokerID, "")
	id, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	reg.SetProcess(id, newFakeProcess(11))
	reg.MarkSessionID(id, "sess-1")
	if err := reg.releaseLease(id, "manual release", 10*time.Millisecond); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("persistence-disabled broker wrote %d entries", len(entries))
	}
}

// TestLeaseStore_NoSecretsInRecords asserts on the RAW FILE BYTES, not on the
// record struct: the point is that no secret reaches the disk by ANY route, and
// a struct assertion would only re-state the fields the projection already
// chose. A spawn secret persisted here would authenticate nothing (the process
// it belongs to died with the broker) while sitting on disk looking live.
func TestLeaseStore_NoSecretsInRecords(t *testing.T) {
	dir := t.TempDir()
	reg, _, journal := newPersistentRegistry(t, dir, "b1", "")

	tickets := newTicketStore(testLogger(), true)
	reg.useTicketStore(tickets)

	id, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	spawnSecret, err := newSpawnSecret()
	if err != nil {
		t.Fatalf("newSpawnSecret: %v", err)
	}
	reg.SetSpawnSecret(id, spawnSecret)
	ticket, err := tickets.mint(id, "ci-runner")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	reg.SetProcess(id, newFakeProcess(31))
	reg.MarkSessionID(id, "sess-secretless")
	if err := reg.releaseLease(id, "manual release", 10*time.Millisecond); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}

	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("reading journal: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("journal is empty; the test would pass vacuously")
	}
	if !bytes.Contains(raw, []byte(id)) {
		t.Fatal("journal does not mention the lease id; the test would pass vacuously")
	}
	if bytes.Contains(raw, []byte(spawnSecret)) {
		t.Error("the lease spawn secret reached the journal")
	}
	if ticket == "" {
		t.Fatal("no ticket was minted; the ticket assertion would pass vacuously")
	}
	if bytes.Contains(raw, []byte(ticket)) {
		t.Error("a client WebSocket ticket reached the journal")
	}
	// The owner's raw claim set is deliberately not persisted either.
	if bytes.Contains(raw, []byte("https://idp.example")) {
		t.Error("the owner's raw claim set reached the journal")
	}
}

// TestLeaseStore_ConcurrentCreateAndRelease drives create and release from many
// goroutines at once (run under -race): the journal must stay parseable and the
// fold must end empty, with no lost or duplicated release.
func TestLeaseStore_ConcurrentCreateAndRelease(t *testing.T) {
	dir := t.TempDir()
	reg, store, journal := newPersistentRegistry(t, dir, "b1", "")

	const workers = 24
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			id, err := reg.NewLease(testOwner())
			if err != nil {
				t.Errorf("NewLease: %v", err)
				return
			}
			reg.SetProcess(id, newFakeProcess(1000))
			reg.MarkSessionID(id, "sess-"+id)
			if err := reg.releaseLease(id, "manual release", 10*time.Millisecond); err != nil {
				t.Errorf("releaseLease: %v", err)
			}
		}()
	}
	wg.Wait()

	live, err := store.Live()
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("Live() = %d after every lease was released, want 0", len(live))
	}
	recs, skipped := readJournalOrFail(t, journal)
	if skipped != 0 {
		t.Errorf("skipped = %d under concurrency, want 0 (records must not interleave)", skipped)
	}
	released := 0
	for _, rec := range recs {
		if rec.Kind == leaseRecordReleased {
			released++
		}
	}
	if released != workers {
		t.Errorf("released records = %d, want %d", released, workers)
	}
}

// TestLeaseStore_CompactionBoundsGrowth proves the journal does not grow without
// bound: churning far more than compactEvery records leaves a file holding
// roughly the live set, not the whole history.
func TestLeaseStore_CompactionBoundsGrowth(t *testing.T) {
	dir := t.TempDir()
	reg, store, journal := newPersistentRegistry(t, dir, "b1", "")

	// One long-lived lease, so compaction is proven to KEEP live state rather
	// than just truncate everything.
	keep, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}

	// Each cycle appends 3 records (created, updated, released), so this is
	// comfortably past compactEvery.
	for i := 0; i < compactEvery; i++ {
		id, err := reg.NewLease(testOwner())
		if err != nil {
			t.Fatalf("NewLease %d: %v", i, err)
		}
		reg.SetProcess(id, newFakeProcess(i+1))
		if err := reg.releaseLease(id, "manual release", 10*time.Millisecond); err != nil {
			t.Fatalf("releaseLease %d: %v", i, err)
		}
	}

	recs, skipped := readJournalOrFail(t, journal)
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	// The bound is live + compactEvery. Without compaction this file would hold
	// ~3*compactEvery records.
	if len(recs) > compactEvery+1 {
		t.Fatalf("journal holds %d records after %d lease cycles; compaction did not bound growth",
			len(recs), compactEvery)
	}
	live, _ := store.Live()
	if len(live) != 1 || live[0].LeaseID != keep {
		t.Fatalf("Live() = %+v, want only the still-live lease", live)
	}
}

// TestLeaseStore_RewriteOnOpenDropsReleasedLeases covers the other half of the
// growth bound: reopening a state_dir compacts what was left behind, so a
// restart never inherits a file full of long-dead leases.
func TestLeaseStore_RewriteOnOpenDropsReleasedLeases(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, leaseJournalName)

	reg, store, _ := newPersistentRegistry(t, dir, "b1", "")
	liveID, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	deadID, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	if err := reg.releaseLease(deadID, "manual release", 10*time.Millisecond); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before, _ := readJournalOrFail(t, journal)
	if len(before) < 3 {
		t.Fatalf("journal holds %d records before reopen, want the full history", len(before))
	}

	reopened, err := openFileLeaseStore(testLogger(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	after, skipped := readJournalOrFail(t, journal)
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if len(after) != 1 || after[0].LeaseID != liveID || after[0].Kind != leaseRecordCreated {
		t.Fatalf("compacted journal = %+v, want exactly the live lease", after)
	}
	live, _ := reopened.Live()
	if len(live) != 1 || live[0].LeaseID != liveID {
		t.Fatalf("Live() after reopen = %+v, want the surviving lease", live)
	}
}

// TestLeaseStore_LateUpdateDoesNotResurrectReleasedLease covers the race the
// tombstone set exists for: SetProcess/MarkSessionID read a lease under the
// registry lock and write outside it, so a teardown can land in between. The
// late record must not put a dead lease back in the live view.
func TestLeaseStore_LateUpdateDoesNotResurrectReleasedLease(t *testing.T) {
	dir := t.TempDir()
	store, err := openFileLeaseStore(testLogger(), dir)
	if err != nil {
		t.Fatalf("openFileLeaseStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	created := LeaseRecord{Kind: leaseRecordCreated, LeaseID: "zzz", BrokerID: "b1", CreatedAt: time.Now()}
	released := created
	released.Kind = leaseRecordReleased
	at := time.Now()
	released.ReleasedAt = &at
	late := created
	late.Kind = leaseRecordUpdated
	late.PID = 5

	for _, rec := range []LeaseRecord{created, released, late} {
		if err := store.Append(rec); err != nil {
			t.Fatalf("Append %s: %v", rec.Kind, err)
		}
	}
	live, _ := store.Live()
	if len(live) != 0 {
		t.Fatalf("Live() = %+v, want the released lease to stay released", live)
	}
}

// TestResolveBrokerID_StableAcrossRestarts: a generated id is persisted in
// state_dir and reused, which is what lets a restart tell its own records apart
// from another broker's.
func TestResolveBrokerID_StableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := resolveBrokerID("", dir)
	if err != nil {
		t.Fatalf("resolveBrokerID: %v", err)
	}
	if first == "" {
		t.Fatal("generated broker id is empty")
	}
	second, err := resolveBrokerID("", dir)
	if err != nil {
		t.Fatalf("resolveBrokerID (restart): %v", err)
	}
	if second != first {
		t.Errorf("broker id changed across restarts: %q then %q", first, second)
	}

	// A different state_dir is a different broker, and must get a different id —
	// otherwise a shared store could not tell two brokers apart.
	other, err := resolveBrokerID("", t.TempDir())
	if err != nil {
		t.Fatalf("resolveBrokerID (other dir): %v", err)
	}
	if other == first {
		t.Error("two state_dirs produced the same broker id")
	}
}

// TestResolveBrokerID_ConfiguredWins: an explicit broker_id is used verbatim and
// no id file is written, so an operator naming their brokers gets exactly the
// names they chose.
func TestResolveBrokerID_ConfiguredWins(t *testing.T) {
	dir := t.TempDir()
	id, err := resolveBrokerID("  broker-eu-1  ", dir)
	if err != nil {
		t.Fatalf("resolveBrokerID: %v", err)
	}
	if id != "broker-eu-1" {
		t.Errorf("broker id = %q, want broker-eu-1 (trimmed)", id)
	}
	if _, err := os.Stat(filepath.Join(dir, brokerIDFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a configured broker_id still wrote an id file")
	}
}

// TestOpenLeaseStore_CreatesStateDirAndBrokerID covers the wiring main.go uses:
// a configured state_dir is created on demand and yields a usable store plus an
// id.
func TestOpenLeaseStore_CreatesStateDirAndBrokerID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	cfg := DefaultConfig()
	cfg.StateDir = dir

	store, brokerID, err := openLeaseStore(testLogger(), cfg)
	if err != nil {
		t.Fatalf("openLeaseStore: %v", err)
	}
	if store == nil {
		t.Fatal("openLeaseStore returned no store for a configured state_dir")
	}
	defer func() { _ = store.Close() }()
	if brokerID == "" {
		t.Error("no broker id was resolved")
	}
	if _, err := os.Stat(filepath.Join(dir, leaseJournalName)); err != nil {
		t.Errorf("journal was not created: %v", err)
	}
}

// TestLeaseStore_AppendAfterCloseErrorsWithoutPanicking: Close must leave the
// store inert rather than crash a broker mid-shutdown, and the error is the
// caller's to log.
func TestLeaseStore_AppendAfterCloseErrorsWithoutPanicking(t *testing.T) {
	store, err := openFileLeaseStore(testLogger(), t.TempDir())
	if err != nil {
		t.Fatalf("openFileLeaseStore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got: %v", err)
	}
	if err := store.Append(LeaseRecord{Kind: leaseRecordCreated, LeaseID: "x"}); err == nil {
		t.Error("Append after Close returned no error")
	}
}
