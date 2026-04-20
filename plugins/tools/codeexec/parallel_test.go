package codeexec

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// registerSlowEchoTool installs an echo-like tool that sleeps a fixed
// duration before replying. Used to observe concurrent execution via wall
// time (serial would be N*sleep, parallel should be ceil(N/workers)*sleep).
func (h *testHarness) registerSlowEchoTool(sleep time.Duration, inflightPeak *atomic.Int64) {
	_ = h.bus.Emit("tool.register", events.ToolDef{
		Name:        "slow_echo",
		Description: "Echo after sleeping",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
			},
			"required": []any{"message"},
		},
	})
	var cur atomic.Int64
	h.bus.Subscribe("tool.invoke", func(e engine.Event[any]) {
		tc, ok := e.Payload.(events.ToolCall)
		if !ok || tc.Name != "slow_echo" {
			return
		}
		cur.Add(1)
		defer cur.Add(-1)
		if inflightPeak != nil {
			for {
				p := inflightPeak.Load()
				c := cur.Load()
				if c <= p || inflightPeak.CompareAndSwap(p, c) {
					break
				}
			}
		}
		time.Sleep(sleep)
		msg, _ := tc.Arguments["message"].(string)
		_ = h.bus.Emit("tool.result", events.ToolResult{
			ID:     tc.ID,
			Name:   tc.Name,
			Output: "echoed: " + msg,
			TurnID: tc.TurnID,
		})
	}, engine.WithPriority(40), engine.WithSource("fake-slow-echo"))
}

