package a2aclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// Stream is a live A2A streaming call: SendStreamingMessage or SubscribeToTask.
// It hands the caller each decoded frame as it arrives while accumulating the
// whole exchange into a StreamResult.
//
// # Lifecycle
//
// Range over Frames() until it closes. It closes when the remote reaches a
// terminal task state, when the remote closes the connection, when a frame
// fails to decode or violates the stream contract, when the idle timeout fires,
// or when the caller cancels the context or calls Close.
//
// After Frames() is drained, Result() carries everything observed and Err()
// carries the terminal error — nil for a stream that reached a terminal state
// cleanly. Both are safe to call at any time (they are mutex-guarded) but are
// only COMPLETE once the channel has closed.
//
// Close is idempotent, safe to call concurrently with draining, and is what
// releases the underlying HTTP response. A caller that abandons a stream early
// must call it; a caller that drains to the end need not, though calling it is
// harmless. Nothing outlives it: the reader goroutine and the idle watchdog
// both exit before the underlying body is done being read.
type Stream struct {
	operation string

	frames chan a2a.StreamResponse
	body   io.ReadCloser
	cancel context.CancelFunc

	idle     time.Duration
	activity chan struct{}

	// done is closed by the reader goroutine as it exits, which is what lets
	// the idle watchdog retire.
	done chan struct{}
	// closedCh is closed by Close, which is what unblocks a reader parked on a
	// send to a caller that has stopped consuming.
	closedCh chan struct{}

	closeOnce   sync.Once
	releaseOnce sync.Once
	closeErr    error

	mu         sync.Mutex
	result     StreamResult
	err        error
	idleFired  bool
	userClosed bool
}

// Frames returns the channel of decoded stream frames. It is closed when the
// stream terminates for any reason.
func (s *Stream) Frames() <-chan a2a.StreamResponse { return s.frames }

// Result returns everything observed so far: the frames in order, the task and
// context identity, the latest status, and the reassembled artifacts. It is
// complete once Frames() has closed, and is populated even when Err() is
// non-nil, so a caller can see how far a failing stream got.
func (s *Stream) Result() StreamResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result.clone()
}

// Err returns the stream's terminal error, or nil when it reached a terminal
// task state cleanly. It is meaningful once Frames() has closed. Every non-nil
// value is a *StreamError.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close abandons the stream, releasing the HTTP response and unblocking the
// reader. It is idempotent and safe to call concurrently with draining Frames().
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.err == nil && !s.result.Terminal {
			s.userClosed = true
		}
		s.mu.Unlock()
		close(s.closedCh)
	})
	s.release()
	return s.closeErr
}

// release cancels the request context and closes the response body exactly
// once. It is what both Close and the idle watchdog use; unlike Close it does
// not mark the stream as caller-abandoned.
func (s *Stream) release() {
	s.releaseOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.body != nil {
			s.closeErr = s.body.Close()
		}
	})
}

// ---- Opening a stream ----

// SendStreamingMessage sends a message and returns the live stream of frames
// the remote answers with.
//
// It refuses before sending anything if the remote's Agent Card declares
// capabilities.streaming = false, since the specification's answer to a
// streaming call on a non-streaming agent is a refusal and discovering that
// locally is faster and clearer than discovering it over the wire.
//
// A message carrying a TaskID and ContextID resumes an existing task on a fresh
// stream; see ResumeRequest.
func (c *Client) SendStreamingMessage(ctx context.Context, req a2a.SendMessageRequest) (*Stream, error) {
	if err := a2a.ValidateSendMessageRequest(&req); err != nil {
		return nil, err
	}
	if err := c.requireStreaming(ctx, a2a.MethodSendStreamingMessage); err != nil {
		return nil, err
	}
	endpoint, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}

	call, err := c.streamCall(a2a.MethodSendStreamingMessage, endpoint, a2a.PathStreamMessage, req)
	if err != nil {
		return nil, err
	}
	return c.openStream(ctx, call)
}

