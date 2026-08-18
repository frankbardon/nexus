package a2a

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Streaming transport for the two HTTP bindings. SendStreamingMessage and
// SubscribeToTask answer with a text/event-stream body whose records each carry
// one StreamResponse (specification section 11.7).
//
// Two framings exist and this file implements both:
//
//   - REST binding: the "data:" payload is a bare StreamResponse object.
//   - JSON-RPC binding: the "data:" payload is a full JSON-RPC Response
//     envelope repeating the request id, whose "result" is the StreamResponse.
//
// The reader auto-detects the framing per record, so a client does not have to
// know which binding the server chose before it starts reading.

// maxSSERecordBytes bounds a single SSE record. Artifact chunks carrying inline
// binary can be large, so the default 64 KiB scanner buffer is far too small.
const maxSSERecordBytes = 8 * 1024 * 1024

// Stream sentinel errors. These report misuse or termination of the local
// stream object; protocol-shaped failures are reported as *Error instead.
var (
	// ErrStreamClosed is returned by SSEWriter.Write once the stream has
	// terminated, whether because the task reached a terminal state, because an
	// error frame was written, or because Close was called.
	ErrStreamClosed = errors.New("a2a: stream is closed")
	// ErrStreamOrder is returned when a frame would violate the ordering rules
	// of specification section 11.7. It is wrapped with the offending detail.
	ErrStreamOrder = errors.New("a2a: stream frame out of order")
)

// WriteSSEHeaders sets the response headers for an A2A SSE stream. Buffering is
// disabled explicitly because intermediaries otherwise coalesce records and
// defeat incremental delivery.
func WriteSSEHeaders(h http.Header) {
	h.Set("Content-Type", ContentTypeSSE)
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// SSEWriter serializes StreamResponse frames onto an io.Writer as Server-Sent
// Events while enforcing the A2A stream contract.
//
// The contract it enforces, in order of application:
//
//  1. Every frame must set exactly one payload arm and be internally valid
//     (ValidateStreamResponse).
//  2. The first frame must be a Task or a Message. Nothing else may open a
//     stream.
//  3. No later frame may be a Task or a Message; after the opening frame only
//     TaskStatusUpdateEvent and TaskArtifactUpdateEvent are legal.
//  4. Update frames must name the task and context the opening Task frame
//     established.
//  5. Status updates must follow a legal task state transition
//     (ValidateTransition) from the last observed state.
//  6. The stream closes as soon as a frame reports a terminal state —
//     COMPLETED, FAILED, CANCELED or REJECTED alike. Writes after that return
//     ErrStreamClosed.
//
// # Interleaving
//
// Rules 2 and 3 are the whole of the ordering constraint. Status and artifact
// updates may interleave freely after the opening frame. A stricter reading
// ("all status updates, then all artifact updates") is not implementable: the
// COMPLETED status frame that closes a stream necessarily follows the artifact
// frames carrying the output it completes.
//
// # Concurrency
//
// SSEWriter is safe for concurrent use: a mutex guards both the stream state
// machine and the byte writes, so frames are never torn. Nexus serve plugins
// still funnel every frame through one goroutine (bus handlers push onto a
// buffered channel, the HTTP goroutine drains it) — the lock is belt and braces
// against a caller that forgets.
type SSEWriter struct {
	w       io.Writer
	flusher http.Flusher
	// rpcID is non-nil for the JSON-RPC binding and carries the request id each
	// frame's envelope repeats. Nil selects the bare REST framing.
	rpcID json.RawMessage

	mu        sync.Mutex
	started   bool
	closed    bool
	frames    int
	state     TaskState
	taskID    string
	contextID string
}

// NewSSEWriter wraps w for the HTTP+JSON/REST binding: each record's payload is
// a bare StreamResponse. If w implements http.Flusher every record is flushed.
func NewSSEWriter(w io.Writer) *SSEWriter {
	sw := &SSEWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		sw.flusher = f
	}
	return sw
}

// NewJSONRPCSSEWriter wraps w for the JSON-RPC binding: each record's payload is
// a JSON-RPC Response envelope repeating id, whose result is the StreamResponse.
// A nil id is written as JSON null, which is what the envelope requires when the
// request id could not be recovered.
func NewJSONRPCSSEWriter(w io.Writer, id json.RawMessage) *SSEWriter {
	sw := NewSSEWriter(w)
	if len(id) == 0 {
		id = nullID
	}
	sw.rpcID = id
	return sw
}

