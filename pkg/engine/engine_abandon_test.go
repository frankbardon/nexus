package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/events"
)

// Abandon exists because "stop the workers" and "record a clean exit" were the
// same operation, and they are not the same thing. A host whose compute is
// being reclaimed, and a test simulating a process death, both need the first
// without the second. Every test below is written against that split: the
// paired assertion is always "the workers are gone" AND "nothing was
// persisted", because an implementation that quietly called Stop would pass
// either half alone.

// blobKeyPrefix is where write-through and the snapshot both put one blob.
func blobKeyPrefix(sessionID, sha string) string {
	return "sessions/" + sessionID + "/blobs/" + sha[:2] + "/" + sha
}

// The headline for the owner marker: the beat stops, the marker stays. Stop
// deletes it — TestCleanShutdownRemovesTheOwnerMarker pins that — and the
// difference between the two is the only evidence the next host has that this
// session was dropped rather than released.
func TestAbandonStopsTheHeartbeatAndKeepsTheOwnerMarker(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID

	claimed, ok := readOwnerMarker(t, backend, sessionID)
	if !ok {
		t.Fatal("no owner marker after Boot")
	}
	owner := eng.sessionOwner
	if owner == nil {
		t.Fatal("no owner state installed after Boot")
	}

	eng.Abandon()

	// The goroutine, not just the bookkeeping. Abandon joins it before
	// returning, so this is a closed-channel check rather than a poll: if it
	// were still running the marker would keep looking live for ever and a
	// genuine takeover would be reported as a split brain.
	select {
	case <-owner.done:
	default:
		t.Error("the owner heartbeat goroutine is still running after Abandon")
	}
	if eng.sessionOwner != nil {
		t.Error("owner state still installed after Abandon")
	}

	left, ok := readOwnerMarker(t, backend, sessionID)
	if !ok {
		t.Fatal("Abandon deleted the owner marker; the next host would read a clean release " +
			"where the session was actually dropped")
	}
	if left.InstanceID != claimed.InstanceID {
		t.Errorf("the marker left behind belongs to instance %q, want the abandoned run's %q",
			left.InstanceID, claimed.InstanceID)
	}
	if left.HeartbeatAt != claimed.HeartbeatAt {
		t.Errorf("heartbeat_at moved during Abandon: %v -> %v", claimed.HeartbeatAt, left.HeartbeatAt)
	}
}

// The other half: the marker Abandon leaves must be readable by the next host
// as what it is — a holder that stopped — and not as a live conflict.
//
// Both of E3-S3's staleness rules are exercised, because Abandon relies on
// whichever one the deployment happens to hit: a container restarted in place
// leaves a dead PID on the same host, a session that moved leaves a heartbeat
// that stopped advancing. The abandoned marker is re-seeded with the fact the
// test cannot wait for — a dead process, or a five-minute-old beat — and
// nothing else about it is touched. That is legitimate precisely because
// Abandon guarantees the beat stopped: without that guarantee the marker would
// keep refreshing itself and neither rule could ever fire.
func TestAnAbandonedMarkerLetsTheNextHostTakeOverInSilence(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname on this platform: %v", err)
	}

	cases := []struct {
		name  string
		local bool
		age   func(sessionOwnerMarker) sessionOwnerMarker
	}{
		{
			// A crashed container restarted in place. The beat is seconds old,
			// so only the liveness probe can keep this quiet.
			name:  "same host, dead pid",
			local: true,
			age: func(m sessionOwnerMarker) sessionOwnerMarker {
				m.Host = host
				m.PID = deadPID()
				m.HeartbeatAt = time.Now().UTC().Add(-time.Second)
				return m
			},
		},
		{
			// The session moved. Nothing can probe another machine's process
			// table, so the stopped heartbeat is the only signal there is.
			name: "another host, stale heartbeat",
			age: func(m sessionOwnerMarker) sessionOwnerMarker {
				m.Host = "the-host-that-was-reclaimed"
				m.PID = 4242
				m.HeartbeatAt = time.Now().UTC().Add(-2 * sessionOwnerStaleAfter)
				return m
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.local {
				requirePIDProbe(t)
			}
			eng, backend := newMemoryObjectStoreEngine(t, "")
			if err := eng.Boot(context.Background()); err != nil {
				t.Fatalf("Boot: %v", err)
			}
			sessionID := eng.Session.ID
			endTurn(t, eng, "turn-1")
			eng.Abandon()

			abandoned, ok := readOwnerMarker(t, backend, sessionID)
			if !ok {
				t.Fatal("Abandon left no marker for the next host to judge")
			}
			seedOwnerMarker(t, backend, tc.age(abandoned))

			// A fresh data root sharing only the store: the shape of a session
			// picked up somewhere else.
			next := newMemoryObjectStoreEngineSharing(t, backend)
			next.RecallSessionID = sessionID
			conflicts, mu := collectOwnerConflicts(next)
			if err := next.Boot(context.Background()); err != nil {
				t.Fatalf("Boot resuming %q: %v", sessionID, err)
			}
			t.Cleanup(func() { _ = next.Stop(context.Background()) })

			mu.Lock()
			n := len(*conflicts)
			mu.Unlock()
			if n != 0 {
				t.Errorf("taking over an abandoned session raised %d conflicts; "+
					"an alarm on the ordinary reclaim path is an alarm nobody reads", n)
			}
			if next.Session == nil || next.Session.ID != sessionID {
				t.Fatalf("resumed session = %+v, want ID %q", next.Session, sessionID)
			}
		})
	}
}

