package codeexec

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// collectStdout installs a code.exec.stdout subscriber on the harness bus and
// returns a snapshot accessor. Keeps ordering stable for assertions.
func collectStdout(t *testing.T, h *testHarness) func() []events.CodeExecStdout {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []events.CodeExecStdout
	)
	h.bus.Subscribe("code.exec.stdout", func(e engine.Event[any]) {
		s, ok := e.Payload.(events.CodeExecStdout)
		if !ok {
			return
		}
		mu.Lock()
		seen = append(seen, s)
		mu.Unlock()
	}, engine.WithPriority(95), engine.WithSource("test-stdout-collector"))
	return func() []events.CodeExecStdout {
		mu.Lock()
		defer mu.Unlock()
		out := make([]events.CodeExecStdout, len(seen))
		copy(out, seen)
		return out
	}
}

// TestStreamingWriter_FlushesOnNewline exercises the writer directly — no
// Yaegi involved — to confirm newline-triggered emission.
func TestStreamingWriter_FlushesOnNewline(t *testing.T) {
	bus := engine.NewEventBus()
	var got []events.CodeExecStdout
	var mu sync.Mutex
	bus.Subscribe("code.exec.stdout", func(e engine.Event[any]) {
		mu.Lock()
		got = append(got, e.Payload.(events.CodeExecStdout))
		mu.Unlock()
	}, engine.WithPriority(50), engine.WithSource("t"))

	w := newStreamingWriter(bus, "c-1", "t-1", 1024)

	// Two newlines => two flushed chunks.
	_, _ = w.Write([]byte("hello\n"))
	_, _ = w.Write([]byte("world\n"))
	// Unterminated tail — should remain buffered until Close.
	_, _ = w.Write([]byte("tail"))

	mu.Lock()
	midCount := len(got)
	mu.Unlock()
	if midCount != 2 {
		t.Fatalf("want 2 mid-stream chunks before Close, got %d", midCount)
	}

	w.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("want 3 chunks total after Close, got %d", len(got))
	}
	if got[0].Chunk != "hello\n" || got[1].Chunk != "world\n" {
		t.Errorf("chunk contents wrong: %+v", got[:2])
	}
	if !got[2].Final || got[2].Chunk != "tail" {
		t.Errorf("final chunk wrong: %+v", got[2])
	}
	if got[0].Final || got[1].Final {
		t.Errorf("mid-stream chunks must not have Final=true")
	}
	if got[2].Truncated {
		t.Errorf("truncated should be false when under cap")
	}
	if w.String() != "hello\nworld\ntail" {
		t.Errorf("aggregate wrong: %q", w.String())
	}
}

// TestStreamingWriter_HonorsByteCap proves bytes past max are dropped and the
// final chunk carries Truncated=true.
func TestStreamingWriter_HonorsByteCap(t *testing.T) {
	bus := engine.NewEventBus()
	var got []events.CodeExecStdout
	var mu sync.Mutex
	bus.Subscribe("code.exec.stdout", func(e engine.Event[any]) {
		mu.Lock()
		got = append(got, e.Payload.(events.CodeExecStdout))
		mu.Unlock()
	}, engine.WithPriority(50), engine.WithSource("t"))

	w := newStreamingWriter(bus, "c-cap", "t-cap", 8)
	_, _ = w.Write([]byte("aaaa\n"))     // 5 bytes accepted, flushed on newline
	_, _ = w.Write([]byte("bbbbcccc\n")) // only 3 more fit; rest dropped
	w.Close()

	mu.Lock()
	defer mu.Unlock()
	if !got[len(got)-1].Final {
		t.Fatalf("last chunk must be Final=true")
	}
	if !got[len(got)-1].Truncated {
		t.Fatalf("last chunk must carry Truncated=true when cap reached")
	}
	if got := w.String(); got != "aaaa\nbbb" {
		t.Errorf("aggregate should contain accepted bytes only; got %q", got)
	}
}

