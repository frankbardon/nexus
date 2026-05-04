package host

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// DefaultCaps is the production capabilities adapter. Performs real network
// fetches (with a finite timeout + body cap), real filesystem reads/writes
// scoped to the configured mounts, and real subprocess invocation.
type DefaultCaps struct {
	Mounts         []FSMount
	HTTPClient     *http.Client
	HTTPMaxBodyMiB int
	ExecTimeout    time.Duration
}

// NewDefaultCaps returns a DefaultCaps with sensible defaults: 30 s HTTP
// client timeout, 16 MiB max body, 30 s exec timeout.
func NewDefaultCaps(mounts []FSMount) *DefaultCaps {
	return &DefaultCaps{
		Mounts:         mounts,
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
		HTTPMaxBodyMiB: 16,
		ExecTimeout:    30 * time.Second,
	}
}

// HTTPGet performs the real HTTP GET. Caller already passed the policy gate.
func (c *DefaultCaps) HTTPGet(ctx context.Context, url string) (int, []byte, map[string][]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	maxBytes := int64(c.HTTPMaxBodyMiB) * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return resp.StatusCode, nil, resp.Header, err
	}
	if int64(len(body)) > maxBytes {
		return resp.StatusCode, nil, resp.Header, fmt.Errorf("response exceeds %d MiB cap", c.HTTPMaxBodyMiB)
	}
	return resp.StatusCode, body, resp.Header, nil
}

// FSReadFile resolves guestPath against configured mounts and reads.
// Symlink-following is suppressed — Lstat first, refuse to dereference any
// symlink whose target leaves the mount tree.
func (c *DefaultCaps) FSReadFile(_ context.Context, guestPath string) ([]byte, error) {
	hostBase, rel := MountForGuest(c.Mounts, guestPath)
	if hostBase == "" {
		return nil, fmt.Errorf("path not under any mount: %s", guestPath)
	}
	abs := filepath.Join(hostBase, rel)
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("symlink follow refused: %s", abs)
	}
	return os.ReadFile(abs)
}

// FSWriteFile writes data to a guest path. Caller has already passed the
// rw-mount gate; this layer just resolves and writes with 0644 perm.
func (c *DefaultCaps) FSWriteFile(_ context.Context, guestPath string, data []byte) error {
	hostBase, rel := MountForGuest(c.Mounts, guestPath)
	if hostBase == "" {
		return fmt.Errorf("path not under any mount: %s", guestPath)
	}
	abs := filepath.Join(hostBase, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, data, 0o644)
}

// ExecRun runs name with args under the host kernel. Wrapped in a hard
// per-call timeout independent of any context the caller passes.
func (c *DefaultCaps) ExecRun(ctx context.Context, name string, args []string) ([]byte, []byte, int, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, c.ExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, name, args...)
	stdoutBuf, err := cmd.Output()
	exitCode := 0
	var stderrBuf []byte
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
		stderrBuf = exitErr.Stderr
		err = nil
	} else if err != nil {
		return nil, nil, 0, err
	}
	return stdoutBuf, stderrBuf, exitCode, nil
}
