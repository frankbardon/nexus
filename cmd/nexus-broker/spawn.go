package main

import (
	"context"
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
	cmd := exec.Command(spec.binaryPath, "-config", spec.configPath)
	cmd.Env = append(os.Environ(),
		brokerframe.EnvBrokerAddr+"="+spec.brokerAddr,
		brokerframe.EnvLeaseID+"="+spec.leaseID,
	)
	// Surface the child's logs through the broker's stderr for observability.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd
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