// Closed reports whether the stream has terminated and will accept no further
// frames.
func (sw *SSEWriter) Closed() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.closed
}

// State returns the last task state observed on the stream, or
// TaskStateUnspecified before the opening frame.
func (sw *SSEWriter) State() TaskState {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.state
}

// Interrupted reports whether the stream is open and parked in a non-terminal
// interrupted state — INPUT_REQUIRED or AUTH_REQUIRED. See the package doc for
// why such a stream deliberately stays open and what a serving layer owes the
// client in that situation.
func (sw *SSEWriter) Interrupted() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return !sw.closed && sw.state.IsInterrupted()
}

// Frames returns the number of payload frames written so far. Comments and
// error frames are not counted.
func (sw *SSEWriter) Frames() int {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.frames
}

// TaskID returns the task id the opening Task frame established, empty for a
// message-only stream.
func (sw *SSEWriter) TaskID() string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.taskID
}

// Write validates and emits one StreamResponse frame, then closes the stream if
// the frame terminated the task. It returns ErrStreamClosed if the stream has
// already terminated, an error wrapping ErrStreamOrder for an ordering
// violation, and an *Error for a malformed frame or an illegal state
// transition.
func (sw *SSEWriter) Write(frame StreamResponse) error {
	if verr := ValidateStreamResponse(&frame); verr != nil {
		return verr
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.closed {
		return ErrStreamClosed
	}

	terminates, err := sw.admit(frame)
	if err != nil {
		return err
	}

	data, err := sw.encode(frame)
	if err != nil {
		return err
	}
	if err := sw.writeRecord(data); err != nil {
		// A failed write means the transport is gone; nothing further can be
		// delivered, so the stream is dead regardless of task state.
		sw.closed = true
		return err
	}

	sw.commit(frame)
	sw.frames++
	if terminates {
		sw.closed = true
	}
	return nil
}

// admit runs the stream contract against a frame without mutating the writer.
// It reports whether the frame terminates the stream.
func (sw *SSEWriter) admit(frame StreamResponse) (bool, error) {
	kind := frame.Kind()

	if !sw.started {
		switch kind {
		case StreamPayloadTask:
			// The opening Task is a snapshot, not a transition: SubscribeToTask
			// legitimately opens with a task already WORKING or INPUT_REQUIRED,
			// so the transition table must not be applied to it. A snapshot in
			// a terminal state is legal and closes the stream at once.
			return frame.Task.Status.State.IsTerminal(), nil
		case StreamPayloadMessage:
			// A direct message reply is the whole of the interaction: there is
			// no task, so no update event can follow and the stream ends here.
			return true, nil
		default:
			return false, fmt.Errorf("%w: stream must open with a task or message frame, got %s", ErrStreamOrder, kind)
		}
	}

	switch kind {
	case StreamPayloadTask, StreamPayloadMessage:
		return false, fmt.Errorf("%w: %s frame is only legal as the opening frame", ErrStreamOrder, kind)
	case StreamPayloadStatusUpdate:
		e := frame.StatusUpdate
		if err := sw.checkIdentity(e.TaskID, e.ContextID); err != nil {
			return false, err
		}
		if err := ValidateTransition(sw.state, e.Status.State); err != nil {
			return false, err
		}
		return e.Status.State.IsTerminal(), nil
	case StreamPayloadArtifactUpdate:
		e := frame.ArtifactUpdate
		if err := sw.checkIdentity(e.TaskID, e.ContextID); err != nil {
			return false, err
		}
		return false, nil
	}
	return false, fmt.Errorf("%w: unknown payload kind %q", ErrStreamOrder, string(kind))
}

// checkIdentity rejects an update event addressed at a different task or
// context than the one the stream opened on.
func (sw *SSEWriter) checkIdentity(taskID, contextID string) error {
	if sw.taskID != "" && taskID != sw.taskID {
		return Errorf(ErrorTypeInvalidAgentResponse,
			"stream frame names task %q but the stream opened on task %q", taskID, sw.taskID)
	}
	if sw.contextID != "" && contextID != sw.contextID {
		return Errorf(ErrorTypeInvalidAgentResponse,
			"stream frame names context %q but the stream opened on context %q", contextID, sw.contextID)
	}
	return nil
}

// commit folds a successfully written frame into the writer's state machine.
func (sw *SSEWriter) commit(frame StreamResponse) {
	switch frame.Kind() {
	case StreamPayloadTask:
		sw.started = true
		sw.taskID = frame.Task.ID
		sw.contextID = frame.Task.ContextID
		sw.state = frame.Task.Status.State
	case StreamPayloadMessage:
		sw.started = true
		sw.contextID = frame.Message.ContextID
	case StreamPayloadStatusUpdate:
		sw.state = frame.StatusUpdate.Status.State
	}
}

// WriteTask emits the opening Task snapshot.
func (sw *SSEWriter) WriteTask(t Task) error { return sw.Write(StreamTask(t)) }

// WriteMessage emits a direct message reply, which both opens and closes the
// stream.
func (sw *SSEWriter) WriteMessage(m Message) error { return sw.Write(StreamMessage(m)) }

// WriteStatus emits a task status update.
func (sw *SSEWriter) WriteStatus(e TaskStatusUpdateEvent) error {
	return sw.Write(StreamStatusUpdate(e))
}

// WriteArtifact emits a task artifact update.
func (sw *SSEWriter) WriteArtifact(e TaskArtifactUpdateEvent) error {
	return sw.Write(StreamArtifactUpdate(e))
}

// WriteError emits a protocol error as the final record of the stream and
// closes it. The error is framed as a JSON-RPC error response for the JSON-RPC
// binding and as the google.rpc.Status REST body otherwise.
//
// An error frame is the transport-level escape hatch for a failure that cannot
// be expressed as a task state — a version mismatch, an authentication failure
// mid-stream. A failure of the work itself belongs in a FAILED status update,
// not here.
func (sw *SSEWriter) WriteError(protoErr *Error) error {
	if protoErr == nil {
		return fmt.Errorf("a2a: WriteError requires an error")
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.closed {
		return ErrStreamClosed
	}

	var (
		data []byte
		err  error
	)
	if sw.rpcID != nil {
		data, err = NewErrorResponse(sw.rpcID, protoErr).Encode()
	} else {
		_, body := protoErr.RESTError()
		data, err = Encode(body)
	}
	if err != nil {
		return err
	}

	sw.closed = true
	return sw.writeRecord(data)
}

// WriteComment emits an SSE comment record. Comments carry no protocol meaning
// and are the conventional way to keep an idle connection alive through proxies
// with read timeouts — which is exactly what an INPUT_REQUIRED stream needs.
func (sw *SSEWriter) WriteComment(text string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.closed {
		return ErrStreamClosed
	}

	var buf bytes.Buffer
	for _, line := range strings.Split(text, "\n") {
		buf.WriteString(": ")
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	if _, err := sw.w.Write(buf.Bytes()); err != nil {
		sw.closed = true
		return fmt.Errorf("a2a: write sse comment: %w", err)
	}
	sw.flush()
	return nil
}

// Ping emits a keep-alive comment.
func (sw *SSEWriter) Ping() error { return sw.WriteComment("ping") }

// Close marks the stream closed without emitting anything. It is idempotent and
// is what a serve layer calls when the client disconnects.
func (sw *SSEWriter) Close() error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.closed = true
	sw.flush()
	return nil
}

// Flush flushes the underlying writer if it supports flushing.
func (sw *SSEWriter) Flush() {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.flush()
}

// encode frames one payload according to the writer's binding. The caller holds
// the lock.
func (sw *SSEWriter) encode(frame StreamResponse) ([]byte, error) {
	if sw.rpcID == nil {
		return Encode(frame)
	}
	resp, err := NewResultResponse(sw.rpcID, frame)
	if err != nil {
		return nil, err
	}
	return resp.Encode()
}

// writeRecord emits one "data:" SSE record. Multi-line payloads are split
// across several data lines per the SSE grammar; JSON never contains a raw
// newline, but this keeps the writer correct for any payload. The caller holds
// the lock.
func (sw *SSEWriter) writeRecord(payload []byte) error {
	var buf bytes.Buffer
	for _, line := range strings.Split(string(payload), "\n") {
		buf.WriteString("data: ")
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	if _, err := sw.w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("a2a: write sse frame: %w", err)
	}
	sw.flush()
	return nil
}

// flush flushes the underlying writer. The caller holds the lock.
func (sw *SSEWriter) flush() {
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
}

// ---- Reader ----

// SSEReader decodes an A2A SSE stream into StreamResponse frames. It handles
// multi-line "data:" records, comment lines, and blank-line record boundaries,
// and it auto-detects the JSON-RPC and REST framings.
//
// It applies the same stream contract as SSEWriter, so a client detects a
// non-conforming server rather than silently mis-sequencing its own state. A
// contract violation is reported as an *Error of type InvalidAgentResponseError,
// which is the error the specification reserves for exactly that.
//
// SSEReader is not safe for concurrent use; one goroutine owns the stream.
type SSEReader struct {
	sc *bufio.Scanner

	started   bool
	closed    bool
	state     TaskState
	taskID    string
	contextID string
}

// NewSSEReader reads an A2A SSE stream from r.
func NewSSEReader(r io.Reader) *SSEReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxSSERecordBytes)
	return &SSEReader{sc: sc}
}

