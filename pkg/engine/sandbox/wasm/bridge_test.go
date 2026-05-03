package wasm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	_ "github.com/frankbardon/nexus/pkg/engine/sandbox/wasm"
)

const httpScript = `package main

import (
	"context"
	"fmt"
	nhttp "nexus_sdk/http"
)

func Run(ctx context.Context) (any, error) {
	resp, err := nhttp.Get("__URL__")
	if err != nil {
		return nil, err
	}
	fmt.Printf("status=%d body=%s\n", resp.Status, string(resp.Body))
	return resp.Status, nil
}
`

const httpDeniedScript = `package main

import (
	"context"
	nhttp "nexus_sdk/http"
)

func Run(ctx context.Context) (any, error) {
	_, err := nhttp.Get("https://blocked.example/x")
	if err != nil {
		if nhttp.IsCapDenied(err) {
			return "denied", nil
		}
		return nil, err
	}
	return "ok", nil
}
`

const fsScript = `package main

import (
	"context"
	"fmt"
	nfs "nexus_sdk/fs"
)

func Run(ctx context.Context) (any, error) {
	data, err := nfs.ReadFile("/work/hello.txt")
	if err != nil {
		return nil, err
	}
	fmt.Printf("read=%s\n", string(data))
	return len(data), nil
}
`

const envScript = `package main

import (
	"context"
	"fmt"
	nenv "nexus_sdk/env"
)

func Run(ctx context.Context) (any, error) {
	v := nenv.Get("FOO")
	fmt.Println("FOO=" + v)
	return v, nil
}
`

func TestBridgeHTTPGetAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello-from-host"))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}

	cfg := map[string]any{
		"allowed_packages": []any{"context", "fmt"},
		"net": map[string]any{
			"allow_hosts": []any{host},
		},
	}
	sb, err := sandbox.New(sandbox.BackendWasm, withSharedCache(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	src := strings.ReplaceAll(httpScript, "__URL__", srv.URL)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindGoWasm,
		Source: []byte(src),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	out := string(res.Stdout)
	if !strings.Contains(out, "status=200") {
		t.Fatalf("expected status=200, got %q", out)
	}
	if !strings.Contains(out, "hello-from-host") {
		t.Fatalf("expected body, got %q", out)
	}
}

func TestBridgeHTTPGetDenied(t *testing.T) {
	cfg := map[string]any{
		"allowed_packages": []any{"context"},
		// No net.allow_hosts → deny all.
	}
	sb, err := sandbox.New(sandbox.BackendWasm, withSharedCache(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindGoWasm,
		Source: []byte(httpDeniedScript),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "denied") {
		t.Fatalf("expected 'denied' in stdout, got %q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestBridgeFSReadInsideMount(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hello.txt")
	if err := writeFile(target, "fs-hello"); err != nil {
		t.Fatal(err)
	}

	cfg := map[string]any{
		"allowed_packages": []any{"context", "fmt"},
		"fs_mounts": []any{
			map[string]any{"host": dir, "guest": "/work", "mode": "ro"},
		},
	}
	sb, err := sandbox.New(sandbox.BackendWasm, withSharedCache(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindGoWasm,
		Source: []byte(fsScript),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "read=fs-hello") {
		t.Fatalf("expected fs read, got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestBridgeEnvGet(t *testing.T) {
	cfg := map[string]any{
		"allowed_packages": []any{"context", "fmt"},
		"env": map[string]any{
			"FOO": "bar",
		},
	}
	sb, err := sandbox.New(sandbox.BackendWasm, withSharedCache(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindGoWasm,
		Source: []byte(envScript),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "FOO=bar") {
		t.Fatalf("expected FOO=bar, got %q", res.Stdout)
	}
}

func writeFile(path, data string) error {
	return os.WriteFile(path, []byte(data), 0o644)
}
