package engine

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestSessionWorkspace_WriteFile_SurvivesKillMidWrite is the regression test
// for the atomic-write fix in WriteFile (E11-S1).
//
// Before the fix, WriteFile wrote straight into the target path via a plain
// os.WriteFile, which opens with O_TRUNC and then streams the new bytes in.
// A process killed partway through that stream left the target holding
// neither the old nor the new content — exactly what surfaced downstream as
// "parsing session metadata: unexpected end of JSON input" on the next
// recall, since metadata/session.json is written through this same method.
//
// A test that only hand-crafts a truncated JSON file and checks that it
// fails to parse would prove the read path's error message, not that the
// write path is fixed. This test instead races a real SIGKILL against a
// subprocess that is actually inside WriteFile, repeatedly across a spread
// of kill delays (since the exact in-flight window depends on disk/OS
// speed), and asserts that the file WriteFile was targeting is — every
// single time — either byte-for-byte the previous complete write or
// byte-for-byte the new complete write. Never a partial one.
//
// The subprocess is this same test binary, re-exec'd with an env var that
// makes this very test function behave as the kill-race helper instead of
// the driver: see the branch at the top of the function body.
func TestSessionWorkspace_WriteFile_SurvivesKillMidWrite(t *testing.T) {
	if os.Getenv("NEXUS_WRITEFILE_ATOMIC_HELPER") == "1" {
		runWriteFileAtomicKillRaceHelper(t)
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	root := t.TempDir()
	const subpath = "metadata/session.json"
	// Large enough that a single WriteFile call takes measurable time even
	// on fast/tmpfs-backed storage, so the kill delays below have a real
	// chance of landing while a write is in flight rather than always before
	// or always after it.
	const size = 16 << 20 // 16MiB

	wantA := bytes.Repeat([]byte{'A'}, size)
	wantB := bytes.Repeat([]byte{'B'}, size)
	targetPath := filepath.Join(root, subpath)

	// Seed a known-good baseline through the very method under test, so
	// every trial below starts from a file guaranteed to exist and be
	// complete — the post-kill assertion can then require existence, not
	// merely tolerate its absence.
	ws := &SessionWorkspace{ID: "kill-race-seed", RootDir: root}
	if err := ws.WriteFile(subpath, wantA); err != nil {
		t.Fatalf("seeding baseline: %v", err)
	}

	// Sweep kill delays across several orders of magnitude instead of
	// picking one arbitrary duration. A single write's wall-clock duration
	// depends on the disk/OS under test, and doubling from microseconds to
	// tens of milliseconds makes it very likely that at least one trial's
	// kill lands squarely inside an in-flight write, regardless of that
	// speed.
	trials := 0
	for delay := 10 * time.Microsecond; delay < 100*time.Millisecond; delay *= 2 {
		trials++

		cmd := exec.Command(exePath, "-test.run=^TestSessionWorkspace_WriteFile_SurvivesKillMidWrite$")
		cmd.Env = append(os.Environ(),
			"NEXUS_WRITEFILE_ATOMIC_HELPER=1",
			"NEXUS_WRITEFILE_ATOMIC_DIR="+root,
			"NEXUS_WRITEFILE_ATOMIC_SUBPATH="+subpath,
			"NEXUS_WRITEFILE_ATOMIC_SIZE="+strconv.Itoa(size),
		)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		if err := cmd.Start(); err != nil {
			t.Fatalf("trial %d: starting kill-race helper: %v", trials, err)
		}

		time.Sleep(delay)
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()

		// A helper that exited on its own (rather than being torn down by
		// our SIGKILL) means the wiring around the helper — not the thing
		// under test — is broken; surface that loudly instead of silently
		// falling through to a check of the (untouched) baseline.
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			if exitErr.ExitCode() != -1 {
				t.Fatalf("trial %d (delay=%s): helper exited on its own (code=%d) instead of being killed, stderr:\n%s",
					trials, delay, exitErr.ExitCode(), stderr.String())
			}
		} else if waitErr != nil {
			t.Fatalf("trial %d (delay=%s): waiting for helper: %v", trials, delay, waitErr)
		}

		data, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatalf("trial %d (delay=%s): target file missing or unreadable after kill: %v", trials, delay, err)
		}
		if !bytes.Equal(data, wantA) && !bytes.Equal(data, wantB) {
			preview := data
			if len(preview) > 64 {
				preview = preview[:64]
			}
			t.Fatalf("trial %d (delay=%s): target file corrupted by kill mid-write: len=%d (want %d), starts %q",
				trials, delay, len(data), size, preview)
		}
	}

	t.Logf("ran %d kill-race trials, target file was always intact", trials)
}

// runWriteFileAtomicKillRaceHelper is the subprocess side of
// TestSessionWorkspace_WriteFile_SurvivesKillMidWrite: it writes two large,
// distinguishable payloads through WriteFile back-to-back in a tight loop so
// the parent's SIGKILL, sent at an unpredictable point in that loop, has a
// real write in flight to land on.
//
// The 10-second deadline is not expected to be reached in the normal case —
// the parent kills this process within, at most, tens of milliseconds — it
// exists only so a helper that is somehow never signaled (e.g. the parent
// failed before reaching the kill) still exits instead of leaking a process.
func runWriteFileAtomicKillRaceHelper(t *testing.T) {
	dir := os.Getenv("NEXUS_WRITEFILE_ATOMIC_DIR")
	subpath := os.Getenv("NEXUS_WRITEFILE_ATOMIC_SUBPATH")
	size, err := strconv.Atoi(os.Getenv("NEXUS_WRITEFILE_ATOMIC_SIZE"))
	if err != nil || dir == "" || subpath == "" {
		t.Fatalf("kill-race helper: missing/invalid env (dir=%q subpath=%q size-err=%v)", dir, subpath, err)
	}

	ws := &SessionWorkspace{ID: "kill-race-helper", RootDir: dir}
	a := bytes.Repeat([]byte{'A'}, size)
	b := bytes.Repeat([]byte{'B'}, size)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := ws.WriteFile(subpath, a); err != nil {
			t.Fatalf("kill-race helper: WriteFile(a): %v", err)
		}
		if err := ws.WriteFile(subpath, b); err != nil {
			t.Fatalf("kill-race helper: WriteFile(b): %v", err)
		}
	}
}
