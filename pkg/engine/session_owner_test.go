package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
	"github.com/frankbardon/nexus/pkg/events"
)

// These tests pin the one property the owner marker exists for: a second host
// opening a session somebody else holds is *loud* — an error log and a bus
// event — and is *not refused*. Every assertion below that checks the alarm
// fired is paired with one checking the run continued, because a change that
// turned this into a lock would pass the first half alone.

// seedOwnerMarker writes an owner marker into the store as if another process
// had claimed the session. Goes through Backend.Put rather than reaching into
// the double's map, so the object lands exactly the way a real holder's would.
func seedOwnerMarker(t *testing.T, backend *objectstoretest.Memory, marker sessionOwnerMarker) {
	t.Helper()
	body, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	path := filepath.Join(t.TempDir(), sessionOwnerMarkerName)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("stage marker: %v", err)
	}
	if err := backend.Put(context.Background(), sessionOwnerMarkerKey(marker.SessionID), path); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
}

// readOwnerMarker parses the marker currently in the store.
func readOwnerMarker(t *testing.T, backend *objectstoretest.Memory, sessionID string) (sessionOwnerMarker, bool) {
	t.Helper()
	body, ok := backend.Get(sessionOwnerMarkerKey(sessionID))
	if !ok {
		return sessionOwnerMarker{}, false
	}
	var m sessionOwnerMarker
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	return m, true
}

// collectOwnerConflicts subscribes before Boot so nothing emitted during Boot
// can be missed.
func collectOwnerConflicts(eng *Engine) (*[]events.SessionOwnerConflict, *sync.Mutex) {
	var mu sync.Mutex
	got := []events.SessionOwnerConflict{}
	eng.Bus.Subscribe("session.owner.conflict", func(ev Event[any]) {
		c, ok := ev.Payload.(events.SessionOwnerConflict)
		if !ok {
			return
		}
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
	})
	return &got, &mu
}

// errUnreadableStore stands in for a transient outage on the marker read path.
var errUnreadableStore = errors.New("bucket unreachable")

// requirePIDProbe skips a test that depends on local process-liveness probing,
// which sessionOwnerPIDAlive only implements on linux and darwin.
func requirePIDProbe(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process liveness probing only implemented on linux/darwin")
	}
}

// deadPID returns a PID guaranteed not to be running. Same trick
// TestIsLockStale_DeadProcess uses: PIDs are 32-bit on the platforms that
// probe, so MaxInt32-1 is beyond the allocatable range.
func deadPID() int { return math.MaxInt32 - 1 }

// newMemoryObjectStoreEngineSharing builds a second engine on a *fresh* data
// root that shares only the object store with the first — the shape of a
// session that moved hosts.
func newMemoryObjectStoreEngineSharing(t *testing.T, backend *objectstoretest.Memory) *Engine {
	t.Helper()
	root := t.TempDir()
	name := "memory-shared-" + t.Name()
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		return backend, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })

	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	cfg.Core.ObjectStore = objectstore.Config{
		BackendName:   name,
		Bucket:        "test-bucket",
		FailurePolicy: objectstore.FailurePolicyDegrade,
	}
	return newFromConfig(cfg)
}

// The marker must be a sibling of the tree, never a member of it. If it were
// under sessions/<id> it would hydrate down onto local disk and then be
// re-uploaded by the next snapshot, so every resumed session would carry a
// fossilised claim from whichever host last ran it.
func TestOwnerMarkerIsOutsideTheSessionPrefix(t *testing.T) {
	const id = "sess-1"
	key := sessionOwnerMarkerKey(id)
	if _, under := objectstore.TrimKeyPrefix(key, sessionObjectKeyPrefix(id)); under {
		t.Errorf("%q is under the session prefix %q; it would hydrate into the tree",
			key, sessionObjectKeyPrefix(id))
	}
	if err := objectstore.ValidateKey(key); err != nil {
		t.Errorf("marker key is not a valid store-relative key: %v", err)
	}
	if err := objectstore.ValidateKeyPrefix(sessionOwnerKeyPrefix(id)); err != nil {
		t.Errorf("marker prefix is not a valid store-relative prefix: %v", err)
	}
	// The neighbouring session must not be caught by the marker prefix either —
	// the exact failure segment-boundary matching exists to prevent.
	if _, under := objectstore.TrimKeyPrefix("sessions/sess-10/files/a.md", sessionOwnerKeyPrefix(id)); under {
		t.Error("the owner prefix of sess-1 selects objects belonging to sess-10")
	}
}

