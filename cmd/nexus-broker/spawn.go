package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// spawnSpec describes a single instance spawn: which binary to exec, the temp
// config file the instance must read, and the lease/dial-back coordinates it
// needs to find its way back to this broker.
type spawnSpec struct {
	binaryPath string
	configPath string
	leaseID    string
	brokerAddr string // ws:// URL of the broker's instance dial-back endpoint

	// spawnSecret is the per-lease second factor the instance must echo in its
	// register frame (see newSpawnSecret). The claim path mints it, records the
	// expected value on the lease, and passes it here; buildCommand injects it
	// into the child's environment. It is NEVER logged and never leaves the
	// broker↔child channel.
	spawnSecret string

	// recallSessionID, when non-empty, resumes a persisted session: the
	// instance is spawned with -recall <id> so the engine reloads that
	// session and replays its history instead of starting fresh.
	recallSessionID string
}

// processHandle is the broker's minimal view of a spawned instance process.
// It is tracked on the lease so later stories (release, crash, capacity) can
// manage the process lifecycle without the gateway knowing about exec.
type processHandle interface {
	// pid returns the OS process id, or 0 if the process has not started.
	pid() int
	// kill forcibly terminates the process.
	kill() error
	// wait blocks until the process exits and returns its exit error.
	wait() error
}

// commandRunner builds and starts an instance process from a spawnSpec. The
// production implementation exec()s the nexus binary; unit tests substitute a
// fake that records the spec and returns a controllable handle without booting
// a real engine.
type commandRunner interface {
	start(ctx context.Context, spec spawnSpec) (processHandle, error)
}

// execRunner is the production commandRunner: it exec()s the configured nexus
// binary with the per-claim temp config and the broker dial-back env.
type execRunner struct{}

// start launches the instance. The process is intentionally NOT tied to the
// claim request's context — it must outlive the HTTP handler; the broker owns
// its lifecycle via the returned handle.
func (execRunner) start(_ context.Context, spec spawnSpec) (processHandle, error) {
	cmd := buildCommand(spec)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting nexus instance: %w", err)
	}
	return &execProcess{cmd: cmd}, nil
}

// buildCommand constructs the *exec.Cmd for an instance spawn. It is split out
// from start so a unit test can assert the args and env without launching a
// process. The instance is told to read the temp config via -config (matching
// cmd/nexus/main.go) and is handed its dial-back target through the shared
// brokerframe env constants — the single source of truth for these names.
func buildCommand(spec spawnSpec) *exec.Cmd {
	args := []string{"-config", spec.configPath}
	if spec.recallSessionID != "" {
		args = append(args, "-recall", spec.recallSessionID)
	}
	cmd := exec.Command(spec.binaryPath, args...)
	// The spawn secret travels in the ENVIRONMENT rather than in argv on
	// purpose: argv is world-readable through /proc (and `ps`) on the machines
	// this runs on, whereas a process's environment is readable only by its own
	// uid and root. It is injected unconditionally — even when the broker runs
	// without an `auth:` block and therefore does not enforce it — so that
	// turning authentication on is a pure broker-config change and never
	// requires a different spawn path.
	cmd.Env = append(os.Environ(),
		brokerframe.EnvBrokerAddr+"="+spec.brokerAddr,
		brokerframe.EnvLeaseID+"="+spec.leaseID,
		brokerframe.EnvSpawnSecret+"="+spec.spawnSecret,
	)
	// Surface the child's logs through the broker's stderr for observability.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd
}

// newSpawnSecret returns a 128-bit random hex secret for one instance spawn —
// the same width and generator as a lease id (see newLeaseID), because it is
// used for the same purpose: an unguessable bearer value.
//
// It lives beside the spawn plumbing rather than in the registry because it is a
// property of a SPAWN, not of a lease: it is minted once per exec, handed to
// exactly that child, and is meaningless to anything the broker did not start.
// The claim path mints it BEFORE calling the runner so the expected value is
// recorded on the lease no matter which commandRunner is wired — a fake runner
// in a test must not be able to bypass the check by never injecting the env.
func newSpawnSecret() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating spawn secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// execProcess adapts an *exec.Cmd to the processHandle interface.
type execProcess struct {
	cmd *exec.Cmd
}

func (p *execProcess) pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *execProcess) kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("killing instance process: %w", err)
	}
	return nil
}

func (p *execProcess) wait() error {
	return p.cmd.Wait()
}
