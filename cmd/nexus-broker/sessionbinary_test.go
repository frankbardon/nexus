package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newIndexedRegistry wires a registry to BOTH a file-backed lease store and a
// session binary index rooted at dir, the way main.go does. Every test here
// builds on it so they exercise the real wiring rather than poking the index
// directly, and so a change that records the pairing somewhere else still has to
// make these pass.
func newIndexedRegistry(t *testing.T, dir string) (*Registry, *sessionBinaryIndex, string) {
	t.Helper()
	reg, _, _ := newPersistentRegistry(t, dir, "broker-test", "")
	idx, err := openSessionBinaryIndex(testLogger(), Config{StateDir: dir})
	if err != nil {
		t.Fatalf("openSessionBinaryIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	reg.useSessionBinaryIndex(idx)
	return reg, idx, filepath.Join(dir, sessionBinaryIndexName)
}

// runSession drives one lease through the lifecycle that produces a binding:
// claim (SetBinary), spawn, session-id report (MarkSessionID). It returns the
// lease id so the caller can release it.
func runSession(t *testing.T, reg *Registry, binary, sessionID string) string {
	t.Helper()
	id, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	reg.SetBinary(id, binary)
	reg.SetProcess(id, newFakeProcess(4242))
	reg.MarkSessionID(id, sessionID)
	return id
}

// TestSessionBinaryIndex_BindingSurvivesRelease is the whole reason this index
// exists. A resume arrives AFTER the original lease was released, so the answer
// that matters is the one given once the lease is gone from memory and from the
// compacted lease journal — the exact point at which a live-leases-only lookup
// reported unknown.
func TestSessionBinaryIndex_BindingSurvivesRelease(t *testing.T) {
	dir := t.TempDir()
	reg, _, _ := newIndexedRegistry(t, dir)

	id := runSession(t, reg, "vision", "sess-survive")
	if got, ok := reg.BinaryForSession("sess-survive"); !ok || got != "vision" {
		t.Fatalf("BinaryForSession while live = (%q, %v), want (vision, true)", got, ok)
	}

	reg.Remove(id)

	// The lease is genuinely gone: this is the state a resume actually meets.
	if _, ok := reg.LeaseOwner(id); ok {
		t.Fatal("lease is still in the registry after Remove; the test is not exercising a resume")
	}
	got, ok := reg.BinaryForSession("sess-survive")
	if !ok {
		t.Fatal("BinaryForSession reported the session unknown after its lease was released — " +
			"the binding did not outlive the lease")
	}
	if got != "vision" {
		t.Errorf("BinaryForSession = %q, want vision", got)
	}
}

// TestSessionBinaryIndex_BindingSurvivesLeaseJournalCompaction pins the
// SEPARATION the story is about. Compacting the lease journal is what erases a
// released lease's record; the binding must be unaffected by it.
func TestSessionBinaryIndex_BindingSurvivesLeaseJournalCompaction(t *testing.T) {
	dir := t.TempDir()
	reg, _, _ := newIndexedRegistry(t, dir)
	store, err := openFileLeaseStore(testLogger(), dir)
	if err != nil {
		t.Fatalf("reopening lease store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	id := runSession(t, reg, "vision", "sess-compact")
	reg.Remove(id)

	// Force a lease-journal compaction rather than waiting for compactEvery, and
	// confirm it did what the story says it does: drop the released lease outright.
	store.mu.Lock()
	rewriteErr := store.rewriteLocked()
	store.mu.Unlock()
	if rewriteErr != nil {
		t.Fatalf("rewriteLocked: %v", rewriteErr)
	}
	recs, _ := readJournalOrFail(t, filepath.Join(dir, leaseJournalName))
	for _, rec := range recs {
		if rec.SessionID == "sess-compact" {
			t.Fatal("the released lease survived compaction; this test no longer proves separation")
		}
	}

	got, ok := reg.BinaryForSession("sess-compact")
	if !ok || got != "vision" {
		t.Errorf("BinaryForSession after lease-journal compaction = (%q, %v), want (vision, true)", got, ok)
	}
}

// TestSessionBinaryIndex_BindingSurvivesReopen is the restart case: a brand new
// index (and a brand new registry, holding no leases at all) reads the binding
// back off disk.
func TestSessionBinaryIndex_BindingSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	reg, idx, _ := newIndexedRegistry(t, dir)
	id := runSession(t, reg, "vision", "sess-restart")
	reg.Remove(id)
	if err := idx.Close(); err != nil {
		t.Fatalf("closing index: %v", err)
	}

	reopened, err := openSessionBinaryIndex(testLogger(), Config{StateDir: dir})
	if err != nil {
		t.Fatalf("reopening index: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	// A fresh registry with no leases at all — nothing in memory can supply this
	// answer, so it can only come from the file.
	fresh := NewRegistry(testLogger(), 0)
	fresh.useSessionBinaryIndex(reopened)
	got, ok := fresh.BinaryForSession("sess-restart")
	if !ok {
		t.Fatal("BinaryForSession reported the session unknown after a restart")
	}
	if got != "vision" {
		t.Errorf("BinaryForSession = %q, want vision", got)
	}
}

// TestSessionBinaryIndex_ResumeClaimRecordsBeforeReport covers the other write
// trigger: a resume names its session in the claim body, so the pairing is
// complete before the instance reports anything — and must be durable even if the
// instance never gets that far.
func TestSessionBinaryIndex_ResumeClaimRecordsBeforeReport(t *testing.T) {
	dir := t.TempDir()
	reg, _, _ := newIndexedRegistry(t, dir)

	// Exactly what handleClaim does for a resume, with no MarkSessionID after it.
	reg.RecordSessionBinary("sess-resume", "vision")

	got, ok := reg.BinaryForSession("sess-resume")
	if !ok || got != "vision" {
		t.Errorf("BinaryForSession = (%q, %v), want (vision, true)", got, ok)
	}
}

// TestSessionBinaryIndex_UnknownSessionReportsUnknown pins the contract E3-S2
// depends on: ("", false) is an ordinary "no opinion", not an error and not a
// mismatch.
func TestSessionBinaryIndex_UnknownSessionReportsUnknown(t *testing.T) {
	dir := t.TempDir()
	reg, _, _ := newIndexedRegistry(t, dir)
	runSession(t, reg, "vision", "sess-known")

	if got, ok := reg.BinaryForSession("sess-never-seen"); ok {
		t.Errorf("BinaryForSession on an unrecorded session = (%q, true), want unknown", got)
	}
	if got, ok := reg.BinaryForSession(""); ok {
		t.Errorf("BinaryForSession(\"\") = (%q, true), want unknown", got)
	}
}

// TestSessionBinaryIndex_EmptyBinaryIsNotRecorded guards the "empty means not
// recorded" rule at the write side. A pre-E3-S1 lease carries no binary, and a
// resume of its session must fall through to unknown rather than be handed the
// binary named empty string.
func TestSessionBinaryIndex_EmptyBinaryIsNotRecorded(t *testing.T) {
	dir := t.TempDir()
	reg, _, path := newIndexedRegistry(t, dir)

	// A lease that never had SetBinary called on it — the pre-E3-S1 shape.
	id, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	reg.MarkSessionID(id, "sess-legacy")
	reg.Remove(id)

	if got, ok := reg.BinaryForSession("sess-legacy"); ok {
		t.Errorf("BinaryForSession for a lease with no binary = (%q, true), want unknown", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading index: %v", err)
	}
	if strings.Contains(string(raw), "sess-legacy") {
		t.Errorf("an empty binding was written to the index:\n%s", raw)
	}
}

// TestSessionBinaryIndex_NoStateDirWritesNothing is the persistence-off case.
// With no state_dir there is no index, no file and no error — and the in-memory
// behaviour is unchanged, so a live lease still answers and a released one does
// not.
func TestSessionBinaryIndex_NoStateDirWritesNothing(t *testing.T) {
	idx, err := openSessionBinaryIndex(testLogger(), DefaultConfig())
	if err != nil {
		t.Fatalf("openSessionBinaryIndex with no state_dir: %v", err)
	}
	if idx != nil {
		t.Fatalf("openSessionBinaryIndex returned %v, want a nil index", idx)
	}

	// A temp dir stands in as the place a stray write would land.
	dir := t.TempDir()
	reg := NewRegistry(testLogger(), 0)
	reg.useSessionBinaryIndex(idx)

	id := runSession(t, reg, "vision", "sess-nostate")
	if got, ok := reg.BinaryForSession("sess-nostate"); !ok || got != "vision" {
		t.Errorf("BinaryForSession while live = (%q, %v), want (vision, true) with persistence off", got, ok)
	}
	reg.RecordSessionBinary("sess-nostate-resume", "vision")

	reg.Remove(id)
	// No index means no durable answer — the pre-index behaviour, exactly.
	if got, ok := reg.BinaryForSession("sess-nostate"); ok {
		t.Errorf("BinaryForSession after release = (%q, true) with no state_dir, want unknown", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a broker with no state_dir wrote %d entries", len(entries))
	}
}

// TestSessionBinaryIndex_TornAndCorruptEntriesAreSkipped is the boot-safety
// case: a broker killed mid-write, plus outright garbage, must cost only the bad
// lines. A corrupt index must NEVER keep the broker from booting.
func TestSessionBinaryIndex_TornAndCorruptEntriesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionBinaryIndexName)

	good := `{"session_id":"sess-good","binary":"vision","at":"2026-01-01T00:00:00Z"}`
	// A record with no binary is as unusable as a malformed one, and is skipped for
	// the same reason: it would occupy a capped slot while answering nothing.
	noBinary := `{"session_id":"sess-nobinary","at":"2026-01-01T00:00:00Z"}`
	garbage := `{"session_id":"sess-broken",`
	// Valid JSON, but with no terminating newline: a torn write is not made
	// trustworthy by happening to parse up to the cut.
	torn := `{"session_id":"sess-torn","binary":"vision","at":"2026-01-01T00:00:00Z"}`
	content := good + "\n" + noBinary + "\n" + garbage + "\n" + torn
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seeding index: %v", err)
	}

	idx, err := openSessionBinaryIndex(testLogger(), Config{StateDir: dir})
	if err != nil {
		t.Fatalf("a corrupt index must not fail the boot, got: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	if got, ok := idx.lookup("sess-good"); !ok || got != "vision" {
		t.Errorf("lookup(sess-good) = (%q, %v), want (vision, true): the good line before the damage was lost", got, ok)
	}
	for _, id := range []string{"sess-nobinary", "sess-broken", "sess-torn"} {
		if got, ok := idx.lookup(id); ok {
			t.Errorf("lookup(%s) = (%q, true), want unknown: the bad line was accepted", id, got)
		}
	}

	// Rewrite-on-open truncated the damage away, so the running broker appends to a
	// clean file.
	recs, skipped, err := readSessionBinaryIndex(path)
	if err != nil {
		t.Fatalf("readSessionBinaryIndex after open: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d after rewrite-on-open, want 0", skipped)
	}
	if len(recs) != 1 || recs[0].SessionID != "sess-good" {
		t.Errorf("rewritten index = %+v, want exactly the one good record", recs)
	}
}

// TestSessionBinaryIndex_UnknownKeysAndAbsentFileLoadCleanly covers forward and
// backward compatibility of the file format: an index absent entirely is the
// normal first boot, and a record carrying keys this broker does not know about
// still loads on its two required fields.
func TestSessionBinaryIndex_UnknownKeysAndAbsentFileLoadCleanly(t *testing.T) {
	absent := t.TempDir()
	idx, err := openSessionBinaryIndex(testLogger(), Config{StateDir: absent})
	if err != nil {
		t.Fatalf("opening an index that does not exist yet: %v", err)
	}
	if got, ok := idx.lookup("anything"); ok {
		t.Errorf("lookup on a fresh index = (%q, true), want unknown", got)
	}
	_ = idx.Close()

	seeded := t.TempDir()
	line := `{"session_id":"sess-future","binary":"vision","at":"2026-01-01T00:00:00Z","spawned_by":"a-newer-broker"}`
	if err := os.WriteFile(filepath.Join(seeded, sessionBinaryIndexName), []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("seeding index: %v", err)
	}
	future, err := openSessionBinaryIndex(testLogger(), Config{StateDir: seeded})
	if err != nil {
		t.Fatalf("opening an index with unknown keys: %v", err)
	}
	t.Cleanup(func() { _ = future.Close() })
	if got, ok := future.lookup("sess-future"); !ok || got != "vision" {
		t.Errorf("lookup(sess-future) = (%q, %v), want (vision, true)", got, ok)
	}
}

// TestSessionBinaryIndex_EntryCapEvictsOldestBindings proves the bound. Nothing
// ever retires a binding on its own, so without the cap the file would grow one
// line per session forever; the cap trades the OLDEST bindings for a bounded
// file, and an evicted session degrades to unknown rather than to a wrong answer.
func TestSessionBinaryIndex_EntryCapEvictsOldestBindings(t *testing.T) {
	dir := t.TempDir()
	idx, err := openSessionBinaryIndex(testLogger(), Config{StateDir: dir})
	if err != nil {
		t.Fatalf("openSessionBinaryIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	// A deterministic clock, one tick per record, so "oldest" is unambiguous and the
	// test needs no sleeps.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var tick int
	idx.now = func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}

	const overflow = 10
	for i := range maxSessionBindings + overflow {
		idx.record(fmt.Sprintf("sess-%05d", i), "vision")
	}
	// The counter-driven rewrite may or may not have landed on the last record, so
	// force one: the cap is a property of the rewrite, not of every append.
	idx.mu.Lock()
	rewriteErr := idx.rewriteLocked()
	idx.mu.Unlock()
	if rewriteErr != nil {
		t.Fatalf("rewriteLocked: %v", rewriteErr)
	}

	if got, ok := idx.lookup("sess-00000"); ok {
		t.Errorf("lookup on the oldest binding = (%q, true), want evicted", got)
	}
	newest := fmt.Sprintf("sess-%05d", maxSessionBindings+overflow-1)
	if got, ok := idx.lookup(newest); !ok || got != "vision" {
		t.Errorf("lookup(%s) = (%q, %v), want (vision, true): the newest binding was evicted", newest, got, ok)
	}

	recs, skipped, err := readSessionBinaryIndex(filepath.Join(dir, sessionBinaryIndexName))
	if err != nil {
		t.Fatalf("readSessionBinaryIndex: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if len(recs) != maxSessionBindings {
		t.Errorf("index holds %d records, want the cap of %d", len(recs), maxSessionBindings)
	}
	// The fold must agree with the file, or a lookup would answer from data no
	// restart could reproduce.
	if len(idx.bindings) != maxSessionBindings {
		t.Errorf("in-memory fold holds %d bindings, want %d", len(idx.bindings), maxSessionBindings)
	}
}

// TestSessionBinaryIndex_RepeatedPairingAppendsOnce keeps the file from growing
// on churn: re-reporting the same session with the same binary — which is what
// every resume of a session does — must not cost a line each time.
func TestSessionBinaryIndex_RepeatedPairingAppendsOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionBinaryIndexName)
	idx, err := openSessionBinaryIndex(testLogger(), Config{StateDir: dir})
	if err != nil {
		t.Fatalf("openSessionBinaryIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	for range 20 {
		idx.record("sess-repeat", "vision")
	}
	recs, _, err := readSessionBinaryIndex(path)
	if err != nil {
		t.Fatalf("readSessionBinaryIndex: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("index holds %d records for one unchanged pairing, want 1", len(recs))
	}

	// A CHANGED binary is a real update and must be written, superseding the old
	// value on the next load.
	idx.record("sess-repeat", "audio")
	if got, ok := idx.lookup("sess-repeat"); !ok || got != "audio" {
		t.Errorf("lookup after a changed binding = (%q, %v), want (audio, true)", got, ok)
	}
	reopened, err := openSessionBinaryIndex(testLogger(), Config{StateDir: dir})
	if err != nil {
		t.Fatalf("reopening index: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got, ok := reopened.lookup("sess-repeat"); !ok || got != "audio" {
		t.Errorf("lookup after reopen = (%q, %v), want (audio, true)", got, ok)
	}
}

// TestSessionBinaryIndex_LiveLeaseWinsOverIndex pins the read precedence. The
// live lease is what is running on this machine right now; the index is a record
// of what ran. When they disagree the live answer is the true one.
func TestSessionBinaryIndex_LiveLeaseWinsOverIndex(t *testing.T) {
	dir := t.TempDir()
	reg, idx, _ := newIndexedRegistry(t, dir)

	// A stale binding, as if the session had previously run under another entry.
	idx.record("sess-moved", "audio")
	runSession(t, reg, "vision", "sess-moved")

	if got, ok := reg.BinaryForSession("sess-moved"); !ok || got != "vision" {
		t.Errorf("BinaryForSession = (%q, %v), want (vision, true) from the live lease", got, ok)
	}
}

// TestSessionBinaryIndex_NoSecretsInRecords asserts on the RAW FILE BYTES for the
// same reason the lease journal test does: the point is that no secret reaches
// the disk by ANY route, and a struct assertion would only re-state the fields
// the record type already chose.
func TestSessionBinaryIndex_NoSecretsInRecords(t *testing.T) {
	dir := t.TempDir()
	reg, _, path := newIndexedRegistry(t, dir)

	id := runSession(t, reg, "vision", "sess-secret")
	reg.SetSpawnSecret(id, "super-secret-value")
	reg.MarkSessionID(id, "sess-secret")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading index: %v", err)
	}
	if strings.Contains(string(raw), "super-secret-value") {
		t.Fatalf("the spawn secret reached the session binary index:\n%s", raw)
	}
	// And the record shape is exactly the three documented fields, so a field added
	// later has to be a deliberate decision rather than an accident.
	var fields map[string]json.RawMessage
	line := strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)[0]
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("decoding record: %v", err)
	}
	if len(fields) != 3 {
		t.Errorf("record carries %d fields (%v), want session_id, binary, at", len(fields), fields)
	}
}
