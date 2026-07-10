package aguiclient

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/frankbardon/nexus/pkg/agui"
)

// Stream is a live, incremental AG-UI run. Where Run buffers the whole SSE
// stream before returning, Stream hands the caller each decoded event as it
// arrives on the Events channel while still accumulating the full ordered
// sequence into a terminal Result. This is the shape the Phase-4 delegate
// integration (E4-S2) uses to pump remote AG-UI events onto the Nexus bus in
// real time and then read the final outcome (including an interrupt) once the
// stream closes.
//
// Lifecycle:
//   - Range over Events() until it is closed. The channel closes when the
//     server finishes the stream (RunFinished / RunError / EOF), a transport or
//     decode error occurs, or the context is cancelled.
//   - After Events() is drained, call Result() for the accumulated events and
//     Err() for any terminal error. Both are safe only once the channel is
//     closed; calling them earlier races the reader goroutine.
//   - Call Close() to release the underlying HTTP response. Close is idempotent
//     and also unblocks the reader if the caller abandons the stream early.
type Stream struct {
	events chan agui.Event
	body   io.Closer

	// Populated by the reader goroutine before it closes events; read only
	// after the channel is drained.
	result Result
	err    error

	closeOnce chan struct{}
}

// Events returns the channel of decoded AG-UI events. It is closed when the run
// terminates or fails.
func (s *Stream) Events() <-chan agui.Event { return s.events }

// Result returns the accumulated run outcome: the HTTP status/header and the
// ordered events observed so far. It is complete only after Events() is closed.
// The returned Result carries the same Interrupt()/Outcome()/Types() helpers as
// Run, so an interrupted run can be resumed by inspecting Result().Interrupt().
func (s *Stream) Result() Result { return s.result }

// Err returns the terminal error of the stream, or nil on a clean finish. It is
// meaningful only after Events() is closed. A non-2xx rejection is not an error
// (inspect Result().StatusCode); a transport failure, decode error, or context
// cancellation is.
func (s *Stream) Err() error { return s.err }

// Close releases the underlying HTTP response body and unblocks the reader
// goroutine if the caller stops consuming Events() early. It is safe to call
// multiple times and safe to call concurrently with draining Events().
func (s *Stream) Close() error {
	select {
	case <-s.closeOnce:
		return nil
	default:
	}
	close(s.closeOnce)
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

// Stream POSTs the input and returns a live Stream that yields each decoded
// AG-UI event as the server produces it. It returns an error only for failures
// that occur before the stream is established (encoding, request construction,
// transport dial). Once the SSE stream is open, per-event decode and transport
// errors are delivered via Stream.Err() after Events() closes.
//
// A non-2xx response (auth/CORS rejection) is not an error: the returned Stream
// carries the status/header in Result() and an already-closed, empty Events()
// channel so callers can range over it uniformly.
func (c *Client) Stream(ctx context.Context, input agui.RunAgentInput) (*Stream, error) {
	resp, err := c.post(ctx, input)
	if err != nil {
		return nil, err
	}

	s := &Stream{
		events:    make(chan agui.Event),
		body:      resp.Body,
		result:    Result{StatusCode: resp.StatusCode, Header: resp.Header},
		closeOnce: make(chan struct{}),
	}

	// A rejection or non-SSE body carries no event stream: drain, record the
	// metadata, and hand back a closed channel so range-over is uniform.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		close(s.events)
		return s, nil
	}

	go s.read(ctx, resp.Body)
	return s, nil
}

// read drains the SSE stream, forwarding each event to the Events channel and
// accumulating them into the terminal Result. It records the first terminal
// error, closes the response body, and closes the channel on exit.
func (s *Stream) read(ctx context.Context, body io.Reader) {
	defer close(s.events)
	defer s.Close()

	reader := agui.NewSSEReader(body)
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			// Distinguish a caller-driven cancellation from a decode/transport
			// fault so downstream callers can react (e.g. not retry a cancel).
			if cerr := ctx.Err(); cerr != nil {
				s.err = fmt.Errorf("aguiclient: stream cancelled: %w", cerr)
				return
			}
			s.err = fmt.Errorf("aguiclient: read sse stream: %w", err)
			return
		}
		s.result.Events = append(s.result.Events, ev)

		select {
		case s.events <- ev:
		case <-ctx.Done():
			s.err = fmt.Errorf("aguiclient: stream cancelled: %w", ctx.Err())
			return
		case <-s.closeOnce:
			// Caller abandoned the stream via Close(); stop forwarding.
			return
		}
	}
}
