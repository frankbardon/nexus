package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// discardLogger is a logger that writes nowhere, for tests that assert on
// behaviour rather than on log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startSleeper launches a long-lived child process and returns its pid, killing
// it on cleanup. It stands in for a nexus instance that survived its broker: the
// only property recovery reads off it is that its pid is alive.
func startSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	// Reap it the instant it dies. In production an adopted instance is NOT the
	// broker's child — it was re-parented to init when the previous broker exited,
	// and init reaps it — so a signal-0 probe stops answering as soon as it is
	// gone. Here it IS this process's child, and an unreaped child lingers as a
	// zombie that signal 0 still reports as alive. Without this the test would be
	// measuring a fixture artefact rather than the behaviour.
	waited := make(chan struct{})
	go func() { defer close(waited); _ = cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	return cmd.Process.Pid
}

// deadPID returns a pid that is certainly not alive: a child process that has
// been run to completion AND reaped, so it is not even a zombie (signal 0 to a
// zombie succeeds, which would make this test lie).
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run short-lived process: %v", err)
	}
	pid := cmd.Process.Pid
	if processAlive(pid) {
		t.Skipf("pid %d was recycled immediately; the liveness assertion cannot be made deterministic here", pid)
	}
	return pid
}

// seedJournal writes records into a fresh state_dir's journal and returns the
// directory. It writes the file DIRECTLY rather than through fileLeaseStore so a
// test can express a pre-restart state (including a torn or corrupt one) that the
// live write path would never produce.
func seedJournal(t *testing.T, recs ...LeaseRecord) string {
	t.Helper()
	dir := t.TempDir()
	var buf []byte
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, leaseJournalName), buf, 0o600); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	return dir
}

