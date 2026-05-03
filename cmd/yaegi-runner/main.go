//go:build wasip1

// Command yaegi-runner is the embedded Yaegi-inside-Wasm interpreter that
// backs the wasm sandbox. It is compiled with GOOS=wasip1 GOARCH=wasm and
// embedded into the engine binary via //go:embed in
// pkg/engine/sandbox/wasm. wazero instantiates one fresh module per snippet
// invocation; the host writes a JSON request to stdin and reads the result
// envelope from stdout, separated from any user-emitted stdout by a unique
// sentinel.
//
// Wire protocol (v1):
//
//	stdin  → JSON request: {"source", "allowed_packages", "timeout_seconds"}
//	stdout → user output emitted by the snippet, followed by:
//	         "\n---NEXUS-RUNNER-RESULT-V1---\n" + JSON response
//	         {"result", "error"}
//	stderr → user stderr output emitted by the snippet
//	exit   → 0 on success, 1 on any host-level failure (parse, timeout, etc.)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

const sentinel = "\n---NEXUS-RUNNER-RESULT-V1---\n"

type request struct {
	Source          string   `json:"source"`
	AllowedPackages []string `json:"allowed_packages"`
	TimeoutSeconds  int      `json:"timeout_seconds"`
}

type response struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func main() {
	if err := run(); err != nil {
		emit(response{Error: err.Error()})
		os.Exit(1)
	}
}

func run() (err error) {
	// Yaegi panics on certain malformed snippets (e.g., a script that
	// references a type the bindings haven't fully registered). Convert to
	// a structured error so the host sees an envelope instead of a missing-
	// envelope diagnostic from a wasm process that died mid-eval.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("yaegi panic: %v", r)
		}
	}()

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	if req.Source == "" {
		return fmt.Errorf("source is empty")
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	i := interp.New(interp.Options{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})

	if err := i.Use(filteredStdlib(req.AllowedPackages)); err != nil {
		return fmt.Errorf("install stdlib: %w", err)
	}
	if err := i.Use(sdkBindings()); err != nil {
		return fmt.Errorf("install nexus_sdk: %w", err)
	}

	if _, err := i.Eval(req.Source); err != nil {
		return fmt.Errorf("compile: %w", err)
	}

	runVal, err := i.Eval("main.Run")
	if err != nil {
		return fmt.Errorf("resolve main.Run: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	rv := reflect.ValueOf(runVal.Interface())
	if rv.Kind() != reflect.Func {
		return fmt.Errorf("main.Run is not a function")
	}
	out := rv.Call([]reflect.Value{reflect.ValueOf(ctx)})
	if len(out) != 2 {
		return fmt.Errorf("main.Run returned %d values, want 2", len(out))
	}
	if errVal := out[1].Interface(); errVal != nil {
		if e, ok := errVal.(error); ok {
			return fmt.Errorf("runtime: %w", e)
		}
		return fmt.Errorf("runtime: %v", errVal)
	}

	emit(response{Result: fmt.Sprintf("%v", out[0].Interface())})
	return nil
}

func emit(r response) {
	enc, err := json.Marshal(r)
	if err != nil {
		// Fallback: write a parseable envelope by hand. Should never happen
		// since response only contains string fields.
		fmt.Fprint(os.Stdout, sentinel)
		fmt.Fprintf(os.Stdout, `{"error":"emit marshal: %s"}`, err)
		return
	}
	fmt.Fprint(os.Stdout, sentinel)
	os.Stdout.Write(enc)
}

// filteredStdlib mirrors plugins/tools/codeexec/stdlib.go. Kept inline to
// keep the runner package self-contained — it must compile cleanly under
// GOOS=wasip1 without dragging the engine module's plugins/* tree.
//
// Entries that name nexus_sdk/* paths are filtered out: those packages are
// installed by sdkBindings(), not by stdlib.Symbols. Including them here
// would be a no-op since stdlib.Symbols won't have a key for them, but
// silently dropping them keeps a config that lists nexus_sdk imports under
// allowed_packages from looking like a misconfiguration.
func filteredStdlib(allowed []string) interp.Exports {
	if len(allowed) == 0 {
		return interp.Exports{}
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, p := range allowed {
		if strings.HasPrefix(p, "nexus_sdk/") {
			continue
		}
		allowedSet[p] = true
	}
	out := interp.Exports{}
	for key, syms := range stdlib.Symbols {
		// yaegi keys are "importpath/pkgname"; trim the trailing /pkgname.
		importPath := key
		if idx := strings.LastIndex(key, "/"); idx >= 0 {
			importPath = key[:idx]
		}
		if !allowedSet[importPath] {
			continue
		}
		dup := make(map[string]reflect.Value, len(syms))
		for k, v := range syms {
			dup[k] = v
		}
		out[key] = dup
	}
	return out
}