// Boot writes a marker naming this process, with a heartbeat.
func TestBootClaimsTheSessionWithAnIdentifiedMarker(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	m, ok := readOwnerMarker(t, backend, eng.Session.ID)
	if !ok {
		t.Fatalf("no owner marker in the store; keys = %v", backend.Keys())
	}
	if m.SchemaVersion != sessionOwnerMarkerVersion {
		t.Errorf("_schema_version = %d, want %d", m.SchemaVersion, sessionOwnerMarkerVersion)
	}
	if m.SessionID != eng.Session.ID {
		t.Errorf("session_id = %q, want %q", m.SessionID, eng.Session.ID)
	}
	if m.KeyPrefix != sessionObjectKeyPrefix(eng.Session.ID) {
		t.Errorf("key_prefix = %q, want %q", m.KeyPrefix, sessionObjectKeyPrefix(eng.Session.ID))
	}
	if m.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", m.PID, os.Getpid())
	}
	if m.Host == "" {
		t.Error("host is empty; the marker cannot name its holder")
	}
	if m.InstanceID == "" {
		t.Error("instance_id is empty; two runs sharing a host and PID would be indistinguishable")
	}
	if m.HeartbeatAt.IsZero() || m.ClaimedAt.IsZero() {
		t.Errorf("marker carries no timestamps: claimed_at=%v heartbeat_at=%v", m.ClaimedAt, m.HeartbeatAt)
	}
}

// The headline case. A live marker from another host is detected, logged and
// raised on the bus — and the boot goes ahead anyway.
func TestSecondHostIsDetectedAndNotRefused(t *testing.T) {
	const sessionID = "sess-split-brain"
	eng, backend := newMemoryObjectStoreEngine(t, "")
	eng.RecallSessionID = sessionID

	// No session tree is seeded: an unknown session ID hydrates to nothing and
	// is minted as a fresh session, which is enough to exercise the claim. The
	// conflict is about who is *writing* the session, not what is in it.
	seedOwnerMarker(t, backend, sessionOwnerMarker{
		SchemaVersion: sessionOwnerMarkerVersion,
		SessionID:     sessionID,
		KeyPrefix:     sessionObjectKeyPrefix(sessionID),
		Host:          "other-host-that-is-not-this-one",
		PID:           4242,
		InstanceID:    "holder-instance",
		ClaimedAt:     time.Now().UTC().Add(-time.Minute),
		HeartbeatAt:   time.Now().UTC(),
	})

	conflicts, mu := collectOwnerConflicts(eng)

	// Not refused: Boot succeeds.
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot refused a conflicting session; detection must not prevent: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	mu.Lock()
	got := append([]events.SessionOwnerConflict(nil), *conflicts...)
	mu.Unlock()

	if len(got) != 1 {
		t.Fatalf("session.owner.conflict emitted %d times, want exactly 1", len(got))
	}
	c := got[0]
	if c.SchemaVersion != events.SessionOwnerConflictVersion {
		t.Errorf("_schema_version = %d, want %d", c.SchemaVersion, events.SessionOwnerConflictVersion)
	}
	if c.SessionID != sessionID {
		t.Errorf("session_id = %q, want %q", c.SessionID, sessionID)
	}
	if c.HolderHost != "other-host-that-is-not-this-one" || c.HolderPID != 4242 || c.HolderInstanceID != "holder-instance" {
		t.Errorf("holder identity not carried: %+v", c)
	}
	if c.LocalPID != os.Getpid() || c.LocalInstanceID == "" {
		t.Errorf("local identity not carried: %+v", c)
	}
	if c.LocalInstanceID == c.HolderInstanceID {
		t.Error("local and holder instance IDs are identical; the two runs are not distinguishable")
	}

	// And it really did go ahead: the session is open and this process is now
	// the recorded holder. No lock was taken, so the claim simply overwrote.
	if eng.Session == nil || eng.Session.ID != sessionID {
		t.Fatalf("session = %+v, want an open session %q", eng.Session, sessionID)
	}
	m, ok := readOwnerMarker(t, backend, sessionID)
	if !ok {
		t.Fatal("owner marker missing after a conflicting claim")
	}
	if m.InstanceID != c.LocalInstanceID {
		t.Errorf("marker instance_id = %q, want this run's %q — the claim did not overwrite",
			m.InstanceID, c.LocalInstanceID)
	}
}

