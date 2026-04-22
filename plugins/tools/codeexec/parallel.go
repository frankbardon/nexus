package codeexec

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/traefik/yaegi/interp"
)

// buildParallelExports returns a Yaegi package at import path "parallel"
// that exposes three structured-concurrency primitives usable from scripts:
//
//   - Map(ctx, items, fn)       — ordered, errgroup-style results.
//   - ForEach(ctx, items, fn)   — same as Map without the result slice.
//   - All(ctx, fns...)          — run N heterogeneous funcs concurrently.
//
// All three are first-error-cancels-the-rest: when any callback returns a
// non-nil error, the derived context is cancelled and in-flight work
// observes ctx.Done() (directly, or via the tools.* shims which already
// honor inv.ctx). Workers is the plugin-level concurrency ceiling.
func buildParallelExports(workers int) interp.Exports {
	if workers < 1 {
		workers = 1
	}
	pkg := map[string]reflect.Value{
		"Map": reflect.ValueOf(func(ctx context.Context, items any, fn any) (any, error) {
			return parallelMap(ctx, items, fn, workers)
		}),
		"ForEach": reflect.ValueOf(func(ctx context.Context, items any, fn any) error {
			return parallelForEach(ctx, items, fn, workers)
		}),
		"All": reflect.ValueOf(func(ctx context.Context, fns ...any) error {
			return parallelAll(ctx, fns, workers)
		}),
	}
	return interp.Exports{"parallel/parallel": pkg}
}

// parallelMap runs fn over every element of items, bounded by workers.
// Returns results ([]R where R = fn's first return type) preserving input
// order, or the first error encountered (remaining work is cancelled).
//
// Signature the script sees: parallel.Map(ctx, items, fn).
// fn must have shape: func(context.Context, T) (R, error).
func parallelMap(parent context.Context, items any, fn any, workers int) (any, error) {
	itemsV := reflect.ValueOf(items)
	if !itemsV.IsValid() || itemsV.Kind() != reflect.Slice {
		return nil, errors.New("parallel.Map: items must be a slice")
	}
	fnV := reflect.ValueOf(fn)
	resultType, err := validateMapFn(fnV, itemsV.Type().Elem())
	if err != nil {
		return nil, err
	}

	n := itemsV.Len()
	results := reflect.MakeSlice(reflect.SliceOf(resultType), n, n)
	zeroResults := reflect.Zero(reflect.SliceOf(resultType)).Interface()

	if n == 0 {
		return results.Interface(), nil
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var (
		errMu    sync.Mutex
		firstErr error
	)
	recordErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
			cancel()
		}
		errMu.Unlock()
	}

	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		// Back-pressure: sem blocks once <workers> goroutines are in flight.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					recordErr(fmt.Errorf("parallel.Map[%d] panic: %v", idx, r))
				}
			}()
			if ctx.Err() != nil {
				return
			}
			out := fnV.Call([]reflect.Value{
				reflect.ValueOf(ctx),
				itemsV.Index(idx),
			})
			if !out[1].IsNil() {
				recordErr(fmt.Errorf("parallel.Map[%d]: %w", idx, out[1].Interface().(error)))
				return
			}
			results.Index(idx).Set(out[0])
		}(i)
	}
	wg.Wait()

	if firstErr != nil {
		return zeroResults, firstErr
	}
	return results.Interface(), nil
}

// parallelForEach is parallel.Map minus the result slice — useful when the
// script only cares about side-effects (e.g. firing N tool calls).
func parallelForEach(parent context.Context, items any, fn any, workers int) error {
	itemsV := reflect.ValueOf(items)
	if !itemsV.IsValid() || itemsV.Kind() != reflect.Slice {
		return errors.New("parallel.ForEach: items must be a slice")
	}
	fnV := reflect.ValueOf(fn)
	if err := validateForEachFn(fnV, itemsV.Type().Elem()); err != nil {
		return err
	}

	n := itemsV.Len()
	if n == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var (
		errMu    sync.Mutex
		firstErr error
	)
	recordErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
			cancel()
		}
		errMu.Unlock()
	}

	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					recordErr(fmt.Errorf("parallel.ForEach[%d] panic: %v", idx, r))
				}
			}()
			if ctx.Err() != nil {
				return
			}
			out := fnV.Call([]reflect.Value{
				reflect.ValueOf(ctx),
				itemsV.Index(idx),
			})
			if !out[0].IsNil() {
				recordErr(fmt.Errorf("parallel.ForEach[%d]: %w", idx, out[0].Interface().(error)))
			}
		}(i)
	}
	wg.Wait()
	return firstErr
}

