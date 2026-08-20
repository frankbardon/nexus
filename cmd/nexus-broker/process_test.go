package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBuildCommand_PutsTheInstanceInItsOwnProcessGroup pins the mechanism the
// whole descendant-teardown story rests on. Without Setpgid the instance shares
// the BROKER's process group, which means two things at once: its children
// survive its death, and there is no group the broker may safely signal.
func TestBuildCommand_PutsTheInstanceInItsOwnProcessGroup(t *testing.T) {
	cmd := buildCommand(spawnSpec{
		binaryPath: "/opt/nexus/bin/nexus",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
	})

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %+v, want Setpgid; without it the instance shares the broker's process group", cmd.SysProcAttr)
	}
	// No run_as configured, so nothing should have set a credential.
	if cmd.SysProcAttr.Credential != nil {
		t.Errorf("Credential = %+v, want none for a spawn with no run_as", cmd.SysProcAttr.Credential)
	}
}

// TestBuildCommand_ProcessGroupAndCredentialCoexist is the other half of
// TestApplyRunAs_ExtendsSysProcAttr: that test proves applyRunAs extends the
// struct, this one proves the two are actually applied together on the real
// spawn path. Either one assigning SysProcAttr wholesale would turn a privilege
// drop into a no-op, or leave every instance in the broker's process group.
func TestBuildCommand_ProcessGroupAndCredentialCoexist(t *testing.T) {
	uid, gid := 1002, 1003
	cmd := buildCommand(spawnSpec{
		binaryPath: "/opt/nexus/bin/nexus",
		configPath: "/tmp/claim-123.yaml",
		leaseID:    "lease-abc",
		brokerAddr: "ws://127.0.0.1:8080/instance",
		runAs:      &RunAsSpec{UID: &uid, GID: &gid, ResolvedHome: "/home/nexus"},
	})

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil, want both Setpgid and a Credential")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid was dropped; run_as replaced SysProcAttr instead of extending it")
	}
	if cmd.SysProcAttr.Credential == nil {
		t.Error("Credential was dropped; the process-group setup replaced SysProcAttr instead of extending it")
	}
}

// TestGroupSignalTarget_RefusesTargetsThatWouldSignalTheWorld covers the
// catastrophic failure mode of negative-pid signalling.
//
// kill(-1, sig) signals every process the caller may signal; on a root broker
// that is the machine. kill(0, sig) signals the CALLER's own process group — the
// broker itself. Both are one negation away from pids the broker genuinely
// handles: processHandle.pid() returns 0 before a process starts, and a truncated
// journal record decodes to 0 too.
func TestGroupSignalTarget_RefusesTargetsThatWouldSignalTheWorld(t *testing.T) {
	for _, pid := range []int{0, 1, -1, -1000} {
		t.Run(strconv.Itoa(pid), func(t *testing.T) {
			target, err := groupSignalTarget(pid)
			if !errors.Is(err, errUnsafeSignalTarget) {
				t.Fatalf("groupSignalTarget(%d) = (%d, %v), want errUnsafeSignalTarget", pid, target, err)
			}
			if target != 0 {
				t.Errorf("refused target = %d, want 0 so a caller that ignores the error signals nothing", target)
			}
		})
	}
}

// TestSignalProcessGroup_RefusesUnsafeTargets pins that the refusal survives the
// wrapper: signalProcessGroup must not reach syscall.Kill at all for these pids.
func TestSignalProcessGroup_RefusesUnsafeTargets(t *testing.T) {
	for _, pid := range []int{0, 1, -1} {
		if err := signalProcessGroup(pid, syscall.SIGTERM); !errors.Is(err, errUnsafeSignalTarget) {
			t.Errorf("signalProcessGroup(%d) error = %v, want errUnsafeSignalTarget", pid, err)
		}
	}
}

// TestGroupSignalTarget_LeaderIsNegatedNonLeaderIsNot is the second guard, and
// the one that keeps a MISSING process group from becoming a broker suicide.
//
// A process that does not lead its own group is sitting in somebody else's —
// here, and on the adopted path in production, that is the signaller's own. The
// non-leader case therefore asserts both that the target is the bare pid and that
// negating it would have hit the caller's group.
func TestGroupSignalTarget_LeaderIsNegatedNonLeaderIsNot(t *testing.T) {
	t.Run("leader", func(t *testing.T) {
		cmd := startGroupSleeper(t, true)
		pid := cmd.Process.Pid
		target, err := groupSignalTarget(pid)
		if err != nil {
			t.Fatalf("groupSignalTarget(%d): %v", pid, err)
		}
		if target != -pid {
			t.Errorf("target = %d, want %d (the group the instance leads)", target, -pid)
		}
	})

	t.Run("non-leader", func(t *testing.T) {
		cmd := startGroupSleeper(t, false)
		pid := cmd.Process.Pid
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			t.Fatalf("Getpgid(%d): %v", pid, err)
		}
		ownPgid, err := syscall.Getpgid(os.Getpid())
		if err != nil {
			t.Fatalf("Getpgid(self): %v", err)
		}
		if pgid != ownPgid {
			t.Skipf("child pgid %d is not this process's group %d; the case under test is not reproduced", pgid, ownPgid)
		}

		target, err := groupSignalTarget(pid)
		if err != nil {
			t.Fatalf("groupSignalTarget(%d): %v", pid, err)
		}
		if target != pid {
			t.Fatalf("target = %d, want the bare pid %d: signalling %d would have hit this process's own group", target, pid, target)
		}
	})
}