func TestParallel_Map_PreservesOrder(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main

import (
	"context"
	"parallel"
)

func Run(ctx context.Context) (any, error) {
	inputs := []int{10, 20, 30, 40, 50}
	out, err := parallel.Map(ctx, inputs, func(ctx context.Context, x int) (int, error) {
		return x * 2, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("error: %s", res.Error)
	}

	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	resultAny, ok := env["result"].([]any)
	if !ok {
		t.Fatalf("result not a slice: %+v", env["result"])
	}
	got := make([]int, len(resultAny))
	for i, v := range resultAny {
		if n, ok := v.(float64); ok {
			got[i] = int(n)
		}
	}
	want := []int{20, 40, 60, 80, 100}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %d want %d (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestParallel_Map_RunsConcurrently(t *testing.T) {
	h := newHarness(t, map[string]any{"max_workers": 4})
	var peak atomic.Int64
	h.registerSlowEchoTool(50*time.Millisecond, &peak)

	script := `package main

import (
	"context"
	"parallel"
	"tools"
)

func Run(ctx context.Context) (any, error) {
	msgs := []string{"a", "b", "c", "d"}
	return parallel.Map(ctx, msgs, func(ctx context.Context, m string) (string, error) {
		r, err := tools.SlowEcho(tools.SlowEchoArgs{Message: m})
		if err != nil { return "", err }
		return r.Output, nil
	})
}
`
	start := time.Now()
	res := h.runCode(script)
	dur := time.Since(start)
	if res.Error != "" {
		t.Fatalf("error: %s", res.Error)
	}
	if dur > 180*time.Millisecond {
		t.Errorf("expected concurrent execution (<180ms); got %s — possibly serialised", dur)
	}
	if peak.Load() < 2 {
		t.Errorf("expected observed concurrency >=2; peak=%d", peak.Load())
	}
}

func TestParallel_Map_FirstErrorCancelsRest(t *testing.T) {
	h := newHarness(t, map[string]any{"max_workers": 2})

	script := `package main

import (
	"context"
	"errors"
	"parallel"
)

func Run(ctx context.Context) (any, error) {
	inputs := []int{1, 2, 3, 4, 5}
	_, err := parallel.Map(ctx, inputs, func(ctx context.Context, x int) (int, error) {
		if x == 3 {
			return 0, errors.New("boom")
		}
		return x, nil
	})
	if err == nil {
		return "want-error", nil
	}
	return err.Error(), nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("unexpected script error: %s", res.Error)
	}
	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	got, _ := env["result"].(string)
	if !strings.Contains(got, "boom") {
		t.Errorf("expected wrapped 'boom' error, got %q", got)
	}
}

func TestParallel_ForEach_Success(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main

import (
	"context"
	"fmt"
	"parallel"
	"sync/atomic"
)

var counter atomic.Int64

func Run(ctx context.Context) (any, error) {
	inputs := []int{1, 2, 3, 4, 5}
	err := parallel.ForEach(ctx, inputs, func(ctx context.Context, x int) error {
		counter.Add(int64(x))
		return nil
	})
	if err != nil { return nil, err }
	return fmt.Sprintf("sum=%d", counter.Load()), nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("err: %s", res.Error)
	}
	if !strings.Contains(res.Output, "sum=15") {
		t.Errorf("expected sum=15, got %s", res.Output)
	}
}

func TestParallel_All_HeterogeneousFuncs(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main

import (
	"context"
	"parallel"
	"sync/atomic"
)

var a, b, c atomic.Int64

func Run(ctx context.Context) (any, error) {
	err := parallel.All(ctx,
		func(ctx context.Context) error { a.Store(1); return nil },
		func(ctx context.Context) error { b.Store(2); return nil },
		func(ctx context.Context) error { c.Store(3); return nil },
	)
	if err != nil { return nil, err }
	return a.Load() + b.Load() + c.Load(), nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("err: %s", res.Error)
	}
	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	if got, _ := env["result"].(float64); int(got) != 6 {
		t.Errorf("want 6, got %v", env["result"])
	}
}

func TestParallel_All_FirstErrorWins(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main

import (
	"context"
	"errors"
	"parallel"
)

func Run(ctx context.Context) (any, error) {
	err := parallel.All(ctx,
		func(ctx context.Context) error { return nil },
		func(ctx context.Context) error { return errors.New("bad-one") },
		func(ctx context.Context) error { return errors.New("bad-two") },
	)
	if err == nil { return "no-err", nil }
	return err.Error(), nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("unexpected: %s", res.Error)
	}
	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	msg, _ := env["result"].(string)
	if !strings.Contains(msg, "bad-one") && !strings.Contains(msg, "bad-two") {
		t.Errorf("expected wrapped error, got %q", msg)
	}
}

func TestParallel_Map_RespectsWorkerLimit(t *testing.T) {
	h := newHarness(t, map[string]any{"max_workers": 2})
	var peak atomic.Int64
	h.registerSlowEchoTool(30*time.Millisecond, &peak)

	script := `package main

import (
	"context"
	"parallel"
	"tools"
)

func Run(ctx context.Context) (any, error) {
	msgs := []string{"a","b","c","d","e","f","g","h"}
	return parallel.Map(ctx, msgs, func(ctx context.Context, m string) (string, error) {
		r, err := tools.SlowEcho(tools.SlowEchoArgs{Message: m})
		if err != nil { return "", err }
		return r.Output, nil
	})
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("err: %s", res.Error)
	}
	if peak.Load() > 2 {
		t.Errorf("worker limit not respected: observed %d in-flight (cap=2)", peak.Load())
	}
	if peak.Load() < 2 {
		t.Errorf("expected to saturate worker pool; peak=%d", peak.Load())
	}
}

func TestParallel_Map_RejectsInvalidSignature(t *testing.T) {
	h := newHarness(t, nil)

	// fn has wrong element type (int vs string slice).
	script := `package main

import (
	"context"
	"parallel"
)

func Run(ctx context.Context) (any, error) {
	_, err := parallel.Map(ctx, []string{"a"}, func(ctx context.Context, x int) (int, error) {
		return x, nil
	})
	if err == nil { return "no-err", nil }
	return err.Error(), nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("err: %s", res.Error)
	}
	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	msg, _ := env["result"].(string)
	if !strings.Contains(msg, "incompatible") {
		t.Errorf("want validation error, got %q", msg)
	}
}

// TestParallel_Map_PanicInCallback ensures a panicking callback doesn't
// crash the whole script — it surfaces as an error on the Map call.
func TestParallel_Map_PanicInCallback(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main

import (
	"context"
	"parallel"
)

func Run(ctx context.Context) (any, error) {
	_, err := parallel.Map(ctx, []int{1, 2, 3}, func(ctx context.Context, x int) (int, error) {
		if x == 2 {
			var s []int
			_ = s[99]
		}
		return x, nil
	})
	if err == nil { return "no-err", nil }
	return err.Error(), nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("script itself errored: %s", res.Error)
	}
	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	msg, _ := env["result"].(string)
	if !strings.Contains(msg, "panic") && !strings.Contains(msg, "range") {
		t.Errorf("expected panic recovery message, got %q", msg)
	}
}

// Sanity: plugin default max_workers is runtime.NumCPU when unset.
func TestPlugin_DefaultMaxWorkers(t *testing.T) {
	h := newHarness(t, nil)
	if h.plugin.maxWorkers < 1 {
		t.Fatalf("default maxWorkers should be >=1, got %d", h.plugin.maxWorkers)
	}
	_ = fmt.Sprintf // keep fmt import alive
}
