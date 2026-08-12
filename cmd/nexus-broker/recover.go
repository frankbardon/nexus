package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

const (
	// reasonNoReattach is the teardown reason recorded for a restored lease whose
	// instance never came back within reattach_window. It is deliberately distinct
	// from reasonIdle and reasonCrash so an operator reading the journal can tell
	// "nobody reconnected after a restart" from "the session went quiet" and from
	// "the process died".
	reasonNoReattach = "restart recovery: no reattach"

	// reasonNoProcess closes out a record for a lease that was minted but whose
	// instance was never spawned — the broker died in the window between the
	// `lease-created` record and the exec. There is nothing to kill.
	reasonNoProcess = "restart recovery: no process was ever spawned"

	// reasonProcessGone closes out a record whose pid is no longer alive: the
	// instance died while the broker was down (or with it).
	reasonProcessGone = "restart recovery: process is gone"

	// defaultReattachWindow bounds how long a restored lease may sit with no
	// instance reattached before it is reaped. It is generous relative to the
	// instance plugin's reconnect backoff (250ms doubling to a 5s ceiling), which
	// gets a surviving instance ~20 attempts inside the window even if it is mid
	// back-off when the broker returns.
	defaultReattachWindow = 60 * time.Second

	// adoptedPollInterval is how often an adopted (non-child) process is probed
	// for exit. The broker cannot wait() a process it did not fork, so exit
	// detection for a restored lease is polling by construction; a quarter second
	// is far below any human-visible teardown latency and costs one signal.
	adoptedPollInterval = 250 * time.Millisecond

	// adoptedTermGrace is how long a reaped adopted process is given after SIGTERM
	// before SIGKILL. See adoptedProcess.kill for why an adopted process gets a
	// signal escalation where a spawned one gets a straight kill.
	adoptedTermGrace = 2 * time.Second
)

// processAlive reports whether pid names a live process.
//
// Signal 0 is the portable liveness probe on darwin and linux: it runs the
// kernel's permission and existence checks and delivers nothing. EPERM counts as
// ALIVE — it means the process exists and belongs to another user, which is not
// the question being asked.
//
// One thing it does NOT have to cope with: a zombie, which signal 0 also reports
// as alive. The broker only ever probes pids it did not fork — an instance
// spawned by a PREVIOUS incarnation, re-parented to init when that process died
// and reaped by it — so a pid this broker asks about is never a child of its own
// awaiting a wait().
//
// IT CANNOT DISTINGUISH A REUSED PID. Between a broker dying and coming back the
// kernel may have handed a recorded pid to an unrelated process, and no probe can
// see the difference. That limitation is the entire reason a restored lease also
// has to pass the spawn-secret check before it goes active (see
// Registry.AttachInstance) — liveness decides what is worth keeping, identity
// decides what is allowed to attach.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// adoptedProcess is a processHandle for an instance this broker did NOT fork —
// one inherited from a previous incarnation of the broker via the lease journal.
//
// It exists so a restored lease can go through the SAME releaseLease teardown as
// every other lease. The alternative, a separate reap path for restored leases,
// would duplicate the shutdown-frame → grace → force-kill sequence and the slot
// accounting that hangs off it, which is the one thing this story was told not to
// do.
type adoptedProcess struct{ id int }

func (p *adoptedProcess) pid() int { return p.id }

// kill terminates an adopted process, escalating SIGTERM → SIGKILL.
//
// A spawned instance gets a straight SIGKILL because releaseLease has already
// asked it to stop politely, over the dial-back socket, and waited out
// release_grace. An adopted one being reaped is by definition one with NO socket
// to have asked on — that is why it is being reaped — so SIGTERM is the only
// graceful signal available, and the engine treats it as a clean shutdown that
// flushes and persists the session. SIGKILL follows within a bounded window so
// this stays a force-kill and not a request.
func (p *adoptedProcess) kill() error {
	proc, err := os.FindProcess(p.id)
	if err != nil {
		return fmt.Errorf("finding adopted instance process %d: %w", p.id, err)
	}
	// Already gone is success: the caller wanted it dead. Any OTHER SIGTERM error
	// falls through to the escalation below — a refused polite signal is not a
	// reason to leave the process running.
	if err := proc.Signal(syscall.SIGTERM); errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	deadline := time.Now().Add(adoptedTermGrace)
	for time.Now().Before(deadline) {
		if !processAlive(p.id) {
			return nil
		}
		time.Sleep(adoptedPollInterval)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("killing adopted instance process %d: %w", p.id, err)
	}
	return nil
}