// The negative that gives Abandon its name: no snapshot, no flush, no
// ownership release, and every local writer left where it was.
func TestAbandonTakesNoShutdownSnapshotAndLeavesLocalStateAlone(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")

	// Subscribed outside runUnsubs, so Abandon's unsubscribe pass cannot hide
	// a snapshot from this counter.
	var results []events.SessionSnapshotResult
	eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
			results = append(results, r)
		}
	})
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID

	endTurn(t, eng, "turn-1")
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("the turn boundary produced %+v, want exactly one successful snapshot", results)
	}

	// Written after the boundary, so the only thing that could ever put it in
	// the store is a shutdown snapshot.
	const afterTheTurn = "files/after-the-turn.md"
	if err := eng.Session.WriteFile(afterTheTurn, []byte("never asked to be stored")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	flushesBefore := backend.Counts().Flushes

	eng.Abandon()

	if len(results) != 1 {
		t.Errorf("Abandon produced %d snapshots in total, want the turn boundary's 1: %+v",
			len(results), results)
	}
	if _, ok := backend.Get("sessions/" + sessionID + "/" + afterTheTurn); ok {
		t.Errorf("%q reached the store; Abandon must take no shutdown snapshot", afterTheTurn)
	}
	if got := backend.Counts().Flushes; got != flushesBefore {
		t.Errorf("flushes went %d -> %d across Abandon, want no flush at all", flushesBefore, got)
	}

	// The local tree is what a killed process leaves: journal open, lock held,
	// storage handles live, session metadata not finalized.
	if eng.Journal == nil {
		t.Error("Abandon closed the journal; a killed process does not")
	}
	if eng.Storage == nil {
		t.Error("Abandon closed per-plugin storage; a killed process does not")
	}
	if _, err := os.Stat(filepath.Join(eng.Session.RootDir, sessionLockFilename)); err != nil {
		t.Errorf("the session lock is gone after Abandon (%v); removing it is a clean exit's job", err)
	}
	meta, err := eng.Session.SessionMetadata()
	if err != nil {
		t.Fatalf("reading session metadata: %v", err)
	}
	if meta.EndedAt != nil {
		t.Error("Abandon finalized the session metadata; a killed process does not")
	}
}

// Abandon must survive being called twice, called after Stop, and called on an
// engine that never booted — a teardown helper that panics on the second call
// is a teardown helper nobody can put in a defer.
func TestAbandonIsIdempotent(t *testing.T) {
	t.Run("twice", func(t *testing.T) {
		eng, backend := newMemoryObjectStoreEngine(t, "")
		if err := eng.Boot(context.Background()); err != nil {
			t.Fatalf("Boot: %v", err)
		}
		sessionID := eng.Session.ID
		eng.Abandon()
		eng.Abandon()
		if _, ok := readOwnerMarker(t, backend, sessionID); !ok {
			t.Error("the second Abandon removed the marker the first one preserved")
		}
	})

	t.Run("after Stop", func(t *testing.T) {
		eng, _ := newMemoryObjectStoreEngine(t, "")
		if err := eng.Boot(context.Background()); err != nil {
			t.Fatalf("Boot: %v", err)
		}
		if err := eng.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		eng.Abandon()
	})

	t.Run("never booted", func(t *testing.T) {
		eng, _ := newMemoryObjectStoreEngine(t, "")
		eng.Abandon()
	})

	t.Run("no object store configured", func(t *testing.T) {
		cfg := DefaultConfig()
		root := t.TempDir()
		cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
		cfg.Core.Storage.Root = root
		eng := newFromConfig(cfg)
		if err := eng.Boot(context.Background()); err != nil {
			t.Fatalf("Boot: %v", err)
		}
		t.Cleanup(func() { _ = eng.Stop(context.Background()) })
		eng.Abandon()
		if eng.objectStore != nil {
			t.Error("a plain engine grew an object store")
		}
	})
}

