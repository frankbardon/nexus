package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// This file makes one failure mode loud. It does not fix it.
//
// # The failure
//
// The object-store seam assumes a session has exactly one writing host at a
// time. Nothing enforces that. Two hosts can hydrate the same session ID — a
// container the scheduler presumed dead but which is still running, a broker
// that re-spawned an instance whose predecessor never actually exited, an
// operator resuming a session by hand — and both will then snapshot the whole
// tree at their own turn boundaries. A snapshot replaces objects wholesale, so
// the loser's conversation history, journal and per-plugin store.db are
// overwritten at whole-file granularity. No error is raised anywhere, on either
// host. The session simply loses half its work, and the only evidence is that
// the transcript does not match what the user watched happen.
//
// On ephemeral compute, presumed-dead-but-alive instances are routine, not
// exotic. That is what makes this worth detecting.
//
// # What this is, and firmly is not
//
// It is an owner *marker*: a small object beside the session tree naming the
// host, PID and per-run instance ID that claimed the session, with a heartbeat
// refreshed while the run is live. A second host reads it before it starts
// working, and if someone else appears to still be holding the session it logs
// at error level and raises session.owner.conflict on the bus.
//
// It is NOT a lease. Deliberately:
//
//   - No lock is taken. There is no compare-and-set, no conditional write, no
//     precondition header. The marker is written with an ordinary Put, and a
//     second writer overwrites the first.
//   - No fencing token is issued, so nothing downstream can reject a write from
//     a superseded owner.
//   - Nothing is refused. A conflicting hydrate proceeds exactly as it would
//     have before this file existed — same tree, same snapshots, same
//     everything. Refusing would mean an incorrectly-detected conflict could
//     strand a session no one can open, which is a worse failure than the one
//     being detected.
//   - Nothing waits. There is no retry loop, no backoff-until-clear, no
//     expiry the engine blocks on.
//
// Fencing, expiry semantics and refusal are a real lease, and a real lease is a
// separate effort with its own risk budget. Everything here is diagnostics:
// **this detects, it does not prevent.**
//
// # Why the marker is a sibling key, not part of the tree
//
// The marker lives under "sessions/<id>.owner/", which — because
// objectstore.TrimKeyPrefix matches on whole segments — is NOT under the
// session's own prefix "sessions/<id>". So it never hydrates down into the
// local tree and never becomes an input to the next snapshot. Exactly the
// argument sessionSnapshotMarkerKey makes for the commit marker, and for the
// same reason: something that describes a session must not become part of it.
//
// It is a directory-shaped prefix rather than a flat "sessions/<id>.owner.json"
// only because objectstore.Backend has no single-object read. Hydrate is the
// only way to pull bytes down, and Hydrate takes a *prefix* whose exact-match
// object is explicitly not "under" it. The rejected alternative was to widen
// the published Backend interface with a Get method; that interface is the one
// thing out-of-repo backend modules compile against, and adding a method to it
// to serve one small diagnostic object is a breaking change for every third
// party the registry exists to support. One extra key segment costs nothing.
//
// # Why the local session lock is not enough
//
// pkg/engine/session_lock.go already refuses to boot against a live session —
// on one machine. It carries a local PID and is excluded from the seam for
// exactly that reason (see objectStoreExcluded). A PID from host A means
// nothing on host B, so the lock cannot see across hosts and the marker cannot
// replace it. They answer different questions and both stay.