// SubscribeToTask reattaches to a task already in flight and streams its
// remaining frames. The stream opens with a Task snapshot in whatever state the
// task currently holds, which is why the reader does not apply the transition
// table to an opening frame.
func (c *Client) SubscribeToTask(ctx context.Context, req a2a.SubscribeToTaskRequest) (*Stream, error) {
	if strings.TrimSpace(req.ID) == "" {
		return nil, a2a.ErrInvalidParams(a2a.FieldViolation{Field: "id", Description: "task id is required"})
	}
	if err := c.requireStreaming(ctx, a2a.MethodSubscribeToTask); err != nil {
		return nil, err
	}
	endpoint, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}

	path, perr := a2a.TaskPath(a2a.PathSubscribeTask, req.ID)
	if perr != nil {
		return nil, perr
	}
	call, err := c.streamCall(a2a.MethodSubscribeToTask, endpoint, path, req)
	if err != nil {
		return nil, err
	}
	return c.openStream(ctx, call)
}

// Run sends a streaming message and drains it to completion, returning the
// accumulated result. It is the convenience form for a caller that wants the
// outcome rather than the incremental frames; the result is returned even when
// the stream failed part way, alongside the error.
func (c *Client) Run(ctx context.Context, req a2a.SendMessageRequest) (StreamResult, error) {
	stream, err := c.SendStreamingMessage(ctx, req)
	if err != nil {
		return StreamResult{}, err
	}
	defer stream.Close()

	for range stream.Frames() {
		// Drained for its side effect on the accumulated result.
	}
	return stream.Result(), stream.Err()
}

// streamCall assembles the HTTP request for a streaming operation in the
// client's binding: a JSON-RPC envelope posted to the single endpoint, or the
// bare parameter object posted to the operation's REST path.
func (c *Client) streamCall(operation, endpoint, restPath string, params any) (httpCall, error) {
	call := httpCall{
		operation: operation,
		method:    http.MethodPost,
		accept:    a2a.ContentTypeSSE,
	}
	if c.binding == a2a.BindingHTTPJSON {
		body, err := a2a.Encode(params)
		if err != nil {
			return call, err
		}
		call.url = endpoint + restPath
		call.body = body
		return call, nil
	}

	id := strconv.FormatUint(c.rpcID.Add(1), 10)
	req, err := a2a.NewRequest(id, operation, params)
	if err != nil {
		return call, err
	}
	body, err := req.Encode()
	if err != nil {
		return call, err
	}
	call.url = endpoint
	call.body = body
	return call, nil
}

// openStream performs the request and, if the remote really did answer with a
// stream, starts the reader.
//
// Everything that can go wrong before the first frame is answered here rather
// than through the frame channel, because a caller has not yet started ranging
// and would otherwise learn of an auth failure as an empty stream.
func (c *Client) openStream(ctx context.Context, call httpCall) (*Stream, error) {
	streamCtx, cancel := context.WithCancel(ctx)

	// The open timeout covers the request and its retries up to the arrival of
	// the response headers, and not a byte further: Do returns as soon as the
	// headers land, so stopping the timer immediately afterwards bounds the
	// handshake without bounding the stream.
	var opened *time.Timer
	if c.streamOpenTimeout > 0 {
		opened = time.AfterFunc(c.streamOpenTimeout, cancel)
	}
	resp, err := c.send(streamCtx, call)
	timedOut := opened != nil && !opened.Stop()

	if err != nil {
		cancel()
		if timedOut {
			return nil, &StreamError{
				Reason:    StreamReasonOpenTimeout,
				Operation: call.operation,
				Detail: fmt.Sprintf("the remote sent no response headers within %s",
					c.streamOpenTimeout),
				Err: err,
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &StreamError{
				Reason:    StreamReasonCanceled,
				Operation: call.operation,
				Detail:    "the context was cancelled before the stream opened",
				Err:       ctxErr,
			}
		}
		return nil, &StreamError{
			Reason:    StreamReasonTransport,
			Operation: call.operation,
			Detail:    "the request failed before the stream opened",
			Err:       err,
		}
	}

	if err := checkSSEResponse(call, resp); err != nil {
		drain(resp)
		cancel()
		return nil, err
	}

	s := &Stream{
		operation: call.operation,
		frames:    make(chan a2a.StreamResponse),
		body:      resp.Body,
		cancel:    cancel,
		idle:      c.streamIdleTimeout,
		activity:  make(chan struct{}, 1),
		done:      make(chan struct{}),
		closedCh:  make(chan struct{}),
		result:    StreamResult{Operation: call.operation, Header: resp.Header},
	}

	src := io.Reader(resp.Body)
	if s.idle > 0 {
		src = &activityReader{r: src, notify: s.touch}
		go s.watchIdle()
	}
	go s.read(streamCtx, src)
	return s, nil
}

// checkSSEResponse verifies that the remote answered a streaming call with an
// actual stream. Both failure modes it catches — an HTTP error status, and a
// 200 carrying something that is not text/event-stream — are reported as a
// *StreamError wrapping the most specific cause available, so a caller can
// still recover a server-sent *a2a.Error with errors.As.
func checkSSEResponse(call httpCall, resp *http.Response) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySnippet*4))
		return &StreamError{
			Reason:    StreamReasonNotSSE,
			Operation: call.operation,
			Detail: fmt.Sprintf("the remote answered HTTP %d where a %s stream was promised",
				resp.StatusCode, a2a.ContentTypeSSE),
			Err: interpretHTTPError(call.url, resp, body),
		}
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, a2a.ContentTypeSSE) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySnippet))
		return &StreamError{
			Reason:    StreamReasonNotSSE,
			Operation: call.operation,
			Detail: fmt.Sprintf("the remote answered content type %q where %s was promised: %s",
				contentType, a2a.ContentTypeSSE, snippet(body)),
		}
	}
	return nil
}