// TestStreamingWriter_ForcesFlushOnLongLine confirms the threshold pushes
// output out even without a newline.
func TestStreamingWriter_ForcesFlushOnLongLine(t *testing.T) {
	bus := engine.NewEventBus()
	var got []events.CodeExecStdout
	var mu sync.Mutex
	bus.Subscribe("code.exec.stdout", func(e engine.Event[any]) {
		mu.Lock()
		got = append(got, e.Payload.(events.CodeExecStdout))
		mu.Unlock()
	}, engine.WithPriority(50), engine.WithSource("t"))

	w := newStreamingWriter(bus, "c-long", "t-long", 4096)
	long := strings.Repeat("x", streamFlushThreshold+100)
	_, _ = w.Write([]byte(long))

	mu.Lock()
	midCount := len(got)
	mu.Unlock()
	if midCount == 0 {
		t.Fatal("expected threshold-triggered flush before Close")
	}
	w.Close()
}

// TestPlugin_EmitsStdoutStream runs a real script that prints twice and
// verifies both mid-stream events and a final chunk arrive — proving the
// writer is correctly plumbed through Yaegi.
func TestPlugin_EmitsStdoutStream(t *testing.T) {
	h := newHarness(t, nil)
	h.registerFakeTool()
	snapshot := collectStdout(t, h)

	script := `package main

import (
	"context"
	"fmt"
)

func Run(ctx context.Context) (any, error) {
	fmt.Println("line-one")
	fmt.Println("line-two")
	return "done", nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("script error: %s", res.Error)
	}

	chunks := snapshot()
	if len(chunks) < 2 {
		t.Fatalf("want at least 2 stdout chunks, got %d: %+v", len(chunks), chunks)
	}

	// Assemble and compare — decouples test from exact chunking policy.
	var joined strings.Builder
	var finalSeen bool
	for _, c := range chunks {
		if c.CallID != res.ID {
			t.Errorf("chunk CallID=%q, want %q", c.CallID, res.ID)
		}
		joined.WriteString(c.Chunk)
		if c.Final {
			finalSeen = true
		}
	}
	if !finalSeen {
		t.Error("no chunk marked Final=true")
	}
	if !strings.Contains(joined.String(), "line-one") || !strings.Contains(joined.String(), "line-two") {
		t.Errorf("joined stream missing content: %q", joined.String())
	}

	// Also the old aggregated Output still lands on the envelope.
	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	if got, _ := env["stdout"].(string); !strings.Contains(got, "line-one") {
		t.Errorf("aggregate stdout missing content: %q", got)
	}
}

// TestPlugin_StreamsBeforeFinalResult exercises the invariant that stdout
// events arrive before the corresponding code.exec.result — i.e. the LLM /
// UI isn't forced to wait for script completion to show output.
func TestPlugin_StreamsBeforeFinalResult(t *testing.T) {
	h := newHarness(t, nil)

	var (
		mu        sync.Mutex
		ordering  []string
		firstTime time.Time
	)
	h.bus.Subscribe("code.exec.stdout", func(e engine.Event[any]) {
		s := e.Payload.(events.CodeExecStdout)
		mu.Lock()
		defer mu.Unlock()
		if firstTime.IsZero() {
			firstTime = time.Now()
		}
		if s.Final {
			ordering = append(ordering, "stdout:final")
		} else {
			ordering = append(ordering, "stdout:chunk")
		}
	}, engine.WithPriority(95), engine.WithSource("t-order"))
	h.bus.Subscribe("code.exec.result", func(_ engine.Event[any]) {
		mu.Lock()
		defer mu.Unlock()
		ordering = append(ordering, "result")
	}, engine.WithPriority(95), engine.WithSource("t-order"))

	script := `package main

import (
	"context"
	"fmt"
)

func Run(ctx context.Context) (any, error) {
	fmt.Println("alpha")
	fmt.Println("beta")
	return nil, nil
}
`
	_ = h.runCode(script)

	mu.Lock()
	defer mu.Unlock()
	if len(ordering) == 0 || ordering[len(ordering)-1] != "result" {
		t.Fatalf("code.exec.result must arrive last; got %v", ordering)
	}
	var sawChunk bool
	for i, ev := range ordering {
		if ev == "stdout:chunk" && !sawChunk {
			sawChunk = true
		}
		if ev == "result" && i < len(ordering)-1 {
			t.Fatalf("result arrived before ordering finished: %v", ordering)
		}
	}
	if !sawChunk {
		t.Errorf("no non-final stdout chunks observed: %v", ordering)
	}
}

// Compile check: slog must still be used in this pkg (avoid unused-import
// flags if harness changes).
var _ = slog.LevelInfo
var _ = io.Discard
