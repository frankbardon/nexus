package host_test

import (
	"context"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	_ "github.com/frankbardon/nexus/pkg/engine/sandbox/host"
)

func TestShellEcho(t *testing.T) {
	sb, err := sandbox.New(sandbox.BackendHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindShell,
		Source: []byte("echo hello"),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "hello" {
		t.Fatalf("stdout: got %q, want hello", got)
	}
	if res.Exit != 0 {
		t.Fatalf("exit: got %d", res.Exit)
	}
}

func TestShellAllowedCommands(t *testing.T) {
	sb, err := sandbox.New(sandbox.BackendHost, map[string]any{
		"allowed_commands": []any{"echo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	t.Run("allowed", func(t *testing.T) {
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
			Kind:   sandbox.KindShell,
			Source: []byte("echo ok"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Exit != 0 || !strings.Contains(string(res.Stdout), "ok") {
			t.Fatalf("expected ok exit + stdout, got exit=%d stdout=%q", res.Exit, res.Stdout)
		}
	})

	t.Run("denied", func(t *testing.T) {
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
			Kind:   sandbox.KindShell,
			Source: []byte("rm -rf /"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Exit != 126 {
			t.Fatalf("expected denied exit=126, got %d", res.Exit)
		}
		if !strings.Contains(string(res.Stderr), "command not allowed") {
			t.Fatalf("expected denial message, got %q", res.Stderr)
		}
	})
}

func TestShellTimeout(t *testing.T) {
	sb, err := sandbox.New(sandbox.BackendHost, map[string]any{
		"timeout": "100ms",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindShell,
		Source: []byte("sleep 5"),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true, got %+v", res)
	}
}

func TestShellEnvRestrict(t *testing.T) {
	sb, err := sandbox.New(sandbox.BackendHost, map[string]any{
		"env_restrict": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindShell,
		Source: []byte("echo $HOME"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "/tmp" {
		t.Fatalf("env-restricted HOME: got %q, want /tmp", got)
	}
}

func TestUnsupportedKind(t *testing.T) {
	sb, err := sandbox.New(sandbox.BackendHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	_, err = sb.Exec(context.Background(), sandbox.ExecRequest{
		Kind:   sandbox.KindGoWasm,
		Source: []byte("anything"),
	})
	if err == nil {
		t.Fatal("expected ErrKindUnsupported, got nil")
	}
}