// ---- Reading ----

// read drains the SSE stream, publishing each frame and folding it into the
// result. It owns the response body from here on: on every exit path it
// releases the response, retires the idle watchdog, and closes the frame
// channel, in that order.
func (s *Stream) read(ctx context.Context, src io.Reader) {
	defer close(s.frames)
	defer close(s.done)
	defer s.release()

	reader := a2a.NewSSEReader(src)
	for {
		frame, err := reader.Next()
		if err != nil {
			s.finish(ctx, reader, err)
			return
		}

		s.mu.Lock()
		s.result.add(*frame)
		s.mu.Unlock()

		select {
		case s.frames <- *frame:
		case <-ctx.Done():
			s.finish(ctx, reader, ctx.Err())
			return
		case <-s.closedCh:
			s.finish(ctx, reader, context.Canceled)
			return
		}

		// A frame reporting a terminal state ends the exchange. The
		// specification requires the remote to close the connection at that
		// point, but a client that WAITS for the close is at the mercy of a
		// remote that forgets to: the exchange is over either way, so stop
		// reading rather than block until a timeout notices.
		if reader.Closed() {
			s.setErr(nil)
			return
		}
	}
}

// finish classifies the reason a stream stopped and records it.
//
// The order of the checks is the substance: a stream that was killed by the
// idle watchdog, by the caller's Close, or by a cancelled context reports THAT,
// not the read error the kill produced. Only once those are excluded does the
// error itself get to speak.
func (s *Stream) finish(ctx context.Context, reader *a2a.SSEReader, err error) {
	s.mu.Lock()
	idle := s.idleFired
	user := s.userClosed
	frames := len(s.result.Frames)
	state := s.result.State
	s.mu.Unlock()

	terminal := reader.Closed()
	if terminal && errors.Is(err, io.EOF) {
		// The ordinary end of a well-behaved stream: a terminal frame, then the
		// remote closing the connection.
		s.setErr(nil)
		return
	}

	switch {
	case idle:
		s.setErr(&StreamError{
			Reason: StreamReasonIdle, Operation: s.operation, Frames: frames,
			Detail: fmt.Sprintf("no bytes arrived for %s", s.idle),
		})
	case user:
		s.setErr(&StreamError{
			Reason: StreamReasonCanceled, Operation: s.operation, Frames: frames,
			Detail: "the stream was closed by the caller before it terminated",
			Err:    context.Canceled,
		})
	case ctx.Err() != nil:
		s.setErr(&StreamError{
			Reason: StreamReasonCanceled, Operation: s.operation, Frames: frames,
			Detail: "the context was cancelled before the stream terminated",
			Err:    ctx.Err(),
		})
	case errors.Is(err, io.EOF):
		if terminal {
			s.setErr(nil)
			return
		}
		if frames > 0 && state.IsInterrupted() {
			// A stream parked on INPUT_REQUIRED or AUTH_REQUIRED that the
			// remote then closed is not a truncation: the task is alive, the
			// client has the question, and the resume path is a fresh message
			// carrying the same taskId. Treating it as a failure would make an
			// answerable interruption look like a broken remote.
			s.setErr(nil)
			return
		}
		s.setErr(&StreamError{
			Reason: StreamReasonTruncated, Operation: s.operation, Frames: frames,
			Detail: truncationDetail(frames, state),
		})
	default:
		s.setErr(classifyFrameError(s.operation, frames, err))
	}
}