// openRecoveryFixture opens a store over dir and wires it to a fresh registry
// exactly as run() does, returning both plus the derived spawn key.
func openRecoveryFixture(t *testing.T, dir, brokerID string, maxConcurrent int) (*Registry, LeaseStore, spawnKey) {
	t.Helper()
	logger := discardLogger()
	store, err := openFileLeaseStore(logger, dir)
	if err != nil {
		t.Fatalf("open lease store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key, err := loadSpawnKey(logger, dir)
	if err != nil {
		t.Fatalf("load spawn key: %v", err)
	}
	reg := NewRegistry(logger, maxConcurrent)
	reg.useLeaseStore(store, brokerID, "")
	return reg, store, key
}

// liveRecord builds a `lease-created` record for a lease that was live when the
// broker stopped.
func liveRecord(leaseID, brokerID string, pid int) LeaseRecord {
	return LeaseRecord{
		Kind:      leaseRecordCreated,
		LeaseID:   leaseID,
		Owner:     LeaseOwnerRecord{ID: "alice", Tenant: "acme", Scopes: []string{"broker.claim"}},
		SessionID: "sess-" + leaseID,
		PID:       pid,
		BrokerID:  brokerID,
		CreatedAt: time.Now().Add(-time.Hour).UTC(),
	}
}

// TestRecoverRestoresLiveLease is the core of restart recovery: a journal record
// whose pid is still alive comes back as a lease, with its original owner and
// session id, holding a capacity slot.
func TestRecoverRestoresLiveLease(t *testing.T) {
	pid := startSleeper(t)
	dir := seedJournal(t, liveRecord("lease-live", "broker-a", pid))
	reg, _, key := openRecoveryFixture(t, dir, "broker-a", 8)

	restored := recoverLeases(discardLogger(), reg, reg.store, "broker-a", key)
	if len(restored) != 1 || restored[0] != "lease-live" {
		t.Fatalf("restored = %v, want [lease-live]", restored)
	}
	if !reg.Has("lease-live") {
		t.Fatal("restored lease is not in the registry")
	}

	// The owner survives the restart, so E2's ownership checks still work.
	owner, ok := reg.LeaseOwner("lease-live")
	if !ok {
		t.Fatal("restored lease reports no owner")
	}
	if owner.ID != "alice" || owner.Tenant != "acme" {
		t.Errorf("restored owner = %+v, want id=alice tenant=acme", owner)
	}
	if !ownsLease(reg, "lease-live", nexusauth.Principal{ID: "alice"}) {
		t.Error("the recorded principal does not own its own restored lease")
	}
	if ownsLease(reg, "lease-live", nexusauth.Principal{ID: "mallory"}) {
		t.Error("a stranger owns a restored lease")
	}

	// The slot is re-held, so max_concurrent cannot be over-admitted.
	if got := reg.SlotsInUse(); got != 1 {
		t.Errorf("slots_in_use = %d, want 1 (the restored lease re-holds its slot)", got)
	}
	if got := reg.SessionID("lease-live"); got != "sess-lease-live" {
		t.Errorf("restored session_id = %q, want sess-lease-live", got)
	}
	if got := reg.PID("lease-live"); got != pid {
		t.Errorf("restored pid = %d, want %d", got, pid)
	}

	// It reads as `spawning` on GET /leases until an instance reattaches, which is
	// exactly what it is: a lease with no live instance connection.
	snap := reg.Snapshot()
	if len(snap.Leases) != 1 || snap.Leases[0].State != surfaceStateSpawning {
		t.Errorf("restored lease snapshot = %+v, want one lease in state %q", snap.Leases, surfaceStateSpawning)
	}
}

// TestRecoverDropsDeadPID proves a record whose process is gone is dropped from
// BOTH the restored set and persisted state, so the next boot does not reconsider
// it.
func TestRecoverDropsDeadPID(t *testing.T) {
	dead := deadPID(t)
	dir := seedJournal(t, liveRecord("lease-dead", "broker-a", dead))
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)

	if restored := recoverLeases(discardLogger(), reg, store, "broker-a", key); len(restored) != 0 {
		t.Fatalf("restored = %v, want nothing (the pid is dead)", restored)
	}
	if reg.Has("lease-dead") {
		t.Error("a lease with a dead pid was restored")
	}
	if got := reg.SlotsInUse(); got != 0 {
		t.Errorf("slots_in_use = %d, want 0", got)
	}

	// Dropped from persisted state too: the store's live view no longer holds it.
	live, err := store.Live()
	if err != nil {
		t.Fatalf("store.Live: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("store still reports %d live leases after the dead one was dropped: %+v", len(live), live)
	}
}

// TestRecoverDropsLeaseWithNoProcess covers the broker dying between minting a
// lease and exec'ing its instance: there is no pid, so there is nothing to
// reattach and nothing to kill.
func TestRecoverDropsLeaseWithNoProcess(t *testing.T) {
	dir := seedJournal(t, liveRecord("lease-unspawned", "broker-a", 0))
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)

	if restored := recoverLeases(discardLogger(), reg, store, "broker-a", key); len(restored) != 0 {
		t.Fatalf("restored = %v, want nothing (the lease never had a process)", restored)
	}
	live, err := store.Live()
	if err != nil {
		t.Fatalf("store.Live: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("store still reports the unspawned lease as live: %+v", live)
	}
}

// TestRecoverSkipsAnotherBrokersRecords proves recovery neither adopts nor kills
// a lease it cannot identify as its own. state_dir is per-broker by construction,
// so this only arises when broker_id changes under a directory — and killing a pid
// recorded by an unknown broker would be worse than leaving it.
func TestRecoverSkipsAnotherBrokersRecords(t *testing.T) {
	pid := startSleeper(t)
	dir := seedJournal(t, liveRecord("lease-foreign", "broker-b", pid))
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)

	if restored := recoverLeases(discardLogger(), reg, store, "broker-a", key); len(restored) != 0 {
		t.Fatalf("restored = %v, want nothing (the record belongs to broker-b)", restored)
	}
	if reg.Has("lease-foreign") {
		t.Error("another broker's lease was adopted")
	}
	if !processAlive(pid) {
		t.Error("another broker's instance was killed")
	}
	// It is left in the journal rather than closed out: we have no standing to
	// declare another broker's lease released.
	live, err := store.Live()
	if err != nil {
		t.Fatalf("store.Live: %v", err)
	}
	if len(live) != 1 {
		t.Errorf("another broker's record was removed from the journal: %+v", live)
	}
}

// TestRecoverCorruptJournalColdStarts proves a journal the broker cannot make
// sense of is a clean cold start with a warning, never a boot failure.
func TestRecoverCorruptJournalColdStarts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, leaseJournalName),
		[]byte("this is not json\n{\"also\":\"not a record\"}\n\x00\x01\x02\n"), 0o600); err != nil {
		t.Fatalf("seed corrupt journal: %v", err)
	}

	// Opening must succeed — the store skips unparseable lines rather than
	// failing the boot.
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)

	restored := recoverLeases(discardLogger(), reg, store, "broker-a", key)
	if len(restored) != 0 {
		t.Fatalf("restored = %v, want nothing from a corrupt journal", restored)
	}
	if got := reg.SlotsInUse(); got != 0 {
		t.Errorf("slots_in_use = %d, want 0 after a cold start", got)
	}
	// And the broker is fully usable: a fresh claim still works.
	if _, err := reg.NewLease(anonymousOwner()); err != nil {
		t.Errorf("minting a lease after a corrupt-journal cold start: %v", err)
	}
}

