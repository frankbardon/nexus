package wasm_test

import (
	"context"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	_ "github.com/frankbardon/nexus/pkg/engine/sandbox/wasm"
)

// sharedBackends caches wasm sandboxes by config-key so tests don't pay the
// ~5–9 s wazero AOT compile cost more than once per distinct config. The
// real engine only creates one backend per plugin per session, so this
// mirrors production reuse rather than gaming the test.
var (
	sharedMu       sync.Mutex
	sharedBackends = map[string]sandbox.Sandbox{}
)

// sharedCacheDir is a wazero compilation cache shared across every wasm
// sandbox built during the test run. The first sandbox pays the ~5–9 s AOT
// compile; subsequent sandboxes (including those with distinct policies)
// load the precompiled module from disk in ~50–200 ms. Without this the
// bridge tests, which need a fresh backend per test for distinct policies,
// each rebuilt from scratch and dominated CI time.
var sharedCacheDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nexus-wasm-test-cache-*")
	if err != nil {
		panic("wasm test cache: " + err.Error())
	}
	sharedCacheDir = dir

	code := m.Run()

	sharedMu.Lock()
	for _, sb := range sharedBackends {
		_ = sb.Close()
	}
	sharedMu.Unlock()
	_ = os.RemoveAll(sharedCacheDir)
	os.Exit(code)
}

// withSharedCache returns a shallow copy of cfg with cache_dir set to the
// shared wazero compilation cache. Tests that build a fresh sandbox per
// case use this so they amortize AOT compile across the package.
func withSharedCache(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg)+1)
	maps.Copy(out, cfg)
	out["cache_dir"] = sharedCacheDir
	return out
}

// helloScript prints to stdout and returns a string. Exercises both the
// user-output channel and the result envelope.
const helloScript = `package main

import (
	"context"
	"fmt"
)

func Run(ctx context.Context) (any, error) {
	fmt.Println("hi from wasm")
	return "ok", nil
}
`

const computeScript = `package main

import "context"

func Run(ctx context.Context) (any, error) {
	sum := 0
	for i := 1; i <= 10; i++ {
		sum += i
	}
	return sum, nil
}
`

const errorScript = `package main

import (
	"context"
	"fmt"
)

func Run(ctx context.Context) (any, error) {
	return nil, fmt.Errorf("boom")
}
`

func newWasm(t *testing.T, cfg map[string]any) sandbox.Sandbox {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{
			"allowed_packages": []any{"fmt", "strings", "context"},
		}
	}
	key := mapKey(cfg)
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sb, ok := sharedBackends[key]; ok {
		return sb
	}
	sb, err := sandbox.New(sandbox.BackendWasm, withSharedCache(cfg))
	if err != nil {
		t.Fatalf("new wasm: %v", err)
	}
	sharedBackends[key] = sb
	return sb
}

func mapKey(cfg map[string]any) string {
	var b strings.Builder
	for k, v := range cfg {
		b.WriteString(k)
		b.WriteByte('=')
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				b.WriteString(toString(e))
				b.WriteByte(',')
			}
		default:
			b.WriteString(toString(v))
		}
		b.WriteByte(';')
	}
	return b.String()
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}

func TestWasmHello(t *testing.T) {
	sb := newWasm(t, nil)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindGoWasm,
		Source: []byte(helloScript),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	out := string(res.Stdout)
	if !strings.Contains(out, "hi from wasm") {
		t.Fatalf("missing user output, got %q", out)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("missing result, got %q", out)
	}
}

func TestWasmCompute(t *testing.T) {
	sb := newWasm(t, nil)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindGoWasm,
		Source: []byte(computeScript),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "55") {
		t.Fatalf("expected 55 in stdout, got %q", res.Stdout)
	}
}

func TestWasmError(t *testing.T) {
	sb := newWasm(t, nil)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindGoWasm,
		Source: []byte(errorScript),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(res.Stderr), "boom") {
		t.Fatalf("expected error 'boom' in stderr, got %q", res.Stderr)
	}
}

func TestWasmTimeout(t *testing.T) {
	sb := newWasm(t, map[string]any{
		"allowed_packages": []any{"fmt", "strings", "context", "time"},
	})
	src := `package main
import (
	"context"
	"time"
)
func Run(ctx context.Context) (any, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50*time.Millisecond):
		}
	}
}
`
	start := time.Now()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:    sandbox.KindGoWasm,
		Source:  []byte(src),
		Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	elapsed := time.Since(start)
	if !res.TimedOut && !strings.Contains(string(res.Stderr), "context") {
		t.Fatalf("expected timeout signal, got TimedOut=%v stderr=%q", res.TimedOut, res.Stderr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout ineffective: %s elapsed", elapsed)
	}
}

func TestWasmRejectsKindShell(t *testing.T) {
	sb := newWasm(t, nil)
	_, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindShell,
		Source: []byte("echo hi"),
	})
	if err == nil {
		t.Fatal("expected ErrKindUnsupported")
	}
}