// A conflict detected during hydration must reach the journal. It is raised
// after startJournal for exactly this reason: an event emitted before the
// journal's wildcard subscribes consumes a dispatch sequence the writer never
// sees, and the writer only flushes contiguous sequences — so one early emit
// empties the journal for the whole run.
func TestOwnerConflictReachesTheJournal(t *testing.T) {
	const sessionID = "sess-journal-order"
	eng, backend := newMemoryObjectStoreEngine(t, "")
	eng.RecallSessionID = sessionID

	seedOwnerMarker(t, backend, sessionOwnerMarker{
		SchemaVersion: sessionOwnerMarkerVersion,
		SessionID:     sessionID,
		Host:          "some-other-host",
		PID:           4242,
		InstanceID:    "holder-instance",
		HeartbeatAt:   time.Now().UTC(),
	})

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionDir := eng.Session.RootDir
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(sessionDir, "journal", "events.jsonl"))
	if err != nil {
		t.Fatalf("reading journal: %v", err)
	}
	// Empty is the specific symptom of emitting too early: the drain stalls on
	// a sequence the writer never received.
	if len(body) == 0 {
		t.Fatal("the journal is empty — an event was emitted before startJournal subscribed")
	}
	if !strings.Contains(string(body), "session.owner.conflict") {
		t.Errorf("the journal does not contain session.owner.conflict; it holds %d bytes", len(body))
	}
}

// The false-alarm case that matters most in practice: an ordinary resume after
// a crash on this same host. The holder's PID is gone, which is exact evidence
// — not a heuristic — that nobody is writing. It must not fire.
func TestDeadHolderOnThisHostIsNotAConflict(t *testing.T) {
	requirePIDProbe(t)

	const sessionID = "sess-crash-resume"
	eng, backend := newMemoryObjectStoreEngine(t, "")
	eng.RecallSessionID = sessionID

	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname on this platform: %v", err)
	}
	seedOwnerMarker(t, backend, sessionOwnerMarker{
		SchemaVersion: sessionOwnerMarkerVersion,
		SessionID:     sessionID,
		Host:          host,
		PID:           deadPID(),
		InstanceID:    "crashed-instance",
		// A heartbeat from one second ago: by age alone this marker looks
		// perfectly live, so only the liveness probe can save the resume.
		HeartbeatAt: time.Now().UTC().Add(-time.Second),
	})

	conflicts, mu := collectOwnerConflicts(eng)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	mu.Lock()
	n := len(*conflicts)
	mu.Unlock()
	if n != 0 {
		t.Errorf("a crash-resume on the same host raised %d conflicts; the alarm would be ignored", n)
	}
}

