package engine

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteReadRemoveSessionLock(t *testing.T) {
	dir := t.TempDir()

	if err := WriteSessionLock(dir, 4242); err != nil {
		t.Fatalf("WriteSessionLock: %v", err)
	}

	lock, err := ReadSessionLock(dir)
	if err != nil {
		t.Fatalf("ReadSessionLock: %v", err)
	}
	if lock.PID != 4242 {
		t.Fatalf("PID = %d, want 4242", lock.PID)
	}
	if lock.StartedAt.IsZero() {
		t.Fatalf("StartedAt should be set")
	}

	if err := RemoveSessionLock(dir); err != nil {
		t.Fatalf("RemoveSessionLock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, sessionLockFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file should be gone, got err=%v", err)
	}

	// Removing again is a no-op.
	if err := RemoveSessionLock(dir); err != nil {
		t.Fatalf("second RemoveSessionLock: %v", err)
	}
}

func TestWriteSessionLockWithTransport(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSessionLockWithTransport(dir, os.Getpid(), "tui"); err != nil {
		t.Fatalf("WriteSessionLockWithTransport: %v", err)
	}
	lock, err := ReadSessionLock(dir)
	if err != nil {
		t.Fatalf("ReadSessionLock: %v", err)
	}
	if lock.Transport != "tui" {
		t.Fatalf("Transport = %q, want %q", lock.Transport, "tui")
	}
}

func TestReadSessionLockMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadSessionLock(dir)
	if err == nil {
		t.Fatalf("ReadSessionLock on empty dir: expected error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestIsLockStale_LiveProcess(t *testing.T) {
	lock := SessionLock{PID: os.Getpid()}
	if IsLockStale(lock) {
		t.Fatalf("self-PID %d reported stale", lock.PID)
	}
}

func TestIsLockStale_DeadProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("liveness probing only implemented on linux/darwin")
	}
	// PIDs are 32-bit on the platforms we care about; math.MaxInt32 is
	// well beyond the live PID range and effectively guaranteed to be
	// unallocated.
	lock := SessionLock{PID: math.MaxInt32 - 1}
	if !IsLockStale(lock) {
		t.Fatalf("PID %d should be reported stale", lock.PID)
	}
}

func TestIsLockStale_ZeroPID(t *testing.T) {
	if !IsLockStale(SessionLock{PID: 0}) {
		t.Fatalf("PID=0 should be treated as stale")
	}
	if !IsLockStale(SessionLock{PID: -1}) {
		t.Fatalf("negative PID should be treated as stale")
	}
}

func TestSessionLockedError(t *testing.T) {
	err := &SessionLockedError{
		Dir:  "/tmp/sess",
		Lock: SessionLock{PID: 99999},
	}
	if !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("SessionLockedError should wrap ErrSessionLocked")
	}
	if msg := err.Error(); msg == "" || !contains(msg, "99999") {
		t.Fatalf("error message should name the PID, got %q", msg)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