// truncationDetail explains a stream that ended early in terms of where it got
// to, which is the difference between "the remote never answered" and "the
// remote gave up half way".
func truncationDetail(frames int, state a2a.TaskState) string {
	if frames == 0 {
		return "the remote closed the stream without sending a single frame"
	}
	return fmt.Sprintf("the remote closed the stream in non-terminal state %s", state.String())
}

// classifyFrameError maps a reader failure onto a StreamReason.
//
// The codec reports a malformed payload, a locally-detected contract violation
// and a server-sent error frame all as *a2a.Error, so the error TYPE is what
// separates them: a parse or validation failure is a malformed frame, an
// InvalidAgentResponseError is the remote breaking the stream contract, and
// anything else is a protocol error the remote deliberately framed — an auth
// failure, a version mismatch, an unknown task.
func classifyFrameError(operation string, frames int, err error) *StreamError {
	var protoErr *a2a.Error
	if errors.As(err, &protoErr) {
		switch protoErr.Type {
		case a2a.ErrorTypeJSONParse, a2a.ErrorTypeInvalidParams:
			return &StreamError{
				Reason: StreamReasonMalformed, Operation: operation, Frames: frames,
				Detail: "the remote sent a frame that could not be decoded", Err: protoErr,
			}
		case a2a.ErrorTypeInvalidAgentResponse:
			return &StreamError{
				Reason: StreamReasonProtocol, Operation: operation, Frames: frames,
				Detail: "the remote violated the A2A stream contract", Err: protoErr,
			}
		default:
			return &StreamError{
				Reason: StreamReasonRemoteError, Operation: operation, Frames: frames,
				Detail: "the remote ended the stream with a protocol error", Err: protoErr,
			}
		}
	}
	return &StreamError{
		Reason: StreamReasonTransport, Operation: operation, Frames: frames,
		Detail: "the connection failed mid-stream", Err: err,
	}
}

// setErr records the terminal error, keeping the first one seen.
func (s *Stream) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// ---- Idle detection ----

// activityReader signals liveness on every non-empty read.
//
// Liveness is counted in BYTES, not frames, because A2A's keep-alive is an SSE
// comment record: it carries no frame, and a stream deliberately parked on
// INPUT_REQUIRED is held open by nothing else. Counting frames would kill
// exactly the streams the comments exist to preserve.
type activityReader struct {
	r      io.Reader
	notify func()
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.notify()
	}
	return n, err
}

// touch records liveness without blocking the reader. A full buffer already
// means "there is unprocessed activity", so dropping the signal loses nothing.
func (s *Stream) touch() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

// watchIdle aborts a stream that has gone completely silent. It retires when
// the reader exits or the caller closes, so it never outlives the stream.
func (s *Stream) watchIdle() {
	timer := time.NewTimer(s.idle)
	defer timer.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-s.closedCh:
			return
		case <-s.activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.idle)
		case <-timer.C:
			s.mu.Lock()
			s.idleFired = true
			s.mu.Unlock()
			// Releasing, rather than Close, so the abort is not misreported as
			// a caller-initiated cancellation.
			s.release()
			return
		}
	}
}