// The remote equivalent: a holder whose heartbeat stopped advancing long ago.
// Nothing can probe another machine's process table, so age is the only
// available signal — and it must be enough to keep a legitimate resume quiet.
func TestStaleRemoteHolderIsNotAConflict(t *testing.T) {
	const sessionID = "sess-stale-remote"
	eng, backend := newMemoryObjectStoreEngine(t, "")
	eng.RecallSessionID = sessionID

	seedOwnerMarker(t, backend, sessionOwnerMarker{
		SchemaVersion: sessionOwnerMarkerVersion,
		SessionID:     sessionID,
		Host:          "a-host-that-died",
		PID:           4242,
		InstanceID:    "long-gone",
		HeartbeatAt:   time.Now().UTC().Add(-2 * sessionOwnerStaleAfter),
	})

	conflicts, mu := collectOwnerConflicts(eng)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	mu.Lock()
	n := len(*conflicts)
	mu.Unlock()
	if n != 0 {
		t.Errorf("a stale marker raised %d conflicts; every legitimate resume would alarm", n)
	}
}

// A clean shutdown removes its own marker. Without this the broker's ordinary
// release-and-respawn cycle — stop, resume the same session minutes later —
// lands inside the staleness window and fires the alarm on the happy path.
func TestCleanShutdownRemovesTheOwnerMarker(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID
	if _, ok := readOwnerMarker(t, backend, sessionID); !ok {
		t.Fatal("no owner marker after Boot")
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := backend.Get(sessionOwnerMarkerKey(sessionID)); ok {
		t.Error("the owner marker survived a clean shutdown; every legitimate resume would alarm")
	}
	if eng.sessionOwner != nil {
		t.Error("owner state still installed after Stop")
	}
}

// Immediately resuming a cleanly-stopped session is the happy path, and it must
// be silent.
func TestResumeAfterCleanShutdownIsSilent(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID
	endTurn(t, eng, "turn-1")
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A second engine on a fresh data root, sharing only the store: the shape
	// of a session that moved hosts.
	second := newMemoryObjectStoreEngineSharing(t, backend)
	second.RecallSessionID = sessionID
	conflicts, mu := collectOwnerConflicts(second)
	if err := second.Boot(context.Background()); err != nil {
		t.Fatalf("second Boot: %v", err)
	}
	t.Cleanup(func() { _ = second.Stop(context.Background()) })

	mu.Lock()
	n := len(*conflicts)
	mu.Unlock()
	if n != 0 {
		t.Errorf("resuming a cleanly-released session raised %d conflicts", n)
	}
}

// The heartbeat is what makes "stale" distinguishable from "active". Without it
// a marker only ever records when a session was claimed, and an hour-old claim
// is indistinguishable from an hour-old crash.
func TestHeartbeatAdvancesTheStoredMarker(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	first, ok := readOwnerMarker(t, backend, eng.Session.ID)
	if !ok {
		t.Fatal("no owner marker after Boot")
	}

	// Backdate the in-memory marker past the staleness threshold, then beat
	// once. A run whose heartbeat works reads as live again afterwards; one
	// whose heartbeat does nothing stays stale forever.
	owner := eng.sessionOwner
	if owner == nil {
		t.Fatal("no owner state installed")
	}
	stale := owner.marker
	stale.HeartbeatAt = time.Now().UTC().Add(-2 * sessionOwnerStaleAfter)
	other := sessionOwnerMarker{Host: "someone-else", PID: 4242, InstanceID: "someone-else"}
	if _, live := sessionOwnerStillLive(stale, other, time.Now().UTC()); live {
		t.Fatal("a marker backdated past the staleness threshold still reads as live")
	}

	eng.beatSessionOwner(eng.objectStore, owner)

	second, ok := readOwnerMarker(t, backend, eng.Session.ID)
	if !ok {
		t.Fatal("owner marker missing after a heartbeat")
	}
	if !second.HeartbeatAt.After(first.HeartbeatAt) {
		t.Errorf("heartbeat_at did not advance: %v -> %v", first.HeartbeatAt, second.HeartbeatAt)
	}
	if second.InstanceID != first.InstanceID || second.ClaimedAt != first.ClaimedAt {
		t.Error("a heartbeat changed the identity or the claim time; it must only refresh the timestamp")
	}
	if _, live := sessionOwnerStillLive(second, other, time.Now().UTC()); !live {
		t.Error("a freshly beaten marker does not read as live; stale and active are indistinguishable")
	}
}

// The staleness rules on their own, including the two that make the alarm
// credible and the one that keeps it useful.
func TestSessionOwnerStillLive(t *testing.T) {
	requirePIDProbe(t)

	now := time.Now().UTC()
	self := sessionOwnerMarker{Host: "host-a", PID: os.Getpid(), InstanceID: "self"}

	tests := []struct {
		name     string
		existing sessionOwnerMarker
		wantLive bool
	}{
		{
			name:     "our own marker",
			existing: sessionOwnerMarker{Host: "host-a", PID: os.Getpid(), InstanceID: "self", HeartbeatAt: now},
			wantLive: false,
		},
		{
			name:     "another live host",
			existing: sessionOwnerMarker{Host: "host-b", PID: 4242, InstanceID: "other", HeartbeatAt: now},
			wantLive: true,
		},
		{
			name:     "another host, heartbeat long stopped",
			existing: sessionOwnerMarker{Host: "host-b", PID: 4242, InstanceID: "other", HeartbeatAt: now.Add(-2 * sessionOwnerStaleAfter)},
			wantLive: false,
		},
		{
			name:     "another host, heartbeat inside the window",
			existing: sessionOwnerMarker{Host: "host-b", PID: 4242, InstanceID: "other", HeartbeatAt: now.Add(-sessionOwnerStaleAfter / 2)},
			wantLive: true,
		},
		{
			name:     "same host, dead PID, fresh heartbeat",
			existing: sessionOwnerMarker{Host: "host-a", PID: deadPID(), InstanceID: "crashed", HeartbeatAt: now},
			wantLive: false,
		},
		{
			name:     "same host, live PID",
			existing: sessionOwnerMarker{Host: "host-a", PID: os.Getpid(), InstanceID: "other", HeartbeatAt: now},
			wantLive: true,
		},
		{
			name:     "no heartbeat at all",
			existing: sessionOwnerMarker{Host: "host-b", PID: 4242, InstanceID: "other"},
			wantLive: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, live := sessionOwnerStillLive(tc.existing, self, now)
			if live != tc.wantLive {
				t.Errorf("live = %v, want %v", live, tc.wantLive)
			}
		})
	}
}