// State returns the last task state seen on the stream.
func (sr *SSEReader) State() TaskState { return sr.state }

// TaskID returns the task id the stream opened on, empty for a message-only
// stream.
func (sr *SSEReader) TaskID() string { return sr.taskID }

// Closed reports whether the stream has reached a terminating frame: a terminal
// task state, or a direct message reply.
func (sr *SSEReader) Closed() bool { return sr.closed }

// Next reads and decodes the next frame. It returns io.EOF when the stream is
// exhausted, an *Error carrying a server-sent protocol error when the server
// framed one, and an *Error of type InvalidAgentResponseError when the server
// violated the stream contract.
func (sr *SSEReader) Next() (*StreamResponse, error) {
	payload, err := sr.nextRecord()
	if err != nil {
		return nil, err
	}

	frame, protoErr := ParseStreamFrame(payload)
	if protoErr != nil {
		// A server-sent error frame ends the stream.
		sr.closed = true
		return nil, protoErr
	}
	if err := sr.admit(frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// admit applies the stream contract to a decoded frame and folds it into the
// reader's state.
func (sr *SSEReader) admit(frame *StreamResponse) error {
	if sr.closed {
		return Errorf(ErrorTypeInvalidAgentResponse,
			"received a %s frame after the stream reached a terminal state", string(frame.Kind()))
	}

	kind := frame.Kind()
	if !sr.started {
		switch kind {
		case StreamPayloadTask:
			sr.started = true
			sr.taskID = frame.Task.ID
			sr.contextID = frame.Task.ContextID
			sr.state = frame.Task.Status.State
			sr.closed = sr.state.IsTerminal()
			return nil
		case StreamPayloadMessage:
			sr.started = true
			sr.contextID = frame.Message.ContextID
			sr.closed = true
			return nil
		default:
			return Errorf(ErrorTypeInvalidAgentResponse,
				"stream opened with a %s frame; a task or message frame is required", string(kind))
		}
	}

	switch kind {
	case StreamPayloadTask, StreamPayloadMessage:
		return Errorf(ErrorTypeInvalidAgentResponse,
			"received a %s frame mid-stream; only update events may follow the opening frame", string(kind))
	case StreamPayloadStatusUpdate:
		e := frame.StatusUpdate
		if err := sr.checkIdentity(e.TaskID, e.ContextID); err != nil {
			return err
		}
		if err := ValidateTransition(sr.state, e.Status.State); err != nil {
			return err
		}
		sr.state = e.Status.State
		sr.closed = sr.state.IsTerminal()
		return nil
	case StreamPayloadArtifactUpdate:
		e := frame.ArtifactUpdate
		return sr.checkIdentity(e.TaskID, e.ContextID)
	}
	return Errorf(ErrorTypeInvalidAgentResponse, "unknown stream payload kind %q", string(kind))
}

func (sr *SSEReader) checkIdentity(taskID, contextID string) error {
	if sr.taskID != "" && taskID != sr.taskID {
		return Errorf(ErrorTypeInvalidAgentResponse,
			"stream frame names task %q but the stream opened on task %q", taskID, sr.taskID)
	}
	if sr.contextID != "" && contextID != sr.contextID {
		return Errorf(ErrorTypeInvalidAgentResponse,
			"stream frame names context %q but the stream opened on context %q", contextID, sr.contextID)
	}
	return nil
}

// nextRecord scans one SSE record and returns its concatenated data payload.
func (sr *SSEReader) nextRecord() ([]byte, error) {
	var data strings.Builder
	haveData := false

	for sr.sc.Scan() {
		line := sr.sc.Text()

		if line == "" {
			if !haveData {
				// Blank line with no pending data: a heartbeat boundary.
				continue
			}
			return []byte(data.String()), nil
		}
		if strings.HasPrefix(line, ":") {
			// Comment / keep-alive.
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		// A single leading space after the colon is stripped per the SSE grammar.
		value = strings.TrimPrefix(value, " ")

		if field == "data" {
			if haveData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			haveData = true
		}
		// "event", "id" and "retry" are ignored: A2A carries its discriminator
		// inside the JSON payload, not in the SSE event name.
	}

	if err := sr.sc.Err(); err != nil {
		return nil, fmt.Errorf("a2a: read sse stream: %w", err)
	}
	if haveData {
		// Final record without a trailing blank line.
		return []byte(data.String()), nil
	}
	return nil, io.EOF
}

// ReadAll drains the stream into a slice of frames, stopping at io.EOF. A frame
// decoded before the failure is retained, so a caller can inspect how far a
// failing stream got.
func (sr *SSEReader) ReadAll() ([]StreamResponse, error) {
	var frames []StreamResponse
	for {
		frame, err := sr.Next()
		if errors.Is(err, io.EOF) {
			return frames, nil
		}
		if err != nil {
			return frames, err
		}
		frames = append(frames, *frame)
	}
}

// ---- Frame parsing ----

// streamEnvelope is the union of the shapes an SSE data payload may take. All
// three are discriminated structurally, so one unmarshal pass suffices.
type streamEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// ParseStreamFrame decodes one SSE data payload into a StreamResponse,
// accepting either the bare REST framing or the JSON-RPC Response envelope. A
// server-sent error, in either framing, is returned as the *Error it encodes.
func ParseStreamFrame(payload []byte) (*StreamResponse, *Error) {
	if !json.Valid(payload) {
		return nil, NewError(ErrorTypeJSONParse, "stream frame is not valid JSON")
	}

	var env streamEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, Errorf(ErrorTypeInvalidAgentResponse, "decode stream frame: %v", err)
	}

	if env.JSONRPC != "" {
		if len(env.Error) > 0 && !bytes.Equal(env.Error, []byte("null")) {
			var rpcErr RPCError
			if err := json.Unmarshal(env.Error, &rpcErr); err != nil {
				return nil, Errorf(ErrorTypeInvalidAgentResponse, "decode stream error frame: %v", err)
			}
			return nil, rpcErr.AsError()
		}
		if len(env.Result) == 0 {
			return nil, NewError(ErrorTypeInvalidAgentResponse,
				"JSON-RPC stream frame carries neither a result nor an error")
		}
		return decodeStreamPayload(env.Result)
	}

	// A REST error body is {"error":{"code":...,"status":...}}; a StreamResponse
	// never has an "error" member, so the presence of an object there is
	// unambiguous.
	if len(env.Error) > 0 && !bytes.Equal(env.Error, []byte("null")) {
		var body RESTError
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, Errorf(ErrorTypeInvalidAgentResponse, "decode stream error frame: %v", err)
		}
		return nil, body.AsError()
	}

	return decodeStreamPayload(payload)
}

// decodeStreamPayload decodes and validates a bare StreamResponse object.
func decodeStreamPayload(data []byte) (*StreamResponse, *Error) {
	frame, err := DecodeStreamResponse(data)
	if err != nil {
		return nil, Errorf(ErrorTypeInvalidAgentResponse, "decode stream response: %v", err)
	}
	if verr := ValidateStreamResponse(frame); verr != nil {
		return nil, verr
	}
	return frame, nil
}
