//go:build unix

package shell

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/sandbox/host"
	"github.com/frankbardon/nexus/pkg/events"
)

// A shell call that is timed out (tool.timeout naming its call ID) must have
// its process killed — including a grandchild the shell started — rather
// than left running after the agent has been handed an error result.
func TestShell_TimedOutCommandIsKilled(t *testing.T) {
	bus := engine.NewEventBus()
	calls := engine.NewToolCallContexts()
	t.Cleanup(calls.Attach(bus))

	sb, err := host.New(map[string]any{})
	if err != nil {
		t.Fatalf("host sandbox: %v", err)
	}
	p := &Plugin{
		bus:       bus,
		logger:    slog.Default(),
		sandbox:   sb,
		toolCalls: calls,
		timeout:   30 * time.Second,
	}

	results := make(chan events.ToolResult, 1)
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok {
			results <- r
		}
	})

	pidFile := filepath.Join(t.TempDir(), "pid")
	// The backgrounded sleep is a grandchild of the tool call: killing only
	// `sh` would leave it running.
	cmd := "sleep 30 & echo $! > " + pidFile + "; wait"
	go p.handleInvoke(events.ToolCall{SchemaVersion: events.ToolCallVersion,
		ID: "sh-1", Name: "shell", Arguments: map[string]any{"command": cmd}})

	pid := waitForPID(t, pidFile)

	start := time.Now()
	_ = bus.Emit("tool.timeout", events.ToolTimeout{SchemaVersion: events.ToolTimeoutVersion,
		ToolName: "shell", CallID: "sh-1", Timeout: time.Second})

	select {
	case r := <-results:
		if !strings.Contains(r.Error, "cancelled") {
			t.Fatalf("result error = %q, want a cancellation", r.Error)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("shell took %s to return after cancellation", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shell call never returned after tool.timeout")
	}

	waitForExit(t, pid)
	if calls.Live("sh-1") {
		t.Fatal("shell did not release its per-call context")
	}
}

// Cancelling a turn (cancel.active naming it) must kill a long command that
// turn is running — here one dispatched by a nested loop (private TurnID,
// ParentTurnID = the cancelled turn) — the same way a timeout does, while a
// command running for another turn keeps going.
func TestShell_TurnCancelKillsCommand(t *testing.T) {
	bus := engine.NewEventBus()
	calls := engine.NewToolCallContexts()
	t.Cleanup(calls.Attach(bus))

	sb, err := host.New(map[string]any{})
	if err != nil {
		t.Fatalf("host sandbox: %v", err)
	}
	p := &Plugin{
		bus:       bus,
		logger:    slog.Default(),
		sandbox:   sb,
		toolCalls: calls,
		timeout:   30 * time.Second,
	}
	bus.Subscribe("tool.invoke", p.handleEvent, engine.WithPriority(50))

	results := make(chan events.ToolResult, 2)
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok {
			results <- r
		}
	})

	dir := t.TempDir()
	invoke := func(id, turn, parent, pidFile string) {
		cmd := "sleep 30 & echo $! > " + pidFile + "; wait"
		go func() {
			_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion,
				ID: id, Name: "shell", TurnID: turn, ParentTurnID: parent,
				Arguments: map[string]any{"command": cmd}})
		}()
	}
	victimPID := filepath.Join(dir, "victim")
	otherPID := filepath.Join(dir, "other")
	invoke("sh-victim", "delegate_1", "turn-1", victimPID)
	invoke("sh-other", "turn-2", "", otherPID)
	pid := waitForPID(t, victimPID)
	other := waitForPID(t, otherPID)
	t.Cleanup(func() { _ = syscall.Kill(other, syscall.SIGKILL) })

	start := time.Now()
	_ = bus.Emit("cancel.active", events.CancelActive{SchemaVersion: events.CancelActiveVersion, TurnID: "turn-1"})

	select {
	case r := <-results:
		if r.ID != "sh-victim" {
			t.Fatalf("first result is for %q, want sh-victim", r.ID)
		}
		if !strings.Contains(r.Error, "cancelled") {
			t.Fatalf("result error = %q, want a cancellation", r.Error)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("shell took %s to return after cancellation", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shell call never returned after its turn was cancelled")
	}
	waitForExit(t, pid)

	if err := syscall.Kill(other, 0); err != nil {
		t.Fatalf("other turn's command died on turn-1's cancel (kill(0) = %v)", err)
	}
	if !calls.Live("sh-other") {
		t.Fatal("other turn's call context is no longer live")
	}
}

func waitForExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild pid %d still alive after the call was cancelled (kill(0) = %v)", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("command never wrote its pid")
	return 0
}
