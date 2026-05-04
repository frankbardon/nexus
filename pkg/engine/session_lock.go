package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// sessionLockFilename is the on-disk name of the per-session lock file.
// It lives directly under the session root so a session is "locked"
// exactly when this file is present and points at a live PID.
const sessionLockFilename = "session.lock"

// SessionLock describes the contents of a session.lock file. The PID
// plus StartedAt are the two pieces operator tools need to detect a
// stale lock (process gone) versus a live lock (process still running).
type SessionLock struct {
	// PID is the OS process ID that wrote the lock. Used by IsLockStale
	// to probe liveness.
	PID int `json:"pid"`
	// StartedAt is the wall-clock time the lock was written. Operators
	// inspecting a stale lock can use it to gauge how old the crashed
	// run was; the engine itself only relies on PID for staleness.
	StartedAt time.Time `json:"started_at"`
	// Transport names the IO plugin (or whatever bootstrapped the
	// engine) so a stale-lock message can hint at where the previous
	// run came from. Optional; empty when unknown.
	Transport string `json:"transport,omitempty"`
}

// ErrSessionLocked is returned by acquire-style helpers when a non-stale
// lock is already present. Callers can errors.Is against it; the lock
// contents themselves are surfaced via SessionLockedError below.
var ErrSessionLocked = errors.New("session is locked by a running process")

// SessionLockedError carries the existing lock alongside ErrSessionLocked
// so error messages can name the offending PID without forcing every
// caller to re-read the file.
type SessionLockedError struct {
	Dir  string
	Lock SessionLock
}

func (e *SessionLockedError) Error() string {
	return fmt.Sprintf("session is already running, pid=%d — use a different session ID or stop the running process",
		e.Lock.PID)
}

func (e *SessionLockedError) Unwrap() error { return ErrSessionLocked }

// WriteSessionLock writes session.lock under dir with the supplied PID
// and the current wall-clock time. Existing files are overwritten —
// callers that want refuse-on-conflict semantics must call ReadSessionLock
// + IsLockStale first.
func WriteSessionLock(dir string, pid int) error {
	if dir == "" {
		return fmt.Errorf("session lock: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session lock: ensure dir: %w", err)
	}
	lock := SessionLock{
		PID:       pid,
		StartedAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("session lock: marshal: %w", err)
	}
	path := filepath.Join(dir, sessionLockFilename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("session lock: write %s: %w", path, err)
	}
	return nil
}

// WriteSessionLockWithTransport is the same as WriteSessionLock but
// records a transport tag (e.g. "tui", "browser", "wails") so stale-
// lock diagnostics can name the prior run's IO surface. Empty
// transports are omitted from the JSON.
func WriteSessionLockWithTransport(dir string, pid int, transport string) error {
	if dir == "" {
		return fmt.Errorf("session lock: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session lock: ensure dir: %w", err)
	}
	lock := SessionLock{
		PID:       pid,
		StartedAt: time.Now().UTC(),
		Transport: transport,
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("session lock: marshal: %w", err)
	}
	path := filepath.Join(dir, sessionLockFilename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("session lock: write %s: %w", path, err)
	}
	return nil
}

// ReadSessionLock loads the lock file at dir, returning os.ErrNotExist
// (wrapped) when no lock is present. A malformed lock is reported as a
// JSON error — callers that want to treat it as stale should do so
// explicitly.
func ReadSessionLock(dir string) (SessionLock, error) {
	var lock SessionLock
	if dir == "" {
		return lock, fmt.Errorf("session lock: empty dir")
	}
	path := filepath.Join(dir, sessionLockFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return lock, err
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return lock, fmt.Errorf("session lock: parse %s: %w", path, err)
	}
	return lock, nil
}

// RemoveSessionLock deletes the lock file under dir. Missing files are
// not an error — Stop is idempotent.
func RemoveSessionLock(dir string) error {
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, sessionLockFilename)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session lock: remove %s: %w", path, err)
	}
	return nil
}

// IsLockStale returns true when the lock's PID is no longer alive on
// the host. A zero or negative PID is considered stale. On platforms
// where liveness probing is not implemented (anything other than
// linux/darwin), the lock is treated as non-stale so we err on the
// side of refusing to clobber a real run.
func IsLockStale(lock SessionLock) bool {
	if lock.PID <= 0 {
		return true
	}
	switch runtime.GOOS {
	case "linux", "darwin":
		return !pidAlive(lock.PID)
	default:
		// Windows and other platforms: conservative default. We do not
		// claim staleness without a reliable liveness probe — a real
		// run would be silently overwritten, which is worse than
		// requiring --force to recover.
		return false
	}
}

// pidAlive probes process liveness via signal 0, which performs the
// kernel's permission + existence check without delivering a signal.
// Unix-only; callers that need cross-platform behavior must gate via
// runtime.GOOS as IsLockStale does.
func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		// On Unix, FindProcess never fails — but be defensive in case
		// future Go versions tighten that contract.
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM → the process exists but is owned by another user; still
	// alive for our purposes (we cannot signal it, but neither could
	// we trust an overwrite of its lock).
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	// ESRCH (no such process) and Go's "os: process already finished"
	// both indicate the PID is gone. Treat any other Signal failure as
	// gone too — better to log a stale-lock warning than refuse to
	// boot indefinitely.
	return false
}
