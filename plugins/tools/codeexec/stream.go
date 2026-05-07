package codeexec

import (
	"bytes"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// Chunks smaller than this are held until a newline arrives or the writer
// is flushed at script end. Long lines that never hit a newline are forced
// out once the pending buffer exceeds this threshold so the LLM / UI never
// waits indefinitely on a single write.
const streamFlushThreshold = 512

// streamingWriter is the io.Writer installed as Stdout/Stderr on the Yaegi
// interpreter. It does three jobs at once:
//
//  1. Accepts writes from the interpreted script, capped at max bytes.
//  2. Emits code.exec.stdout events per flushed chunk so IO plugins can
//     render output as the script runs.
//  3. Aggregates every accepted byte so the final code.exec.result and the
//     persisted stdout.txt keep the same shape they had before streaming.
//
// Phase-1 rejects `go` statements, so writes are single-threaded in
// practice — the mutex is defensive against future multi-writer shapes.
type streamingWriter struct {
	bus    engine.EventBus
	callID string
	turnID string
	max    int

	mu        sync.Mutex
	pending   []byte       // buffered since the last flush
	total     bytes.Buffer // everything accepted (for the final Output field)
	written   int          // counts bytes accepted into total (= total.Len())
	truncated bool
	closed    bool
}

// newStreamingWriter constructs a writer bound to a specific call. A nil bus
// is tolerated (tests may want the aggregation without the events).
func newStreamingWriter(bus engine.EventBus, callID, turnID string, max int) *streamingWriter {
	return &streamingWriter{
		bus:    bus,
		callID: callID,
		turnID: turnID,
		max:    max,
	}
}

// Write accepts bytes, trims to the remaining cap, and flushes at each
// newline or when the pending buffer crosses the threshold. Per io.Writer
// semantics, the returned byte count is always len(p) even when we drop the
// tail — the script writes never "fail" from its own POV.
func (w *streamingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return len(p), nil
	}

	accepted := p
	if w.max > 0 {
		remaining := w.max - w.written
		if remaining <= 0 {
			w.truncated = true
			return len(p), nil
		}
		if len(accepted) > remaining {
			accepted = accepted[:remaining]
			w.truncated = true
		}
	}

	w.total.Write(accepted)
	w.written += len(accepted)
	w.pending = append(w.pending, accepted...)

	// Flush whole-line chunks: everything up to and including the last
	// newline inside the pending buffer.
	if idx := bytes.LastIndexByte(w.pending, '\n'); idx >= 0 {
		w.flushLocked(w.pending[:idx+1], false)
		w.pending = w.pending[idx+1:]
	}

	// Residual tail got long — force it out so the UI doesn't starve.
	if len(w.pending) >= streamFlushThreshold {
		w.flushLocked(w.pending, false)
		w.pending = w.pending[:0]
	}

	return len(p), nil
}

// Close flushes any buffered tail and emits a final event. Safe to call
// multiple times; only the first call emits.
func (w *streamingWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true

	// Emit whatever's left — plus a sentinel Final event carrying the
	// truncation flag. Combine into one emit when the tail is non-empty so
	// consumers don't have to dedupe on CallID + Final.
	if len(w.pending) > 0 {
		w.flushLocked(w.pending, true)
		w.pending = w.pending[:0]
		return
	}
	w.flushLocked(nil, true)
}

// flushLocked emits a code.exec.stdout event. Caller holds w.mu.
func (w *streamingWriter) flushLocked(chunk []byte, final bool) {
	if w.bus == nil {
		return
	}
	if len(chunk) == 0 && !final {
		return
	}
	_ = w.bus.Emit("code.exec.stdout", events.CodeExecStdout{SchemaVersion: events.CodeExecStdoutVersion, CallID: w.callID,
		TurnID:    w.turnID,
		Chunk:     string(chunk),
		Final:     final,
		Truncated: final && w.truncated,
	})
}

// String returns the full accepted output — used to populate the Output
// field on code.exec.result and the persisted stdout.txt artifact.
func (w *streamingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total.String()
}

// Truncated reports whether any bytes were dropped due to the cap.
func (w *streamingWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}
