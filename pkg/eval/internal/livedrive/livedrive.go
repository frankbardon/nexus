// Package livedrive holds the "drive a live engine, then wait for it to
// idle" loop shared by pkg/eval/runner's RunLive and pkg/eval/protocol's
// Run. Both packages boot a real (non-replay) engine, let some other
// component fire the case's/request's input(s) into it — that part stays
// caller-specific, since each package overlays its config differently (see
// each package's own YAML-surgery helpers; pkg/eval's own context notes
// these are deliberately not unifiable) — and then need to block until
// either the engine's own session-end signal fires, an optional turn cap
// is hit, or the caller's context runs out. This is that block, factored
// into one implementation so it exists exactly once in the repo.
//
// It is an internal package on purpose: it is not a general-purpose public
// API, just the seam between runner and protocol that lets both import it
// without either importing the other (which would risk a cycle — the same
// reasoning as the deliberate one-way report→runner edge documented on
// pkg/eval/runner/multi.go's RunMany doc comment). Both packages live
// under pkg/eval, so both can import pkg/eval/internal/livedrive.
package livedrive

import (
	"context"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
)

// WaitForIdle blocks until eng's live session completes, one of three ways:
//
//   - Naturally: a plugin emits io.session.end, surfaced on
//     eng.SessionEnded(). Returns (false, nil).
//   - By turn cap: maxTurns > 0 and that many agent.turn.end events have
//     fired. Returns (true, nil).
//   - By context: ctx is cancelled or its deadline is exceeded before
//     either of the above. Returns (false, ctx.Err()).
//
// maxTurns <= 0 disables the cap — WaitForIdle then only returns on natural
// completion or ctx exhaustion. RunLive passes 0: it drives a case's whole
// Inputs list and waits for the transport's own session-end, not a
// turn-count cap. protocol.Run passes its request's MaxTurns.
//
// Callers are responsible for translating a non-nil err into their own
// error type (e.g. protocol.Run wraps it as ErrTimeout); WaitForIdle itself
// is error-type-agnostic on purpose, since runner and protocol don't share
// an error vocabulary.
func WaitForIdle(ctx context.Context, eng *engine.Engine, maxTurns int) (hitCap bool, err error) {
	turnCtx, turnCancel := context.WithCancel(ctx)
	defer turnCancel()

	var (
		mu       sync.Mutex
		turnEnds int
		capped   bool
	)

	unsub := eng.Bus.Subscribe("agent.turn.end", func(_ engine.Event[any]) {
		mu.Lock()
		turnEnds++
		if maxTurns > 0 && turnEnds >= maxTurns {
			capped = true
			turnCancel()
		}
		mu.Unlock()
	})
	defer unsub()

	select {
	case <-eng.SessionEnded():
		return false, nil
	case <-turnCtx.Done():
		mu.Lock()
		reachedCap := capped
		mu.Unlock()
		if reachedCap {
			return true, nil
		}
		return false, ctx.Err()
	}
}
