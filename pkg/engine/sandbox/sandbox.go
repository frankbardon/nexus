// Package sandbox is the engine-wide execution-isolation abstraction.
// Backends — host, wasm, and future tiers like gVisor or Firecracker —
// implement Sandbox and are selected per-plugin via config.
//
// Tools that shell out (`tools/shell`) or execute agent-emitted code
// (`tools/codeexec`) call the sandbox they receive via PluginContext rather
// than reaching directly for `os/exec` or an in-process interpreter. This
// keeps the kernel surface a single audited boundary and lets the same plugin
// run unchanged under any installed isolation tier.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Backend is the registered name of a sandbox implementation. The engine
// resolves the configured name to a factory at boot.
type Backend string

const (
	BackendHost Backend = "host"
	BackendWasm Backend = "wasm"
)

// Kind selects the execution shape inside the request. Backends accept a
// subset of kinds; mismatches return ErrKindUnsupported at Exec time.
type Kind string

const (
	// KindShell runs a shell command line against the host kernel via the
	// configured shell. Honored by the host backend; rejected by wasm.
	KindShell Kind = "shell"

	// KindGoYaegi evaluates Go source through Yaegi inside the host process.
	// Honored by the host backend.
	KindGoYaegi Kind = "go_yaegi"

	// KindGoWasm evaluates Go source through an embedded Yaegi-on-Wasm
	// runner. Honored by the wasm backend.
	KindGoWasm Kind = "go_wasm"
)

// NetPolicy describes how the sandbox should treat network egress for a call.
// Backends that do not gate network return ErrCapabilityNotSupported when
// asked to enforce a non-default policy.
type NetPolicy struct {
	// Mode is one of "deny" (default), "allow_hosts", or "allow_all".
	Mode string
	// AllowHosts is the exact-match hostname allowlist consulted when Mode
	// is "allow_hosts". Empty under any other mode.
	AllowHosts []string
}

// Mount declares a host→guest filesystem binding visible to the executed
// payload. Mode is "ro" (read-only) or "rw".
type Mount struct {
	Host  string
	Guest string
	Mode  string
}

// ExecRequest is the input to Sandbox.Exec. Source carries the payload —
// shell command for KindShell, Go source for KindGoYaegi/KindGoWasm.
type ExecRequest struct {
	Kind    Kind
	Source  []byte
	Args    []string
	Stdin   []byte
	Env     map[string]string
	WorkDir string
	Mounts  []Mount
	Net     NetPolicy
	Timeout time.Duration

	// AllowedPackages narrows the stdlib whitelist visible to a Go-source
	// payload (KindGoYaegi / KindGoWasm). Backends apply it as an override
	// on top of any backend-level default. Ignored for KindShell.
	AllowedPackages []string

	// Backend lets a per-call override pick a non-default backend for the
	// same plugin. Empty means "use the configured default".
	Backend Backend
}

// ExecResult is the output of Sandbox.Exec. Stdout and Stderr are captured
// in full up to the backend's configured cap; over-cap output sets
// Truncated.
type ExecResult struct {
	Stdout    []byte
	Stderr    []byte
	Exit      int
	TimedOut  bool
	Truncated bool
	Usage     UsageStats
}

// UsageStats reports observable resource consumption for a single Exec.
// Backends fill the fields they can measure; zero is "unknown".
type UsageStats struct {
	Wall       time.Duration
	CPUUser    time.Duration
	CPUSystem  time.Duration
	MaxRSSMiB  int64
	OutputSize int64
}

// CapabilitySet describes what a backend can enforce. Used at boot to
// validate that the configuration's expectations match the backend's reach
// (e.g., a config that requests `net.policy: allow_hosts` against a backend
// whose CapabilitySet returns NetGated=false fails Init loudly).
type CapabilitySet struct {
	Kinds       []Kind
	NetGated    bool
	FSGated     bool
	ProcessGate bool
}

// Sandbox is the interface every backend implements.
type Sandbox interface {
	// Exec runs req synchronously and returns the captured result. Returning
	// a non-nil error means the sandbox itself failed (timeout, missing
	// dependency, capability denied); a non-zero ExecResult.Exit means the
	// payload itself exited non-zero, which is not an error.
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)

	// Capabilities reports the set the backend can enforce for the lifetime
	// of this Sandbox instance.
	Capabilities() CapabilitySet

	// Close releases any resources (subprocess pools, wazero runtimes, etc.).
	Close() error
}

// Errors returned by sandbox implementations.
var (
	ErrKindUnsupported        = errors.New("sandbox: kind not supported by backend")
	ErrCapabilityNotSupported = errors.New("sandbox: capability not supported by backend")
	ErrCapabilityDenied       = errors.New("sandbox: capability denied")
	ErrTimeout                = errors.New("sandbox: execution timed out")
	ErrBackendNotConfigured   = errors.New("sandbox: backend not configured")
	ErrUnknownBackend         = errors.New("sandbox: unknown backend")
	ErrMissingDependency      = errors.New("sandbox: backend dependency missing")
)

// Wrap returns a sandbox-prefixed error wrapping cause for context.
func Wrap(action string, cause error) error {
	return fmt.Errorf("sandbox: %s: %w", action, cause)
}