// wait blocks until the adopted process is gone. It POLLS because wait(2) only
// works on one's own children and this process belongs to a broker that no longer
// exists; nothing else can reap it, so nothing else can be waited on. It always
// reports nil: an adopted process's exit status is not available to us, and
// inventing one would be worse than reporting none.
func (p *adoptedProcess) wait() error {
	for processAlive(p.id) {
		time.Sleep(adoptedPollInterval)
	}
	return nil
}

// recoverLeases replays the lease journal and rebuilds the set of leases that
// were live when this broker stopped. It returns the ids it restored, for the
// reattach reaper to bound.
//
// It MUST run before the broker serves: a surviving instance is already dialing
// /instance on its reconnect backoff, and until its lease is back in the registry
// every one of those attempts is refused as an unknown lease.
//
// Per record, exactly one of four things happens:
//
//   - Another broker's record → left alone. Nothing is adopted and, crucially,
//     nothing is killed: state_dir is per-broker by construction, so this only
//     happens when broker_id changed under a directory, and force-killing a pid
//     recorded by a broker we cannot identify is not a repair.
//   - No pid → closed out. The broker died between minting the lease and exec'ing
//     the instance, so there is no process and nothing to reattach.
//   - Pid not alive → closed out. The instance died while we were down.
//   - Pid alive → restored, with its slot, its owner and a derived spawn secret.
//     It stays INACTIVE until an instance presents that secret.
//
// A journal that cannot be read at all is a WARNING and a cold start, never a
// boot failure — losing the ability to reclaim instances is bad, refusing to
// serve is worse. (The store failing to OPEN is a different matter and is fatal;
// see openLeaseStore.)
func recoverLeases(logger *slog.Logger, reg *Registry, store LeaseStore, brokerID string, key spawnKey) []string {
	if store == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	recs, err := store.Live()
	if err != nil {
		logger.Warn("replaying the lease journal failed; this broker starts with no restored leases "+
			"and any instance it previously spawned is an orphan that must be cleaned up manually",
			"error", err)
		return nil
	}
	if len(recs) == 0 {
		return nil
	}

	var restored []string
	for _, rec := range recs {
		if rec.BrokerID != "" && brokerID != "" && rec.BrokerID != brokerID {
			logger.Warn("skipping a lease record written by a different broker; "+
				"state_dir must not be shared, and broker_id must be stable across restarts",
				"lease_id", rec.LeaseID, "record_broker_id", rec.BrokerID, "broker_id", brokerID)
			continue
		}
		switch {
		case rec.PID <= 0:
			logger.Info("dropping a restored lease that never had a process",
				"lease_id", rec.LeaseID)
			closeOutRecord(reg, rec, reasonNoProcess)
			continue
		case !processAlive(rec.PID):
			logger.Info("dropping a restored lease whose instance is gone",
				"lease_id", rec.LeaseID, "pid", rec.PID)
			closeOutRecord(reg, rec, reasonProcessGone)
			continue
		}

		// Derive the secret this lease's instance must present. With no key
		// (state_dir unset) there is no store either, so this branch is
		// unreachable — the guard is here because a future caller that wires one
		// without the other would otherwise restore leases nothing can ever
		// authenticate, and silently reaping them 60 seconds later is a worse way
		// to learn that than a log line.
		if len(key) == 0 {
			logger.Error("cannot derive a spawn secret for a restored lease (no spawn key); "+
				"the lease is dropped and its instance killed rather than left unauthenticatable",
				"lease_id", rec.LeaseID, "pid", rec.PID)
			killAdopted(logger, rec)
			closeOutRecord(reg, rec, reasonProcessGone)
			continue
		}
		secret, err := key.secretFor(rec.LeaseID)
		if err != nil {
			logger.Error("deriving the spawn secret for a restored lease failed",
				"lease_id", rec.LeaseID, "error", err)
			continue
		}

		spec := restoreSpec{
			id:          rec.LeaseID,
			owner:       principalFromRecord(rec.Owner),
			sessionID:   rec.SessionID,
			binary:      rec.Binary,
			pid:         rec.PID,
			createdAt:   rec.CreatedAt,
			spawnSecret: secret,
		}
		if err := reg.RestoreLease(spec); err != nil {
			logger.Error("restoring a lease failed", "lease_id", rec.LeaseID, "error", err)
			continue
		}
		// The adopted handle is what lets the shared teardown kill this instance,
		// and starts the single reaper goroutine whose exit signal the crash
		// watcher observes.
		reg.SetProcess(rec.LeaseID, &adoptedProcess{id: rec.PID})
		// Crash watching from the moment it is restored, not from reattach: an
		// adopted instance that dies before anything reconnects must still free its
		// slot and close out its record.
		go reg.watchExit(rec.LeaseID)

		restored = append(restored, rec.LeaseID)
		// `binary` is logged because a restored lease is the one case where the
		// broker did not choose the variant this boot: it is believing a record. An
		// empty value is a record from a broker that predates the field.
		logger.Info("restored lease from the journal; awaiting instance reattach",
			"lease_id", rec.LeaseID, "pid", rec.PID,
			"session_id", rec.SessionID, "binary", rec.Binary,
			"principal_id", rec.Owner.ID)
	}
	logger.Info("lease journal replayed",
		"records", len(recs), "restored", len(restored), "dropped", len(recs)-len(restored))
	return restored
}