const (
	// sessionOwnerKeySuffix turns a session key prefix into the prefix holding
	// its owner marker. A sibling of the tree, like the commit marker's
	// ".snapshot.json" suffix — and sharing that scheme's one pathological
	// case: a session literally named "<other-id>.owner" would collide. Session
	// IDs are timestamps or UUIDs, and the collision predates this file, so it
	// is recorded rather than guarded against with a second naming rule.
	sessionOwnerKeySuffix = ".owner"

	// sessionOwnerMarkerName is the object under that prefix. Fixed rather
	// than per-host on purpose: one object that the current holder overwrites,
	// not an accumulating set of claims nobody deletes.
	sessionOwnerMarkerName = "owner.json"

	// sessionOwnerMarkerVersion is the marker's own on-disk format version,
	// kept separate from the bus event so an operator reading the bucket can
	// tell what they are looking at without a Nexus build to hand. Same split
	// sessionSnapshotMarkerVersion makes.
	sessionOwnerMarkerVersion = 1

	// sessionOwnerHeartbeatInterval is how often a live run refreshes its
	// marker. Frequent enough that a marker left by a crash is recognisably
	// stale within minutes, rare enough that the cost — one small Put and one
	// Flush — is invisible beside a turn-boundary snapshot of the whole tree.
	sessionOwnerHeartbeatInterval = 30 * time.Second

	// sessionOwnerStaleAfter is how old a heartbeat must be before the holder
	// is presumed gone. Ten missed beats, which is a lot of slack, and the
	// slack is the point twice over:
	//
	//   - The heartbeat timestamp is written by the *other* host's clock and
	//     compared against ours. NTP-level skew is milliseconds, but a badly
	//     configured container clock is not, and an alarm that fires because
	//     two machines disagree about the time is an alarm everyone learns to
	//     ignore.
	//   - A heartbeat only becomes visible after a Flush, and a backend that
	//     batches may take a while.
	//
	// The cost of being generous is a longer window in which a genuinely dead
	// holder still looks live — and since nothing is refused, that window costs
	// only a missed alarm, never a blocked session.
	//
	// A constant rather than a config key, for the reason E3-S2 gave for the
	// same choice: every knob has to be documented, validated and supported
	// forever, and an operator who wants to tune this is really asking for a
	// lease, which this is not.
	sessionOwnerStaleAfter = 10 * sessionOwnerHeartbeatInterval

	// sessionOwnerIOTimeout bounds one marker read, write or delete. Short:
	// none of them is on any critical path, and a wedged store must not delay
	// a boot or a shutdown for a diagnostic.
	sessionOwnerIOTimeout = 15 * time.Second
)

// sessionOwnerKeyPrefix is the key prefix holding the owner marker for a
// session. See the block comment above for why it is a sibling of the tree.
func sessionOwnerKeyPrefix(sessionID string) string {
	return sessionObjectKeyPrefix(sessionID) + sessionOwnerKeySuffix
}

// sessionOwnerMarkerKey is the marker object itself.
func sessionOwnerMarkerKey(sessionID string) string {
	return sessionOwnerKeyPrefix(sessionID) + "/" + sessionOwnerMarkerName
}

