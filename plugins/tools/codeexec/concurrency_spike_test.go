package codeexec

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// These tests probe whether Yaegi's interpreter is safe to re-enter from
// multiple host goroutines. The outcome gates the `parallel` package design:
//
//   - If concurrent Call() on an interpreted function is safe (with or
//     without a per-call mutex), we can expose parallel.Map/ForEach/All
//     that execute interpreted callbacks on a host worker pool.
//   - If it panics or races, we fall back to a narrower primitive that
//     only parallelises tools.* calls (pure bus emissions) and serialises
//     the script-side coordination.
//
// Run with:
//   go test -race ./plugins/tools/codeexec/ -run TestSpike_Concurrent -v

// TestSpike_ConcurrentSameFunc — N host goroutines call the same pure
// interpreted function in parallel. Pure compute, no shared state in the
// Yaegi-side code. If this races, nothing generic is safe.
func TestSpike_ConcurrentSameFunc(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatalf("stdlib: %v", err)
	}
	_, err := i.Eval(`
package main

func Double(x int) int {
	return x * 2
}
`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}

	fn, err := i.Eval("main.Double")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	const N = 200
	var (
		wg   sync.WaitGroup
		errs = make(chan error, N)
	)
	for n := 0; n < N; n++ {
		n := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs <- fmt.Errorf("panic for n=%d: %v", n, r)
				}
			}()
			out := fn.Call([]reflect.Value{reflect.ValueOf(n)})
			if got := out[0].Int(); got != int64(n*2) {
				errs <- fmt.Errorf("n=%d: got %d want %d", n, got, n*2)
			}
		}()
	}
	wg.Wait()
	close(errs)

	var count int
	for e := range errs {
		t.Error(e)
		count++
	}
	if count > 0 {
		t.Fatalf("%d goroutines failed — interpreter not safe for concurrent Call()", count)
	}
}

// TestSpike_ConcurrentSameFunc_LockedEntry — same as above but we hold a
// plugin-owned mutex across each Call(). If the unlocked version fails but
// this passes, we can still offer parallel-with-serialised-entry semantics
// (useful mainly for tools.* fanout, since the serialised entry defeats
// CPU-parallelism benefits for pure compute).
func TestSpike_ConcurrentSameFunc_LockedEntry(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatalf("stdlib: %v", err)
	}
	if _, err := i.Eval(`package main
func Double(x int) int { return x * 2 }`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	fn, _ := i.Eval("main.Double")

	var interpMu sync.Mutex
	const N = 200
	var wg sync.WaitGroup
	var good atomic.Int64
	for n := 0; n < N; n++ {
		n := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			interpMu.Lock()
			out := fn.Call([]reflect.Value{reflect.ValueOf(n)})
			interpMu.Unlock()
			if out[0].Int() == int64(n*2) {
				good.Add(1)
			}
		}()
	}
	wg.Wait()
	if int(good.Load()) != N {
		t.Fatalf("locked-entry variant failed: %d/%d succeeded", good.Load(), N)
	}
}

// TestSpike_ConcurrentClosureOverLocal — the closure reads an immutable
// local variable captured from its enclosing scope. This is the realistic
// shape of a parallel.Map callback: `func(x T) R { /* use x and some outer
// const */ }`. If this races, even read-only closure capture is unsafe.
func TestSpike_ConcurrentClosureOverLocal(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatalf("stdlib: %v", err)
	}
	if _, err := i.Eval(`package main

func MakeScale(factor int) func(int) int {
	return func(x int) int { return x * factor }
}
`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	maker, _ := i.Eval("main.MakeScale")
	scale := maker.Call([]reflect.Value{reflect.ValueOf(7)})[0] // 7x scaler

	const N = 200
	var wg sync.WaitGroup
	var fail atomic.Int64
	for n := 0; n < N; n++ {
		n := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					fail.Add(1)
				}
			}()
			out := scale.Call([]reflect.Value{reflect.ValueOf(n)})
			if out[0].Int() != int64(n*7) {
				fail.Add(1)
			}
		}()
	}
	wg.Wait()
	if fail.Load() > 0 {
		t.Fatalf("closure-over-local unsafe: %d/%d failed", fail.Load(), N)
	}
}

// TestSpike_ConcurrentDifferentFuncs — different interpreted functions
// called concurrently. This models parallel.All([]func() ...). If this
// races but same-func passes, per-function state is the issue and we need
// to design around that.
func TestSpike_ConcurrentDifferentFuncs(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatalf("stdlib: %v", err)
	}
	if _, err := i.Eval(`package main

func A() int { return 1 }
func B() int { return 2 }
func C() int { return 3 }
`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	a, _ := i.Eval("main.A")
	b, _ := i.Eval("main.B")
	c, _ := i.Eval("main.C")

	const iters = 100
	var wg sync.WaitGroup
	var fail atomic.Int64
	for iter := 0; iter < iters; iter++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					fail.Add(1)
				}
			}()
			if a.Call(nil)[0].Int() != 1 {
				fail.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					fail.Add(1)
				}
			}()
			if b.Call(nil)[0].Int() != 2 {
				fail.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					fail.Add(1)
				}
			}()
			if c.Call(nil)[0].Int() != 3 {
				fail.Add(1)
			}
		}()
	}
	wg.Wait()
	if fail.Load() > 0 {
		t.Fatalf("%d concurrent calls across distinct interpreted funcs failed", fail.Load())
	}
}