// parallelAll runs N heterogeneous funcs concurrently. Each fn must have
// signature `func(context.Context) error`. First error wins and cancels
// the rest.
func parallelAll(parent context.Context, fns []any, workers int) error {
	if len(fns) == 0 {
		return nil
	}
	fnVals := make([]reflect.Value, 0, len(fns))
	for i, f := range fns {
		v := reflect.ValueOf(f)
		if err := validateAllFn(v); err != nil {
			return fmt.Errorf("parallel.All[%d]: %w", i, err)
		}
		fnVals = append(fnVals, v)
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var (
		errMu    sync.Mutex
		firstErr error
	)
	recordErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
			cancel()
		}
		errMu.Unlock()
	}

	for i, fnV := range fnVals {
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(idx int, fn reflect.Value) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					recordErr(fmt.Errorf("parallel.All[%d] panic: %v", idx, r))
				}
			}()
			if ctx.Err() != nil {
				return
			}
			out := fn.Call([]reflect.Value{reflect.ValueOf(ctx)})
			if !out[0].IsNil() {
				recordErr(fmt.Errorf("parallel.All[%d]: %w", idx, out[0].Interface().(error)))
			}
		}(i, fnV)
	}
	wg.Wait()
	return firstErr
}

// -- signature validation ---------------------------------------------------

var (
	ctxInterface = reflect.TypeOf((*context.Context)(nil)).Elem()
)

// validateMapFn returns R for a valid `func(context.Context, T) (R, error)`.
func validateMapFn(fn reflect.Value, wantT reflect.Type) (reflect.Type, error) {
	if !fn.IsValid() || fn.Kind() != reflect.Func {
		return nil, errors.New("parallel.Map: fn must be a function")
	}
	t := fn.Type()
	if t.NumIn() != 2 {
		return nil, fmt.Errorf("parallel.Map: fn must take (context.Context, T); got %d args", t.NumIn())
	}
	if !t.In(0).Implements(ctxInterface) && t.In(0) != ctxInterface {
		return nil, fmt.Errorf("parallel.Map: fn first arg must be context.Context; got %s", t.In(0))
	}
	if !wantT.AssignableTo(t.In(1)) && !t.In(1).AssignableTo(wantT) {
		return nil, fmt.Errorf("parallel.Map: fn second arg (%s) incompatible with items element type (%s)",
			t.In(1), wantT)
	}
	if t.NumOut() != 2 {
		return nil, fmt.Errorf("parallel.Map: fn must return (R, error); got %d returns", t.NumOut())
	}
	if !t.Out(1).Implements(errorInterface) && t.Out(1) != errorInterface {
		return nil, fmt.Errorf("parallel.Map: fn second return must be error; got %s", t.Out(1))
	}
	return t.Out(0), nil
}

// validateForEachFn ensures `func(context.Context, T) error`.
func validateForEachFn(fn reflect.Value, wantT reflect.Type) error {
	if !fn.IsValid() || fn.Kind() != reflect.Func {
		return errors.New("parallel.ForEach: fn must be a function")
	}
	t := fn.Type()
	if t.NumIn() != 2 {
		return fmt.Errorf("parallel.ForEach: fn must take (context.Context, T); got %d args", t.NumIn())
	}
	if !t.In(0).Implements(ctxInterface) && t.In(0) != ctxInterface {
		return fmt.Errorf("parallel.ForEach: fn first arg must be context.Context")
	}
	if !wantT.AssignableTo(t.In(1)) && !t.In(1).AssignableTo(wantT) {
		return fmt.Errorf("parallel.ForEach: fn second arg (%s) incompatible with items element type (%s)",
			t.In(1), wantT)
	}
	if t.NumOut() != 1 {
		return fmt.Errorf("parallel.ForEach: fn must return error; got %d returns", t.NumOut())
	}
	if !t.Out(0).Implements(errorInterface) && t.Out(0) != errorInterface {
		return fmt.Errorf("parallel.ForEach: fn return must be error")
	}
	return nil
}

// validateAllFn ensures `func(context.Context) error`.
func validateAllFn(fn reflect.Value) error {
	if !fn.IsValid() || fn.Kind() != reflect.Func {
		return errors.New("fn must be a function")
	}
	t := fn.Type()
	if t.NumIn() != 1 {
		return fmt.Errorf("fn must take (context.Context); got %d args", t.NumIn())
	}
	if !t.In(0).Implements(ctxInterface) && t.In(0) != ctxInterface {
		return errors.New("fn arg must be context.Context")
	}
	if t.NumOut() != 1 {
		return fmt.Errorf("fn must return error; got %d returns", t.NumOut())
	}
	if !t.Out(0).Implements(errorInterface) && t.Out(0) != errorInterface {
		return errors.New("fn return must be error")
	}
	return nil
}