// A store that cannot be read is not evidence of a conflict. Treating it as one
// would fire the alarm on every transient outage.
func TestUnreadableMarkerDoesNotAlarmOrFailTheBoot(t *testing.T) {
	const sessionID = "sess-unreadable"
	backend := &scriptedBackend{}
	eng, _ := newObjectStoreEngine(t, backend)
	eng.RecallSessionID = sessionID
	backend.hydrate = func(keyPrefix string, destDir string) error {
		if keyPrefix == sessionOwnerKeyPrefix(sessionID) {
			return errUnreadableStore
		}
		writeSessionTree(t, destDir, sessionID, nil)
		return nil
	}

	conflicts, mu := collectOwnerConflicts(eng)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("a failed marker read must not fail the boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	mu.Lock()
	n := len(*conflicts)
	mu.Unlock()
	if n != 0 {
		t.Errorf("an unreadable marker raised %d conflicts", n)
	}
}

// Zero impact by default: with no backend named there is no marker, no
// goroutine and no scratch directory.
func TestNoObjectStoreClaimsNoOwnership(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)

	conflicts, mu := collectOwnerConflicts(eng)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if eng.sessionOwner != nil {
		t.Error("owner state installed with no object-store backend configured")
	}
	if eng.ownerConflict != nil {
		t.Error("a conflict was recorded with no object-store backend configured")
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	mu.Lock()
	n := len(*conflicts)
	mu.Unlock()
	if n != 0 {
		t.Errorf("%d conflicts emitted on the default path", n)
	}
}
