// Package wasm is the wazero-backed sandbox for agent-generated Go snippets.
// Boots an embedded Yaegi runner compiled for GOOS=wasip1; the host instantiates
// a fresh module per call and feeds the snippet source through stdin. Result
// envelope arrives on stdout after a sentinel that separates it from any
// user-emitted output.
package wasm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	wasi "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	"github.com/frankbardon/nexus/pkg/engine/sandbox/wasm/host"
)

// expandHome resolves a leading ~ to the user's home dir. Mirrors
// engine.ExpandPath but is reimplemented here to keep the sandbox/wasm
// package free of `pkg/engine` import cycles.
func expandHome(p string) (string, error) {
	if p == "" {
		return p, nil
	}
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	rest := strings.TrimPrefix(p, "~")
	rest = strings.TrimPrefix(rest, string(filepath.Separator))
	return filepath.Join(home, rest), nil
}

const (
	resultSentinel = "\n---NEXUS-RUNNER-RESULT-V1---\n"
	defaultTimeout = 30 * time.Second
	defaultMaxOut  = 64 * 1024
)

func init() {
	sandbox.Register(sandbox.BackendWasm, newBackend)
}

// Backend implements sandbox.Sandbox by routing Go snippets through an
// embedded Yaegi-on-Wasm runner. Holds one wazero.Runtime + compiled module
// for the life of the backend; per-Exec instantiates a fresh wasm module so
// snippets cannot share state.
type Backend struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	bridge   *host.Bridge

	allowedPackages []string
	timeout         time.Duration
	maxOutputBytes  int

	closed sync.Once
}

func newBackend(cfg map[string]any) (sandbox.Sandbox, error) {
	bytesWasm, err := runnerBytes()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", sandbox.ErrMissingDependency, err)
	}

	ctx := context.Background()
	rtCfg := wazero.NewRuntimeConfig()
	if dir, ok := cfg["cache_dir"].(string); ok && dir != "" {
		expanded, expErr := expandHome(dir)
		if expErr != nil {
			return nil, fmt.Errorf("wasm sandbox: cache_dir: %w", expErr)
		}
		cache, cErr := wazero.NewCompilationCacheWithDir(expanded)
		if cErr != nil {
			return nil, fmt.Errorf("wasm sandbox: open compilation cache %q: %w", expanded, cErr)
		}
		rtCfg = rtCfg.WithCompilationCache(cache)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)
	if _, err := wasi.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("wasm: install wasi: %w", err)
	}

	policy, err := parsePolicy(cfg)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	bridge := host.NewBridge(policy, host.NewDefaultCaps(policy.FSMounts))
	if err := bridge.Register(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("wasm: register bridge: %w", err)
	}

	compiled, err := rt.CompileModule(ctx, bytesWasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("wasm: compile runner: %w", err)
	}

	b := &Backend{
		runtime:        rt,
		compiled:       compiled,
		bridge:         bridge,
		timeout:        defaultTimeout,
		maxOutputBytes: defaultMaxOut,
	}

	if raw, ok := cfg["allowed_packages"].([]any); ok {
		for _, e := range raw {
			if s, ok := e.(string); ok && s != "" {
				b.allowedPackages = append(b.allowedPackages, s)
			}
		}
	}
	if s, ok := cfg["timeout"].(string); ok && s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			_ = rt.Close(ctx)
			return nil, fmt.Errorf("wasm sandbox: invalid timeout %q: %w", s, err)
		}
		b.timeout = d
	}
	if v, ok := intLike(cfg["max_output_bytes"]); ok && v > 0 {
		b.maxOutputBytes = v
	}

	return b, nil
}

// Capabilities reports the wasm backend's enforcement reach.
func (b *Backend) Capabilities() sandbox.CapabilitySet {
	return sandbox.CapabilitySet{
		Kinds:       []sandbox.Kind{sandbox.KindGoWasm},
		NetGated:    true,
		FSGated:     true,
		ProcessGate: true,
	}
}