// TestExecProcess_SignalsReachGrandchildren is the point of the process-group
// work: an instance's descendants — shell-tool commands, MCP stdio servers, code
// interpreters — must die with it rather than being re-parented to init and left
// holding the session's files and API budget.
//
// A test that only watched the direct child would pass without the fix, so this
// one watches a GRANDCHILD: the spawned shell forks a second process that records
// its own pid, and the assertion is on that pid.
func TestExecProcess_SignalsReachGrandchildren(t *testing.T) {
	cases := []struct {
		name   string
		signal func(*execProcess) error
	}{
		{"terminate", (*execProcess).terminate},
		{"kill", (*execProcess).kill},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
			// The backgrounded `sh -c` is the grandchild: it records its pid and
			// then becomes a long sleep. The foreground sleep keeps the direct
			// child alive so the two deaths are distinguishable.
			script := fmt.Sprintf("sh -c 'echo $$ > %s; exec sleep 300' &\nexec sleep 300", pidFile)
			cmd := exec.Command("/bin/sh", "-c", script)
			applyProcessGroup(cmd)
			if err := cmd.Start(); err != nil {
				t.Fatalf("starting the instance stand-in: %v", err)
			}
			proc := &execProcess{cmd: cmd}
			child := cmd.Process.Pid

			grandchild := readPIDFile(t, pidFile)
			t.Cleanup(func() {
				// Belt and braces: if the assertion below fails, do not leave a
				// stray sleep behind for the rest of the suite.
				_ = syscall.Kill(grandchild, syscall.SIGKILL)
			})
			if !processAlive(grandchild) {
				t.Fatalf("grandchild %d was not alive before the signal", grandchild)
			}

			if err := tc.signal(proc); err != nil {
				t.Fatalf("signalling the instance: %v", err)
			}
			if err := proc.wait(); err == nil && tc.name == "kill" {
				t.Errorf("wait after SIGKILL returned nil, want a signal exit error")
			}

			waitForExit(t, grandchild)
			if processAlive(child) {
				// The direct child is ours, so it is reaped by wait above; this is
				// only a sanity check that nothing resurrected it.
				t.Errorf("direct child %d is still alive after teardown", child)
			}
		})
	}
}

// TestAdoptedProcess_KillReachesGrandchildren covers the other teardown path.
//
// An adopted instance is one inherited from a previous broker across a restart,
// so it has no dial-back socket by definition — but it does have the same
// subprocess tree, and reaping it used to leave that tree behind exactly as the
// spawned path did.
func TestAdoptedProcess_KillReachesGrandchildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := fmt.Sprintf("sh -c 'echo $$ > %s; exec sleep 300' &\nexec sleep 300", pidFile)
	cmd := exec.Command("/bin/sh", "-c", script)
	applyProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the adopted stand-in: %v", err)
	}
	// Reap it as init would in production: an adopted process is NOT the broker's
	// child, so an unreaped zombie here would make adoptedProcess.kill wait out
	// its whole escalation window against a fixture artefact.
	waited := make(chan struct{})
	go func() { defer close(waited); _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-waited })

	grandchild := readPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	if err := (&adoptedProcess{id: cmd.Process.Pid}).kill(); err != nil {
		t.Fatalf("killing the adopted process: %v", err)
	}
	waitForExit(t, grandchild)
}

// TestExecProcess_SignalIsANoOpAfterReaping pins the pid-reuse guard. Signalling
// goes through the raw pid because a group signal has to, and a raw pid is
// exactly what the kernel may hand to an unrelated process the moment the child
// is reaped — so a late kill must do nothing at all.
func TestExecProcess_SignalIsANoOpAfterReaping(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	applyProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	proc := &execProcess{cmd: cmd}
	if err := proc.wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if err := proc.terminate(); err != nil {
		t.Errorf("terminate after reaping = %v, want nil", err)
	}
	if err := proc.kill(); err != nil {
		t.Errorf("kill after reaping = %v, want nil", err)
	}
}

// startGroupSleeper starts a long-lived child, optionally as the leader of its own
// process group, and guarantees it is cleaned up.
func startGroupSleeper(t *testing.T, ownGroup bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if ownGroup {
		applyProcessGroup(cmd)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// readPIDFile waits for the grandchild to record its pid and returns it.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the grandchild never recorded its pid in %s", path)
	return 0
}

// waitForExit fails unless pid is gone within a bounded window.
func waitForExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d survived its instance; killing an instance must kill what it started", pid)
}