// Abandon then Stop is the composition a long-lived host wants: drop the
// session's remote state, then release the local handles. Stop must degenerate
// to local teardown — no snapshot, and above all no marker delete, because a
// delete here would forge the clean release Abandon deliberately withheld.
func TestStopAfterAbandonWritesNothingRemote(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")

	var results []events.SessionSnapshotResult
	eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
			results = append(results, r)
		}
	})
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID
	endTurn(t, eng, "turn-1")

	eng.Abandon()
	keysAfterAbandon := len(backend.Keys())

	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after Abandon: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("Stop after Abandon produced %d snapshots in total, want 1: %+v", len(results), results)
	}
	if got := len(backend.Keys()); got != keysAfterAbandon {
		t.Errorf("the store went from %d keys to %d across Stop; Abandon must have severed it",
			keysAfterAbandon, got)
	}
	if _, ok := readOwnerMarker(t, backend, sessionID); !ok {
		t.Error("Stop after Abandon deleted the owner marker; the abandonment would read as a clean release")
	}
	// Local teardown did happen, which is the reason to call Stop at all.
	if eng.Journal != nil {
		t.Error("Stop after Abandon left the journal open")
	}
	if _, err := os.Stat(filepath.Join(eng.Session.RootDir, sessionLockFilename)); !os.IsNotExist(err) {
		t.Errorf("the session lock survived Stop after Abandon (%v)", err)
	}
}

// Abandon detaches the blob hook and joins the worker, so a blob written
// afterwards cannot reach the store. Asserted without a poll on purpose: the
// join is what makes "not yet" and "never" the same answer here.
func TestAbandonDetachesTheBlobHook(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID
	store, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}

	eng.Abandon()

	if eng.blobPushes != nil {
		t.Error("the write-through worker is still installed after Abandon")
	}
	h, err := store.Put([]byte("written after the host gave up"), "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := backend.Get(blobKeyPrefix(sessionID, h.SHA256) + ".bin"); ok {
		t.Error("a blob written after Abandon reached the store")
	}
}

// The drain-versus-discard decision, at the level where it is decided.
//
// stopBlobWriteThrough finishes the queue because the shutdown snapshot behind
// it would re-upload the same bytes anyway. Abandon has no snapshot behind it,
// so the same drain would be a write into a store the caller just said to stop
// writing to. The control case runs the identical queue without the flag, which
// is what makes the negative mean something rather than proving the items were
// unpushable all along.
func TestAbandonedBlobWriteThroughDiscardsItsQueue(t *testing.T) {
	for _, discard := range []bool{false, true} {
		name := "drains"
		if discard {
			name = "discards"
		}
		t.Run(name, func(t *testing.T) {
			eng, backend := newMemoryObjectStoreEngine(t, "")
			if err := eng.Boot(context.Background()); err != nil {
				t.Fatalf("Boot: %v", err)
			}
			t.Cleanup(func() { _ = eng.Stop(context.Background()) })
			sessionID := eng.Session.ID

			// blobs.New rather than SessionWorkspace.BlobStore: no put hook, so
			// the engine's own live worker never sees these and the only thing
			// that can push them is the worker this test drives by hand.
			bs, err := blobs.New(eng.Session.BlobsDir(), 0)
			if err != nil {
				t.Fatalf("blobs.New: %v", err)
			}

			w := &blobWriteThrough{
				queue: make(chan blobPushItem, 8),
				stop:  make(chan struct{}),
				done:  make(chan struct{}),
			}
			var shas []string
			for _, body := range []string{"queued one", "queued two", "queued three"} {
				h, err := bs.Put([]byte(body), "text/plain")
				if err != nil {
					t.Fatalf("blob put: %v", err)
				}
				shas = append(shas, h.SHA256)
				w.queue <- blobPushItem{
					binPath:  h.Path,
					metaPath: strings.TrimSuffix(h.Path, ".bin") + ".meta",
				}
			}

			w.discard.Store(discard)
			close(w.stop)
			// Run the worker on this goroutine: with both the queue and the
			// stop signal ready, select picks between them at random, and
			// running it here means every one of those interleavings has to
			// reach the same answer for the test to pass.
			eng.runBlobWriteThrough(w, eng.objectStore, eng.Session)

			for _, sha := range shas {
				_, ok := backend.Get(blobKeyPrefix(sessionID, sha) + ".bin")
				if ok == discard {
					t.Errorf("blob %s present=%v with discard=%v", sha[:8], ok, discard)
				}
			}
		})
	}
}
