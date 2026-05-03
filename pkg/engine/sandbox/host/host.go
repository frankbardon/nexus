// Package host is the trivially-isolated sandbox backend: shell commands run
// directly via os/exec against the host kernel. Useful as the fallback for
// `tools/shell` until a stricter backend (gVisor / Firecracker / landlock)
// lands. Honors `allowed_commands`, `working_dir`, `path_dirs`, and the
// legacy env-restriction flag.
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/sandbox"
)

func init() {
	sandbox.Register(sandbox.BackendHost, New)
}

// Backend implements sandbox.Sandbox for the host process.
type Backend struct {
	allowedCommands []string
	workingDir      string
	pathDirs        []string
	defaultTimeout  time.Duration
	envRestrict     bool
}

// New builds a Backend from the per-plugin sandbox config block.
//
// Recognised keys (all optional):
//
//	allowed_commands: ["git", "npm"]   — empty/absent = unrestricted
//	working_dir: "~/.nexus/work"        — passed through ExpandPath at the
//	                                       caller; this layer takes the
//	                                       string verbatim.
//	path_dirs: ["/opt/bin"]             — prepended to PATH at exec time.
//	timeout: "30s"                      — default per-Exec deadline.
//	env_restrict: true                  — clear env to a minimal allowlist.
func New(cfg map[string]any) (sandbox.Sandbox, error) {
	b := &Backend{defaultTimeout: 30 * time.Second}

	if raw, ok := cfg["allowed_commands"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				b.allowedCommands = append(b.allowedCommands, s)
			}
		}
	}
	if s, ok := cfg["working_dir"].(string); ok {
		b.workingDir = s
	}
	if raw, ok := cfg["path_dirs"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				b.pathDirs = append(b.pathDirs, s)
			}
		}
	}
	if s, ok := cfg["timeout"].(string); ok && s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("host sandbox: invalid timeout %q: %w", s, err)
		}
		b.defaultTimeout = d
	}
	if v, ok := cfg["env_restrict"].(bool); ok {
		b.envRestrict = v
	}
	return b, nil
}

// Capabilities reports what the host backend can enforce.
func (b *Backend) Capabilities() sandbox.CapabilitySet {
	return sandbox.CapabilitySet{
		Kinds:       []sandbox.Kind{sandbox.KindShell},
		NetGated:    false,
		FSGated:     false,
		ProcessGate: false,
	}
}

// Close is a no-op for the host backend.
func (b *Backend) Close() error { return nil }

// Exec dispatches the request. v1 supports KindShell only.
func (b *Backend) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	switch req.Kind {
	case sandbox.KindShell:
		return b.execShell(ctx, req)
	default:
		return sandbox.ExecResult{}, fmt.Errorf("%w: %s", sandbox.ErrKindUnsupported, req.Kind)
	}
}

func (b *Backend) execShell(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	command := strings.TrimRight(string(req.Source), "\n")
	if command == "" {
		return sandbox.ExecResult{}, errors.New("host sandbox: empty shell command")
	}
	if !b.commandAllowed(command) {
		return sandbox.ExecResult{
			Exit:   126,
			Stderr: fmt.Appendf(nil, "command not allowed: %s\n", command),
		}, nil
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = b.defaultTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)

	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	} else if b.workingDir != "" {
		cmd.Dir = b.workingDir
	}

	if b.envRestrict {
		cmd.Env = []string{
			"PATH=/usr/bin:/bin",
			"HOME=/tmp",
			"LANG=en_US.UTF-8",
		}
	}
	if len(b.pathDirs) > 0 {
		if cmd.Env == nil {
			cmd.Env = cmd.Environ()
		}
		extra := strings.Join(b.pathDirs, ":")
		cmd.Env = applyPathPrefix(cmd.Env, extra)
	}
	if len(req.Env) > 0 {
		if cmd.Env == nil {
			cmd.Env = cmd.Environ()
		}
		for k, v := range req.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	res := sandbox.ExecResult{
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
		Usage:  sandbox.UsageStats{Wall: elapsed, OutputSize: int64(stdout.Len() + stderr.Len())},
	}
	if cmd.ProcessState != nil {
		res.Exit = cmd.ProcessState.ExitCode()
	}
	if cmdCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, nil
	}
	if runErr != nil {
		// Non-zero exit lands here without ctx error; keep res, propagate
		// nothing — the caller distinguishes via Exit.
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return res, fmt.Errorf("host sandbox: exec failed: %w", runErr)
		}
	}
	return res, nil
}

func (b *Backend) commandAllowed(command string) bool {
	if len(b.allowedCommands) == 0 {
		return true
	}
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return false
	}
	return slices.Contains(b.allowedCommands, parts[0])
}

func applyPathPrefix(env []string, extra string) []string {
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			env[i] = "PATH=" + extra + ":" + e[len("PATH="):]
			return env
		}
	}
	return append(env, "PATH="+extra)
}