// Close releases the wazero runtime + compiled module.
func (b *Backend) Close() error {
	var err error
	b.closed.Do(func() {
		ctx := context.Background()
		if b.compiled != nil {
			_ = b.compiled.Close(ctx)
		}
		if b.runtime != nil {
			err = b.runtime.Close(ctx)
		}
	})
	return err
}

// Exec runs the snippet inside a fresh wasm instance.
func (b *Backend) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if req.Kind != sandbox.KindGoWasm {
		return sandbox.ExecResult{}, fmt.Errorf("%w: %s", sandbox.ErrKindUnsupported, req.Kind)
	}
	if len(req.Source) == 0 {
		return sandbox.ExecResult{}, errors.New("wasm sandbox: empty source")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = b.timeout
	}
	allowed := b.allowedPackages

	reqJSON, err := json.Marshal(map[string]any{
		"source":           string(req.Source),
		"allowed_packages": allowed,
		"timeout_seconds":  int(timeout.Seconds()),
	})
	if err != nil {
		return sandbox.ExecResult{}, fmt.Errorf("wasm sandbox: marshal request: %w", err)
	}

	stdoutBuf := newCapBuf(b.maxOutputBytes)
	stderrBuf := newCapBuf(b.maxOutputBytes)
	moduleCfg := wazero.NewModuleConfig().
		WithStdin(bytes.NewReader(reqJSON)).
		WithStdout(stdoutBuf).
		WithStderr(stderrBuf).
		WithName("")

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	_, runErr := b.runtime.InstantiateModule(cmdCtx, b.compiled, moduleCfg)
	elapsed := time.Since(started)

	res := sandbox.ExecResult{
		Truncated: stdoutBuf.truncated || stderrBuf.truncated,
		Usage:     sandbox.UsageStats{Wall: elapsed, OutputSize: int64(stdoutBuf.Len() + stderrBuf.Len())},
	}

	if cmdCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.Stdout = stdoutBuf.Bytes()
		res.Stderr = stderrBuf.Bytes()
		return res, nil
	}

	stdoutAll := stdoutBuf.Bytes()
	idx := bytes.LastIndex(stdoutAll, []byte(resultSentinel))
	if idx < 0 {
		// No envelope written. Use exit code as the diagnostic.
		res.Stdout = stdoutAll
		res.Stderr = stderrBuf.Bytes()
		var exitErr *sys.ExitError
		if errors.As(runErr, &exitErr) {
			res.Exit = int(exitErr.ExitCode())
			return res, nil
		}
		if runErr != nil {
			return res, fmt.Errorf("wasm sandbox: runner: %w", runErr)
		}
		return res, fmt.Errorf("wasm sandbox: runner produced no result envelope")
	}

	userStdout := stdoutAll[:idx]
	envelope := stdoutAll[idx+len(resultSentinel):]
	res.Stdout = bytes.TrimRight(userStdout, "\n")
	res.Stderr = stderrBuf.Bytes()

	var resp struct {
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(envelope), &resp); err != nil {
		return res, fmt.Errorf("wasm sandbox: parse envelope: %w", err)
	}

	var exitErr *sys.ExitError
	if errors.As(runErr, &exitErr) {
		res.Exit = int(exitErr.ExitCode())
	}
	if resp.Error != "" {
		// Surface the runner's own error in stderr so callers see it as
		// part of the diagnostic stream, alongside any wasm-side stderr.
		if len(res.Stderr) > 0 && res.Stderr[len(res.Stderr)-1] != '\n' {
			res.Stderr = append(res.Stderr, '\n')
		}
		res.Stderr = append(res.Stderr, []byte(resp.Error)...)
	}
	if resp.Result != "" {
		// Append the result text to user stdout so callers that just want a
		// value from `Run` see it without parsing the envelope themselves.
		if len(res.Stdout) > 0 && res.Stdout[len(res.Stdout)-1] != '\n' {
			res.Stdout = append(res.Stdout, '\n')
		}
		res.Stdout = append(res.Stdout, []byte(resp.Result)...)
	}
	return res, nil
}

func intLike(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}