// TestRecoverWithNoStoreIsNoop proves the state_dir-unset path: with no store
// there is nothing to replay, and boot behaves exactly as it did before recovery
// existed.
func TestRecoverWithNoStoreIsNoop(t *testing.T) {
	reg := NewRegistry(discardLogger(), 8)
	if restored := recoverLeases(discardLogger(), reg, nil, "", nil); restored != nil {
		t.Fatalf("restored = %v, want nil with no store", restored)
	}
	if got := reg.SlotsInUse(); got != 0 {
		t.Errorf("slots_in_use = %d, want 0", got)
	}
}

// TestRestoredLeaseRequiresSpawnSecret is the identity gate: a live pid is not
// proof of identity (it may have been reused), so a restored lease stays inactive
// until something presents the secret this broker DERIVES for it, rather than
// merely naming its lease id. Every registration clears that bar now, but a
// restored lease is where the reasoning is sharpest, so it keeps its own test.
func TestRestoredLeaseRequiresSpawnSecret(t *testing.T) {
	pid := startSleeper(t)
	dir := seedJournal(t, liveRecord("lease-live", "broker-a", pid))
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)
	if len(recoverLeases(discardLogger(), reg, store, "broker-a", key)) != 1 {
		t.Fatal("setup: the lease was not restored")
	}

	// This broker has no `auth:` block; the secret is required regardless.
	if err := reg.AttachInstance("lease-live", newWSConn(nil), ""); err == nil {
		t.Fatal("a register frame with NO secret attached to a restored lease")
	}
	if err := reg.AttachInstance("lease-live", newWSConn(nil), "not-the-secret"); err == nil {
		t.Fatal("a register frame with the WRONG secret attached to a restored lease")
	}
	// Still inactive after both refusals.
	if reg.InstanceConn("lease-live") != nil {
		t.Fatal("a refused registration left an instance connection bound")
	}
	snap := reg.Snapshot()
	if snap.Leases[0].State != surfaceStateSpawning {
		t.Errorf("restored lease state = %q after refused registrations, want %q",
			snap.Leases[0].State, surfaceStateSpawning)
	}

	// The genuine instance holds the secret this broker derives for the lease.
	secret, err := key.secretFor("lease-live")
	if err != nil {
		t.Fatalf("derive secret: %v", err)
	}
	if err := reg.AttachInstance("lease-live", newWSConn(nil), secret); err != nil {
		t.Fatalf("the correct secret was refused: %v", err)
	}
	if reg.InstanceConn("lease-live") == nil {
		t.Fatal("the restored lease has no instance connection after a valid register")
	}
}

// TestReapUnreattachedRestoredLease proves the bounded window: a restored lease
// nothing reconnects to is torn down through the shared release path — its
// process killed, its slot freed, its record closed out.
func TestReapUnreattachedRestoredLease(t *testing.T) {
	pid := startSleeper(t)
	dir := seedJournal(t, liveRecord("lease-stale", "broker-a", pid))
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)
	restored := recoverLeases(discardLogger(), reg, store, "broker-a", key)
	if len(restored) != 1 {
		t.Fatalf("setup: restored = %v", restored)
	}
	if reg.SlotsInUse() != 1 {
		t.Fatalf("setup: slots_in_use = %d, want 1", reg.SlotsInUse())
	}

	reapUnreattached(context.Background(), discardLogger(), reg, restored, 20*time.Millisecond, 20*time.Millisecond)

	// The release runs in its own goroutine (as the idle sweeper's do), so wait
	// for the observable outcome rather than assuming it is already done.
	waitForState(t, 5*time.Second, func() bool { return !reg.Has("lease-stale") },
		"the unreattached lease was never removed")
	if got := reg.SlotsInUse(); got != 0 {
		t.Errorf("slots_in_use = %d after reaping, want 0", got)
	}
	waitForState(t, 5*time.Second, func() bool { return !processAlive(pid) },
		"the unreattached lease's process was never killed")

	// The record is closed out too. Polled rather than read once: Remove journals
	// the release LAST — after the lock is dropped and both peers are closed — so
	// the lease leaves the registry map fractionally before the journal catches up.
	waitForState(t, 5*time.Second, func() bool {
		live, err := store.Live()
		return err == nil && len(live) == 0
	}, "the reaped lease is still recorded as live in the journal")
}