// sessionOwnerMarker is the JSON body of the marker. Small and fixed-size: it
// is rewritten every heartbeat, so anything proportional to the session would
// make the diagnostic a cost that grows with the thing it diagnoses.
type sessionOwnerMarker struct {
	SchemaVersion int    `json:"_schema_version"`
	SessionID     string `json:"session_id"`
	KeyPrefix     string `json:"key_prefix"`

	// Host is os.Hostname. On the target deployments (Kubernetes, Cloud Run,
	// ECS) it is per-instance and therefore genuinely identifying; on a laptop
	// two runs share it, which is what PID and InstanceID are for.
	Host string `json:"host"`
	// PID is the OS process ID. Only meaningful in combination with Host —
	// probing it against a different host's process table is exactly the
	// mistake session.lock exists to avoid making.
	PID int `json:"pid"`
	// InstanceID is unique per engine run. It is the field that makes the
	// answer to "is this marker mine?" exact, without which two containers
	// sharing a hostname and a PID would be indistinguishable.
	InstanceID string `json:"instance_id"`

	ClaimedAt time.Time `json:"claimed_at"`
	// HeartbeatAt is refreshed every sessionOwnerHeartbeatInterval while the
	// run is live. A marker whose HeartbeatAt has stopped advancing is the
	// signature of a holder that died without releasing.
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// sessionOwner is the run-scoped owner-marker state: the marker this process
// wrote, the scratch file it is staged through, and the heartbeat goroutine.
// Nil whenever no object-store backend is configured, so the default path grows
// no goroutine, no scratch directory and no remote object.
type sessionOwner struct {
	marker sessionOwnerMarker
	key    string

	// scratchDir holds the staged marker body. Backend.Put takes a local path,
	// so the marker needs a file somewhere; outside the session tree, because
	// anything inside it would be picked up by the whole-tree snapshot and
	// round-tripped back through hydration on the next resume.
	scratchDir string

	// mu guards marker.HeartbeatAt, which the heartbeat goroutine advances and
	// which release reads while writing the final state out.
	mu sync.Mutex

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// claimSessionOwnership records this process as the holder of the session, and
// raises the alarm if someone else already looks like one.
//
// Called from Boot once the session workspace exists and the local session lock
// has been taken, and always — for a resumed session and a brand-new one alike.
// Covering new sessions matters less for detection (a fresh ID has no prior
// holder) than for the run *after* it: without a marker written now, a second
// host resuming this session later would find nothing to conflict with.
//
// Covering the resume path unconditionally is what also makes E1-S2's "local
// tree wins" short-circuit visible. A warm host with a stale local tree skips
// hydration entirely and never touches the store — but it still passes through
// here, so a session another host has since taken over is detected rather than
// silently shadowed by the older local copy.
//
// This never fails the boot. Every error path logs and continues: the marker is
// a diagnostic, and a diagnostic that can take down a session it was only
// supposed to describe is a worse bug than the one it detects.
//
// A no-op when no backend is configured.
func (e *Engine) claimSessionOwnership(ctx context.Context) {
	store := e.objectStore
	if store == nil || e.Session == nil {
		return
	}

	host, err := os.Hostname()
	if err != nil {
		// Not fatal and not even worth a warning at boot: the marker still
		// carries a PID and a unique instance ID, which is enough to tell two
		// holders apart. Only the human-readable half degrades.
		host = "unknown"
	}
	now := time.Now().UTC()
	self := sessionOwnerMarker{
		SchemaVersion: sessionOwnerMarkerVersion,
		SessionID:     e.Session.ID,
		KeyPrefix:     sessionObjectKeyPrefix(e.Session.ID),
		Host:          host,
		PID:           os.Getpid(),
		InstanceID:    GenerateID(),
		ClaimedAt:     now,
		HeartbeatAt:   now,
	}

	scratch, err := os.MkdirTemp("", "nexus-session-owner-")
	if err != nil {
		e.Logger.Warn("object store: cannot stage the session owner marker; "+
			"split-brain detection is off for this run", "session_id", e.Session.ID, "error", err)
		return
	}

	owner := &sessionOwner{
		marker:     self,
		key:        sessionOwnerMarkerKey(e.Session.ID),
		scratchDir: scratch,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}

	// Read before write. The order is the whole detection: whatever is in the
	// store right now was put there by someone else, and this Put is about to
	// overwrite it.
	existing, found, err := e.readSessionOwnerMarker(ctx, store, e.Session.ID, scratch)
	switch {
	case err != nil:
		// A store that cannot be read is not evidence of a conflict, and
		// treating it as one would fire the alarm on every transient outage.
		e.Logger.Warn("object store: reading the session owner marker failed; "+
			"a second holder would not be detected on this boot",
			"session_id", e.Session.ID, "error", err)
	case found:
		e.evaluateSessionOwnerMarker(existing, self, now)
	}

	e.sessionOwner = owner
	if err := e.writeSessionOwnerMarker(ctx, store, owner, self); err != nil {
		// Logged, not fatal: the next heartbeat retries, and a session that
		// runs with no marker is exactly as correct as one from before this
		// file existed — just undetectable by the next host.
		e.Logger.Warn("object store: writing the session owner marker failed",
			"session_id", e.Session.ID, "key", owner.key, "error", err)
	}

	go e.runSessionOwnerHeartbeat(store, owner)
}

// evaluateSessionOwnerMarker decides whether the marker already in the store
// means someone else is holding this session, and if so logs and records the
// alarm.
//
// The alarm is *recorded* rather than emitted here. This runs during Boot,
// before startJournal has subscribed the journal's wildcard, and the bus
// assigns a sequence number to every event whether the journal is listening or
// not. The writer only flushes envelopes in contiguous sequence order, so one
// event emitted this side of startJournal stalls the drain and the journal
// comes out empty — the same trap installBlobWriteThrough documents. The event
// goes out from publishSessionOwnerConflict instead, after the journal is
// recording and after plugin Init has had a chance to subscribe.
func (e *Engine) evaluateSessionOwnerMarker(existing, self sessionOwnerMarker, now time.Time) {
	age := now.Sub(existing.HeartbeatAt)

	if reason, live := sessionOwnerStillLive(existing, self, now); !live {
		// Not a conflict. Logged at info because "I took over a session
		// somebody else left behind" is the ordinary shape of a resume after a
		// crash, and an operator grepping for the real alarm must not have to
		// wade through these.
		if existing.InstanceID != self.InstanceID {
			e.Logger.Info("object store: taking over an abandoned session owner marker",
				"session_id", self.SessionID,
				"holder_host", existing.Host,
				"holder_pid", existing.PID,
				"holder_instance_id", existing.InstanceID,
				"heartbeat_age", age.Round(time.Second),
				"reason", reason)
		}
		return
	}

	e.Logger.Error("object store: SPLIT BRAIN — this session appears to be open on another host; "+
		"both hosts will snapshot the whole tree at their turn boundaries and the later snapshot wins. "+
		"Nexus does not refuse this and cannot prevent it: stop one of the two runs",
		"session_id", self.SessionID,
		"holder_host", existing.Host,
		"holder_pid", existing.PID,
		"holder_instance_id", existing.InstanceID,
		"holder_heartbeat_at", existing.HeartbeatAt,
		"heartbeat_age", age.Round(time.Second),
		"local_host", self.Host,
		"local_pid", self.PID,
		"local_instance_id", self.InstanceID,
	)

	e.ownerConflict = &events.SessionOwnerConflict{
		SchemaVersion:       events.SessionOwnerConflictVersion,
		SessionID:           self.SessionID,
		HolderHost:          existing.Host,
		HolderPID:           existing.PID,
		HolderInstanceID:    existing.InstanceID,
		HolderHeartbeatAt:   existing.HeartbeatAt,
		HeartbeatAgeSeconds: age.Seconds(),
		LocalHost:           self.Host,
		LocalPID:            self.PID,
		LocalInstanceID:     self.InstanceID,
	}
}

// sessionOwnerStillLive judges an existing marker against this process, and is
// the whole of the false-alarm story.
//
// An alarm that fires on every legitimate resume after a crash is an alarm
// everybody learns to ignore, which would leave the real failure exactly as
// invisible as it is today. So a marker is treated as live only when nothing
// available says otherwise, and there are three ways for it to say otherwise:
//
//  1. It is ours. Only reachable if a run somehow re-claims its own session.
//  2. It was written by a process on *this* host that is no longer running.
//     This is the common case — a crashed container restarted in place, a
//     killed CLI resumed by hand — and it is exact, not heuristic: the same
//     signal-0 liveness probe IsLockStale uses on session.lock. It is sound
//     only because Host matches; a PID from another machine says nothing at
//     all about that machine, which is precisely the mistake that keeps
//     session.lock off the seam in the first place.
//  3. Its heartbeat stopped advancing more than sessionOwnerStaleAfter ago.
//     The only signal available for a remote host, and the reason the
//     threshold is deliberately generous.
//
// The residual false alarm is a crash on a *different* host resumed inside the
// staleness window. Nothing can distinguish that from a genuine second writer
// without a lease, and a lease is out of scope. It costs one logged error and
// one bus event on a run that is otherwise fine — the safe direction, given the
// alternative is missing a real split brain.
//
// The residual missed alarm is the mirror image: a second host that starts
// inside the window before the true holder's first heartbeat lands, or a marker
// whose Put never flushed. Both are misses, not corruption; nothing here gates
// anything.
func sessionOwnerStillLive(existing, self sessionOwnerMarker, now time.Time) (reason string, live bool) {
	if existing.InstanceID != "" && existing.InstanceID == self.InstanceID {
		return "marker belongs to this run", false
	}
	if existing.Host != "" && existing.Host == self.Host && existing.PID > 0 && !sessionOwnerPIDAlive(existing.PID) {
		return "holder process on this host is gone", false
	}
	if existing.HeartbeatAt.IsZero() {
		// A marker with no heartbeat at all is either from a build that did
		// not write one or a truncated write. Not evidence of a live holder.
		return "marker carries no heartbeat", false
	}
	if now.Sub(existing.HeartbeatAt) > sessionOwnerStaleAfter {
		return "heartbeat is stale", false
	}
	return "", true
}

// sessionOwnerPIDAlive probes local process liveness, and refuses to guess on
// platforms where the probe is not implemented.
//
// Mirrors IsLockStale's platform gate rather than calling pidAlive directly so
// that the *conservative* answer differs correctly: IsLockStale refuses to
// declare a lock stale without a reliable probe, because clobbering a live run
// is the cost there. Here an unavailable probe means falling through to the
// heartbeat check, which is the honest answer — this host cannot tell, so judge
// the marker the same way it would judge a remote one.
func sessionOwnerPIDAlive(pid int) bool {
	switch runtime.GOOS {
	case "linux", "darwin":
		return pidAlive(pid)
	default:
		return true
	}
}

// readSessionOwnerMarker pulls the marker down and parses it.
//
// found is false — with no error — when the store holds no marker, which is the
// ordinary case for a session nobody has opened before.
//
// The read goes through Hydrate because Backend has no single-object get; see
// the block comment at the top of this file for why the interface was not
// widened to add one. destDir gets a fresh subdirectory so a stale file from an
// earlier call can never be mistaken for a fresh read.
func (e *Engine) readSessionOwnerMarker(ctx context.Context, store *sessionObjectStore, sessionID string, scratch string) (sessionOwnerMarker, bool, error) {
	var marker sessionOwnerMarker

	readDir := filepath.Join(scratch, "read")
	if err := os.RemoveAll(readDir); err != nil {
		return marker, false, fmt.Errorf("owner marker: clearing read staging: %w", err)
	}
	if err := os.MkdirAll(readDir, 0o700); err != nil {
		return marker, false, fmt.Errorf("owner marker: read staging: %w", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, sessionOwnerIOTimeout)
	defer cancel()
	if err := store.backend.Hydrate(readCtx, sessionOwnerKeyPrefix(sessionID), readDir); err != nil {
		return marker, false, fmt.Errorf("owner marker: reading %s: %w", sessionOwnerKeyPrefix(sessionID), err)
	}

	data, err := os.ReadFile(filepath.Join(readDir, sessionOwnerMarkerName))
	if errors.Is(err, fs.ErrNotExist) {
		return marker, false, nil
	}
	if err != nil {
		return marker, false, fmt.Errorf("owner marker: reading staged marker: %w", err)
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		// Unparseable is treated as absent by the caller's switch, not as a
		// conflict: an alarm raised by a corrupt object would be noise, and a
		// marker written by a future format is not evidence of a live holder.
		return marker, false, fmt.Errorf("owner marker: parsing %s: %w", sessionOwnerMarkerKey(sessionID), err)
	}
	return marker, true, nil
}

// writeSessionOwnerMarker stages the marker and makes it durable.
//
// Flushed rather than left to the backend's own batching: an unflushed
// heartbeat is invisible to the host that needs to read it, which would make
// every marker look stale and turn detection off without saying so. The Flush
// is a whole-backend barrier, so it also makes any queued blob or snapshot work
// durable early — harmless, and once every thirty seconds.
func (e *Engine) writeSessionOwnerMarker(ctx context.Context, store *sessionObjectStore, owner *sessionOwner, marker sessionOwnerMarker) error {
	body, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("owner marker: marshaling: %w", err)
	}
	path := filepath.Join(owner.scratchDir, sessionOwnerMarkerName)
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("owner marker: staging: %w", err)
	}

	putCtx, cancel := context.WithTimeout(ctx, sessionOwnerIOTimeout)
	defer cancel()
	if err := store.backend.Put(putCtx, owner.key, path); err != nil {
		return fmt.Errorf("owner marker: uploading %s: %w", owner.key, err)
	}
	if err := store.backend.Flush(putCtx); err != nil {
		return fmt.Errorf("owner marker: flushing %s: %w", owner.key, err)
	}
	return nil
}

// runSessionOwnerHeartbeat refreshes the marker until the run ends.
//
// Without it a marker would only ever record when a session was claimed, and
// "claimed an hour ago" is indistinguishable from "crashed an hour ago" — every
// legitimate resume would then look like a conflict, or none would, depending
// on which way the threshold was set. The heartbeat is what makes stale
// distinguishable from active, which is the difference between a signal worth
// having and one everybody mutes.
func (e *Engine) runSessionOwnerHeartbeat(store *sessionObjectStore, owner *sessionOwner) {
	defer close(owner.done)

	ticker := time.NewTicker(sessionOwnerHeartbeatInterval)
	defer ticker.Stop()

	// Deliberately not derived from Boot's context: the heartbeat must keep
	// running for the whole session, and hosts routinely hand Boot a context
	// they cancel as soon as it returns.
	for {
		select {
		case <-owner.stop:
			return
		case <-ticker.C:
			e.beatSessionOwner(store, owner)
		}
	}
}

// beatSessionOwner advances the heartbeat and republishes the marker. Split out
// of the ticker loop so the substance of a beat is reachable without waiting a
// real interval for it.
func (e *Engine) beatSessionOwner(store *sessionObjectStore, owner *sessionOwner) {
	owner.mu.Lock()
	owner.marker.HeartbeatAt = time.Now().UTC()
	marker := owner.marker
	owner.mu.Unlock()

	if err := e.writeSessionOwnerMarker(context.Background(), store, owner, marker); err != nil {
		// Warn, never fail. A missed beat only widens the window in which this
		// run looks abandoned to somebody else; it cannot affect this run at
		// all.
		e.Logger.Warn("object store: session owner heartbeat failed",
			"session_id", marker.SessionID, "key", owner.key, "error", err)
	}
}

// releaseSessionOwnership stops the heartbeat and removes the marker.
//
// The delete is what keeps the alarm credible. The broker's normal lifecycle is
// release-and-respawn: an instance stops cleanly and the same session is
// resumed minutes later, well inside sessionOwnerStaleAfter. Leaving the marker
// behind would make every one of those ordinary resumes fire the split-brain
// error, and an alarm that fires on the happy path is an alarm that gets
// ignored. So a clean exit removes its own record; a crash leaves one, which is
// exactly the distinction worth drawing.
//
// This is not releasing a lock. Nothing was ever acquired, nothing is waiting
// on it, and a failed delete costs at most one spurious alarm within the
// staleness window — never a session that cannot be opened.
//
// Idempotent, and a no-op when nothing was claimed.
func (e *Engine) releaseSessionOwnership(store *sessionObjectStore) {
	owner := e.sessionOwner
	if owner == nil {
		return
	}
	e.sessionOwner = nil

	owner.stopOnce.Do(func() { close(owner.stop) })
	<-owner.done

	if store != nil {
		// A fresh background context: Stop is routinely handed an
		// already-cancelled one, and this is teardown work that still has to
		// happen after cancellation.
		ctx, cancel := context.WithTimeout(context.Background(), sessionOwnerIOTimeout)
		if err := store.backend.Delete(ctx, owner.key); err != nil {
			e.Logger.Warn("object store: removing the session owner marker failed; "+
				"a resume within the staleness window may report a false conflict",
				"key", owner.key, "error", err)
		} else if err := store.backend.Flush(ctx); err != nil {
			e.Logger.Warn("object store: flushing the session owner marker removal failed",
				"key", owner.key, "error", err)
		}
		cancel()
	}

	if err := os.RemoveAll(owner.scratchDir); err != nil {
		e.Logger.Warn("object store: removing the owner marker scratch dir failed",
			"dir", owner.scratchDir, "error", err)
	}
}

// publishSessionOwnerConflict raises the alarm recorded during Boot, once.
//
// Called from Boot after the journal is recording and after plugin Init and
// Ready have run, so the event lands in the journal *and* reaches subscribers.
// Emitting it at the point of detection would do neither: hydration and the
// ownership claim both run before startJournal, and an event emitted there
// consumes a dispatch sequence the journal writer never sees, stalling its
// contiguous-order drain and emptying the journal for the whole run.
func (e *Engine) publishSessionOwnerConflict() {
	conflict := e.ownerConflict
	if conflict == nil {
		return
	}
	e.ownerConflict = nil
	_ = e.Bus.Emit("session.owner.conflict", *conflict)
}