// principalFromRecord rebuilds the owner of a restored lease from its persisted
// projection. Claims are absent by design (see LeaseOwnerRecord) and are NOT
// synthesised: ownership compares ID and nothing else, and a fabricated claim set
// would be a lie that some later policy check could come to trust.
func principalFromRecord(rec LeaseOwnerRecord) nexusauth.Principal {
	p := nexusauth.Principal{ID: rec.ID, Tenant: rec.Tenant}
	if rec.Scopes != nil {
		p.Scopes = make([]string, len(rec.Scopes))
		copy(p.Scopes, rec.Scopes)
	}
	return p
}

// closeOutRecord journals the teardown of a lease that recovery decided not to
// restore, so the next boot does not reconsider it. The original record is
// carried through verbatim apart from the three teardown fields, which keeps
// broker_id, advertise_addr and created_at exactly as they were written rather
// than restamped from the current config.
func closeOutRecord(reg *Registry, rec LeaseRecord, reason string) {
	now := reg.now()
	rec.Kind = leaseRecordReleased
	rec.ReleasedAt = &now
	rec.Reason = reason
	reg.appendRecord(rec)
}

// killAdopted force-terminates the instance behind a record recovery refuses to
// restore. It is used only on the unreachable no-key path: everywhere else a
// dropped record's process is already gone, and a restored one is torn down
// through releaseLease.
func killAdopted(logger *slog.Logger, rec LeaseRecord) {
	if err := (&adoptedProcess{id: rec.PID}).kill(); err != nil {
		logger.Warn("killing an unrecoverable instance failed",
			"lease_id", rec.LeaseID, "pid", rec.PID, "error", err)
	}
}

// reapUnreattached bounds how long restored leases may wait for their instances.
//
// After window elapses, every restored lease still in the pending state — nothing
// ever presented its spawn secret — is torn down through the SHARED releaseLease
// path, so the shutdown-frame → grace → force-kill sequence, the slot accounting
// and the `lease-released` record all stay in the one place that owns them. Each
// release runs in its own goroutine, as the idle sweeper does, so one stubborn
// instance cannot delay reaping its siblings.
//
// A non-positive window falls back to defaultReattachWindow. The window is
// deliberately NOT disableable: "wait forever" means a lease whose instance is
// never coming back holds a capacity slot for the life of the process, which is
// the orphan this whole epic exists to remove.
func reapUnreattached(ctx context.Context, logger *slog.Logger, reg *Registry, ids []string, window, grace time.Duration) {
	if len(ids) == 0 {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	if window <= 0 {
		window = defaultReattachWindow
	}
	if grace <= 0 {
		grace = defaultReleaseGrace
	}
	logger.Info("awaiting instance reattach for restored leases",
		"leases", len(ids), "reattach_window", window)

	t := time.NewTimer(window)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}

	stale := reg.unattachedRestored(ids)
	if len(stale) == 0 {
		logger.Info("every restored lease reattached within the window", "leases", len(ids))
		return
	}
	for _, id := range stale {
		logger.Warn("no instance reattached to a restored lease within reattach_window; reaping it",
			"lease_id", id, "pid", reg.PID(id), "reattach_window", window)
		go func(id string) {
			if err := reg.releaseLease(id, reasonNoReattach, grace); err != nil &&
				!errors.Is(err, errUnknownLease) {
				logger.Warn("reaping an unreattached lease failed", "lease_id", id, "error", err)
			}
		}(id)
	}
}