// TestReapSkipsReattachedLease proves the window only reaps leases nobody came
// back for: one that registered is left running.
func TestReapSkipsReattachedLease(t *testing.T) {
	pid := startSleeper(t)
	dir := seedJournal(t, liveRecord("lease-back", "broker-a", pid))
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)
	restored := recoverLeases(discardLogger(), reg, store, "broker-a", key)
	if len(restored) != 1 {
		t.Fatalf("setup: restored = %v", restored)
	}

	secret, err := key.secretFor("lease-back")
	if err != nil {
		t.Fatalf("derive secret: %v", err)
	}
	if err := reg.AttachInstance("lease-back", newWSConn(nil), secret); err != nil {
		t.Fatalf("reattach: %v", err)
	}

	reapUnreattached(context.Background(), discardLogger(), reg, restored, 20*time.Millisecond, 20*time.Millisecond)
	// Give any (incorrect) release goroutine time to land before asserting it did
	// not happen.
	time.Sleep(200 * time.Millisecond)

	if !reg.Has("lease-back") {
		t.Fatal("a reattached lease was reaped")
	}
	if !processAlive(pid) {
		t.Error("a reattached lease's instance was killed")
	}
}

// TestReapUnreattachedStopsOnContextCancel proves shutdown does not turn into a
// mass reap: cancelling the context while the window is still open abandons the
// wait and leaves the leases alone.
func TestReapUnreattachedStopsOnContextCancel(t *testing.T) {
	pid := startSleeper(t)
	dir := seedJournal(t, liveRecord("lease-live", "broker-a", pid))
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)
	restored := recoverLeases(discardLogger(), reg, store, "broker-a", key)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reapUnreattached(ctx, discardLogger(), reg, restored, time.Hour, time.Second)

	if !reg.Has("lease-live") {
		t.Error("a restored lease was reaped after the reap wait was cancelled")
	}
}

// TestRestoredLeaseIsIdleSwept proves a restored lease participates in idle
// sweeping normally once it is back: it is an ordinary lease with an ordinary
// last-activity stamp.
func TestRestoredLeaseIsIdleSwept(t *testing.T) {
	pid := startSleeper(t)
	dir := seedJournal(t, liveRecord("lease-idle", "broker-a", pid))
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)
	if len(recoverLeases(discardLogger(), reg, store, "broker-a", key)) != 1 {
		t.Fatal("setup: the lease was not restored")
	}

	// A restored lease is stamped active AT RESTORE, not at its (hour-old)
	// creation time — otherwise the sweeper would reap a healthy session instantly.
	if ids := reg.idleLeases(time.Now().Add(-time.Minute)); len(ids) != 0 {
		t.Fatalf("a just-restored lease is already idle: %v", ids)
	}
	if ids := reg.idleLeases(time.Now().Add(time.Minute)); len(ids) != 1 || ids[0] != "lease-idle" {
		t.Fatalf("idleLeases = %v, want [lease-idle] past the cutoff", ids)
	}
}

// TestRestoredLeaseCrashWatchFreesSlot proves crash watching is armed from the
// moment a lease is restored: an adopted instance that dies before anything
// reconnects still frees its slot and closes its record out.
func TestRestoredLeaseCrashWatchFreesSlot(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid := cmd.Process.Pid
	dir := seedJournal(t, liveRecord("lease-crash", "broker-a", pid))
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 8)
	if len(recoverLeases(discardLogger(), reg, store, "broker-a", key)) != 1 {
		t.Fatal("setup: the lease was not restored")
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	waitForState(t, 10*time.Second, func() bool { return !reg.Has("lease-crash") },
		"a restored lease whose instance died was never removed")
	if got := reg.SlotsInUse(); got != 0 {
		t.Errorf("slots_in_use = %d after the adopted instance died, want 0", got)
	}
}

// TestRestoreOverCapacityStillHoldsSlots proves the cap stays honest when an
// operator lowers max_concurrent between restarts: the running instances are
// counted (going briefly over the cap) rather than hidden, so no fresh claim is
// admitted on top of them.
func TestRestoreOverCapacityStillHoldsSlots(t *testing.T) {
	pidA := startSleeper(t)
	pidB := startSleeper(t)
	dir := seedJournal(t,
		liveRecord("lease-a", "broker-a", pidA),
		liveRecord("lease-b", "broker-a", pidB),
	)
	// max_concurrent lowered to 1 while two instances are running.
	reg, store, key := openRecoveryFixture(t, dir, "broker-a", 1)

	if restored := recoverLeases(discardLogger(), reg, store, "broker-a", key); len(restored) != 2 {
		t.Fatalf("restored = %v, want both leases", restored)
	}
	if got := reg.SlotsInUse(); got != 2 {
		t.Fatalf("slots_in_use = %d, want 2 (both running instances counted)", got)
	}
	// And the broker refuses to over-admit on top of them.
	if _, err := reg.NewLease(anonymousOwner()); err == nil {
		t.Error("a fresh claim was admitted while restored leases already exceed max_concurrent")
	}
}

// TestSpawnKeyDerivesStableSecretAcrossRestarts is the property the whole
// reattach design rests on: the secret is reproducible from state_dir alone, and
// never written anywhere.
func TestSpawnKeyDerivesStableSecretAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := loadSpawnKey(discardLogger(), dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := loadSpawnKey(discardLogger(), dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	a, err := first.secretFor("lease-1")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	b, err := second.secretFor("lease-1")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if a != b {
		t.Errorf("the same key derived different secrets across a reload: %q vs %q", a, b)
	}
	if a == "" {
		t.Error("derived an empty secret")
	}

	// Different leases get different secrets, so one lease's secret is useless
	// against another.
	other, err := second.secretFor("lease-2")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if other == a {
		t.Error("two lease ids derived the same secret")
	}

	// A different state_dir is a different key.
	third, err := loadSpawnKey(discardLogger(), t.TempDir())
	if err != nil {
		t.Fatalf("third load: %v", err)
	}
	elsewhere, err := third.secretFor("lease-1")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if elsewhere == a {
		t.Error("two independent state_dirs derived the same secret")
	}

	// The key file exists, is owner-only, and holds no lease's secret.
	info, err := os.Stat(filepath.Join(dir, spawnKeyFileName))
	if err != nil {
		t.Fatalf("stat spawn key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("spawn key mode = %v, want 0600", mode)
	}
	data, err := os.ReadFile(filepath.Join(dir, spawnKeyFileName))
	if err != nil {
		t.Fatalf("read spawn key: %v", err)
	}
	if string(data) == a+"\n" {
		t.Error("the key file contains a lease's spawn secret verbatim")
	}
}

// TestSpawnKeyAbsentIsRandomPerSpawn proves the no-state_dir path is unchanged:
// with no key, every spawn gets a fresh random secret exactly as it did before
// derivation existed.
func TestSpawnKeyAbsentIsRandomPerSpawn(t *testing.T) {
	key, err := loadSpawnKey(discardLogger(), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if key != nil {
		t.Fatalf("loadSpawnKey(\"\") = %v, want nil", key)
	}
	a, err := key.secretFor("lease-1")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	b, err := key.secretFor("lease-1")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if a == b {
		t.Error("an absent key produced a reproducible secret; it must be random per spawn")
	}
}

// TestSpawnKeyCorruptFileIsRegenerated proves a lost or damaged key degrades
// rather than failing the boot: a new key is written, and the (now underivable)
// leases will simply be reaped instead of reattached.
func TestSpawnKeyCorruptFileIsRegenerated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, spawnKeyFileName), []byte("not hex at all\n"), 0o600); err != nil {
		t.Fatalf("seed corrupt key: %v", err)
	}
	key, err := loadSpawnKey(discardLogger(), dir)
	if err != nil {
		t.Fatalf("loadSpawnKey over a corrupt file failed the boot: %v", err)
	}
	if len(key) != spawnKeyBytes {
		t.Fatalf("regenerated key length = %d, want %d", len(key), spawnKeyBytes)
	}
	// And it is now stable.
	again, err := loadSpawnKey(discardLogger(), dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	first, _ := key.secretFor("lease-1")
	second, _ := again.secretFor("lease-1")
	if first != second {
		t.Error("the regenerated key is not stable across a reload")
	}
}

// TestProcessAliveDistinguishesLiveFromDead pins the liveness probe itself, which
// is the input every drop/restore decision is made on.
func TestProcessAliveDistinguishesLiveFromDead(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("processAlive says this very process is dead")
	}
	if processAlive(0) {
		t.Error("processAlive accepted pid 0")
	}
	if processAlive(-1) {
		t.Error("processAlive accepted a negative pid")
	}
	if processAlive(deadPID(t)) {
		t.Error("processAlive says a reaped process is alive")
	}
}

// waitForState polls cond until it holds or the timeout elapses, failing with msg.
// Recovery hands work to goroutines (the reaper, the crash watcher), so the
// assertions are on eventual state rather than on immediate return.
func waitForState(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v: %s", timeout, msg)
}
