package a2a

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTaskID = "task-1"
	testCtxID  = "ctx-1"
)

// openStream writes the canonical opening Task frame and returns the writer and
// its backing buffer.
func openStream(t *testing.T) (*SSEWriter, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	sw := NewSSEWriter(&buf)
	task := NewTask(testTaskID, testCtxID)
	if err := sw.WriteTask(task); err != nil {
		t.Fatalf("write opening task: %v", err)
	}
	return sw, &buf
}

func statusFrame(state TaskState) TaskStatusUpdateEvent {
	return NewStatusUpdate(testTaskID, testCtxID, NewTaskStatusAt(state, time.Unix(0, 0)))
}

func artifactFrame(id, text string) TaskArtifactUpdateEvent {
	return NewArtifactUpdate(testTaskID, testCtxID, NewTextArtifact(id, id, text))
}

// readFrames parses an SSE body back into frames using the reader.
func readFrames(t *testing.T, body string) []StreamResponse {
	t.Helper()
	frames, err := NewSSEReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("read frames: %v", err)
	}
	return frames
}

func TestSSEWriterSpecOrder(t *testing.T) {
	sw, buf := openStream(t)

	if err := sw.WriteStatus(statusFrame(TaskStateWorking)); err != nil {
		t.Fatalf("working status: %v", err)
	}
	if err := sw.WriteArtifact(artifactFrame("a1", "hello")); err != nil {
		t.Fatalf("artifact: %v", err)
	}
	if err := sw.WriteStatus(statusFrame(TaskStateCompleted)); err != nil {
		t.Fatalf("completed status: %v", err)
	}

	frames := readFrames(t, buf.String())
	want := []StreamPayloadKind{
		StreamPayloadTask,
		StreamPayloadStatusUpdate,
		StreamPayloadArtifactUpdate,
		StreamPayloadStatusUpdate,
	}
	if len(frames) != len(want) {
		t.Fatalf("got %d frames, want %d", len(frames), len(want))
	}
	for i, kind := range want {
		if got := frames[i].Kind(); got != kind {
			t.Errorf("frame %d: got kind %q, want %q", i, got, kind)
		}
	}
	if frames[0].Task.ID != testTaskID {
		t.Errorf("opening frame task id = %q, want %q", frames[0].Task.ID, testTaskID)
	}
}

func TestSSEWriterRejectsNonOpeningFirstFrame(t *testing.T) {
	cases := map[string]StreamResponse{
		"status":   StreamStatusUpdate(statusFrame(TaskStateWorking)),
		"artifact": StreamArtifactUpdate(artifactFrame("a1", "x")),
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			sw := NewSSEWriter(&buf)
			err := sw.Write(frame)
			if !errors.Is(err, ErrStreamOrder) {
				t.Fatalf("got %v, want ErrStreamOrder", err)
			}
			if buf.Len() != 0 {
				t.Errorf("rejected frame still wrote %d bytes", buf.Len())
			}
		})
	}
}

func TestSSEWriterRejectsSecondOpeningFrame(t *testing.T) {
	sw, _ := openStream(t)
	err := sw.WriteTask(NewTask(testTaskID, testCtxID))
	if !errors.Is(err, ErrStreamOrder) {
		t.Fatalf("second task frame: got %v, want ErrStreamOrder", err)
	}
	err = sw.WriteMessage(NewAgentMessage("m1", "hi"))
	if !errors.Is(err, ErrStreamOrder) {
		t.Fatalf("mid-stream message frame: got %v, want ErrStreamOrder", err)
	}
}

// TestSSEWriterClosesOnEveryTerminalState is the load-bearing termination test:
// the stream must close on FAILED and CANCELED exactly as it does on COMPLETED.
func TestSSEWriterClosesOnEveryTerminalState(t *testing.T) {
	for _, state := range []TaskState{
		TaskStateCompleted,
		TaskStateFailed,
		TaskStateCanceled,
		TaskStateRejected,
	} {
		t.Run(state.String(), func(t *testing.T) {
			sw, buf := openStream(t)
			if err := sw.WriteStatus(statusFrame(TaskStateWorking)); err != nil {
				t.Fatalf("working status: %v", err)
			}
			if sw.Closed() {
				t.Fatal("stream closed while task was still working")
			}

			if err := sw.WriteStatus(statusFrame(state)); err != nil {
				t.Fatalf("terminal status: %v", err)
			}
			if !sw.Closed() {
				t.Fatalf("stream still open after %s", state)
			}
			if got := sw.State(); got != state {
				t.Errorf("writer state = %s, want %s", got, state)
			}

			// Nothing may follow.
			before := buf.Len()
			if err := sw.WriteStatus(statusFrame(TaskStateWorking)); !errors.Is(err, ErrStreamClosed) {
				t.Errorf("status after terminal: got %v, want ErrStreamClosed", err)
			}
			if err := sw.WriteArtifact(artifactFrame("a2", "late")); !errors.Is(err, ErrStreamClosed) {
				t.Errorf("artifact after terminal: got %v, want ErrStreamClosed", err)
			}
			if err := sw.WriteComment("ping"); !errors.Is(err, ErrStreamClosed) {
				t.Errorf("comment after terminal: got %v, want ErrStreamClosed", err)
			}
			if buf.Len() != before {
				t.Errorf("wrote %d bytes after the stream closed", buf.Len()-before)
			}

			// The terminal frame itself was delivered.
			frames := readFrames(t, buf.String())
			last := frames[len(frames)-1]
			if last.StatusUpdate == nil || last.StatusUpdate.Status.State != state {
				t.Fatalf("last frame did not carry the %s status", state)
			}
			if _, terminal := last.TerminalState(); !terminal {
				t.Error("last frame does not report a terminal state")
			}
		})
	}
}

// TestSSEWriterClosesOnTerminalOpeningSnapshot covers SubscribeToTask against an
// already-finished task: the snapshot is delivered and the stream ends.
func TestSSEWriterClosesOnTerminalOpeningSnapshot(t *testing.T) {
	var buf bytes.Buffer
	sw := NewSSEWriter(&buf)
	task := NewTask(testTaskID, testCtxID)
	task.Status = NewTaskStatusAt(TaskStateFailed, time.Unix(0, 0))
	if err := sw.WriteTask(task); err != nil {
		t.Fatalf("write terminal snapshot: %v", err)
	}
	if !sw.Closed() {
		t.Fatal("stream open after a terminal opening snapshot")
	}
	if len(readFrames(t, buf.String())) != 1 {
		t.Fatal("terminal snapshot was not delivered")
	}
}

// TestSSEWriterAcceptsNonSubmittedOpeningSnapshot pins the deliberate decision
// that the opening Task frame is a snapshot, not a transition: SubscribeToTask
// may legitimately open mid-flight.
func TestSSEWriterAcceptsNonSubmittedOpeningSnapshot(t *testing.T) {
	for _, state := range []TaskState{TaskStateWorking, TaskStateInputRequired, TaskStateAuthRequired} {
		t.Run(state.String(), func(t *testing.T) {
			var buf bytes.Buffer
			sw := NewSSEWriter(&buf)
			task := NewTask(testTaskID, testCtxID)
			task.Status = NewTaskStatusAt(state, time.Unix(0, 0))
			if err := sw.WriteTask(task); err != nil {
				t.Fatalf("open on %s: %v", state, err)
			}
			if sw.Closed() {
				t.Fatalf("stream closed on non-terminal opening state %s", state)
			}
		})
	}
}

// TestSSEWriterInputRequiredKeepsStreamOpen pins the documented judgment call:
// an INPUT_REQUIRED interruption is not terminal, so it must not close the
// stream, and the parked condition must be observable so a serving layer can
// apply its own deadline.
func TestSSEWriterInputRequiredKeepsStreamOpen(t *testing.T) {
	sw, buf := openStream(t)
	if err := sw.WriteStatus(statusFrame(TaskStateWorking)); err != nil {
		t.Fatalf("working status: %v", err)
	}

	question := NewAgentMessage("q1", "Which environment should I deploy to?").
		InContext(testCtxID).ForTask(testTaskID)
	ask := statusFrame(TaskStateInputRequired)
	ask.Status = ask.Status.WithMessage(question)
	if err := sw.WriteStatus(ask); err != nil {
		t.Fatalf("input-required status: %v", err)
	}

	if sw.Closed() {
		t.Fatal("stream closed on INPUT_REQUIRED, which is not a terminal state")
	}
	if !sw.Interrupted() {
		t.Fatal("Interrupted() is false while parked on INPUT_REQUIRED")
	}

	// A parked stream still accepts keep-alives.
	if err := sw.Ping(); err != nil {
		t.Fatalf("ping while parked: %v", err)
	}

	// The client answers; the run resumes and completes on the same stream.
	if err := sw.WriteStatus(statusFrame(TaskStateWorking)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if sw.Interrupted() {
		t.Fatal("Interrupted() still true after resuming")
	}
	if err := sw.WriteStatus(statusFrame(TaskStateCompleted)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !sw.Closed() {
		t.Fatal("stream open after COMPLETED")
	}

	frames := readFrames(t, buf.String())
	if len(frames) != 5 {
		t.Fatalf("got %d frames, want 5", len(frames))
	}
	asked := frames[2].StatusUpdate
	if asked.Status.State != TaskStateInputRequired {
		t.Fatalf("frame 2 state = %s, want INPUT_REQUIRED", asked.Status.State)
	}
	if asked.Status.Message == nil {
		t.Fatal("INPUT_REQUIRED frame carries no question message")
	}
	if got, _ := asked.Status.Message.Parts[0].TextValue(); !strings.Contains(got, "environment") {
		t.Errorf("question text = %q", got)
	}
}

// TestSSEWriterInterruptedDeadlineTerminates exercises the documented policy
// hook: a serving layer bounds the wait and ends it through a real terminal
// transition rather than a silent hangup.
func TestSSEWriterInterruptedDeadlineTerminates(t *testing.T) {
	sw, buf := openStream(t)
	if err := sw.WriteStatus(statusFrame(TaskStateInputRequired)); err != nil {
		t.Fatalf("input-required: %v", err)
	}
	if !sw.Interrupted() {
		t.Fatal("not reported as interrupted")
	}

	// Deadline expires: the serving layer fails the task explicitly.
	timeout := statusFrame(TaskStateFailed)
	timeout.Status = timeout.Status.WithMessage(NewAgentMessage("t1", "timed out waiting for input"))
	if err := sw.WriteStatus(timeout); err != nil {
		t.Fatalf("timeout transition: %v", err)
	}
	if !sw.Closed() {
		t.Fatal("stream open after the timeout transition")
	}

	frames := readFrames(t, buf.String())
	last := frames[len(frames)-1]
	if last.StatusUpdate.Status.State != TaskStateFailed {
		t.Fatalf("final state = %s, want FAILED", last.StatusUpdate.Status.State)
	}
}

func TestSSEWriterMessageOnlyStreamCloses(t *testing.T) {
	var buf bytes.Buffer
	sw := NewSSEWriter(&buf)
	if err := sw.WriteMessage(NewAgentMessage("m1", "no task needed")); err != nil {
		t.Fatalf("write message: %v", err)
	}
	if !sw.Closed() {
		t.Fatal("message-only stream stayed open")
	}
	if err := sw.WriteStatus(statusFrame(TaskStateWorking)); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("got %v, want ErrStreamClosed", err)
	}
}

func TestSSEWriterRejectsIllegalTransition(t *testing.T) {
	sw, _ := openStream(t)
	if err := sw.WriteStatus(statusFrame(TaskStateCompleted)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Terminal, so the stream is closed; a fresh writer proves the transition
	// check itself rather than the closed check.
	sw2, _ := openStream(t)
	bad := NewStatusUpdate(testTaskID, testCtxID, TaskStatus{State: "TASK_STATE_BOGUS"})
	var protoErr *Error
	if err := sw2.WriteStatus(bad); !errors.As(err, &protoErr) {
		t.Fatalf("got %v, want *Error", err)
	}
}

func TestSSEWriterRejectsForeignTaskFrame(t *testing.T) {
	sw, _ := openStream(t)
	foreign := NewStatusUpdate("other-task", testCtxID, NewTaskStatusAt(TaskStateWorking, time.Unix(0, 0)))
	var protoErr *Error
	if err := sw.WriteStatus(foreign); !errors.As(err, &protoErr) {
		t.Fatalf("got %v, want *Error", err)
	} else if protoErr.Type != ErrorTypeInvalidAgentResponse {
		t.Errorf("error type = %s, want InvalidAgentResponseError", protoErr.Type)
	}
}

func TestSSEWriterRejectsInvalidFrame(t *testing.T) {
	var buf bytes.Buffer
	sw := NewSSEWriter(&buf)
	if err := sw.Write(StreamResponse{}); err == nil {
		t.Fatal("empty frame accepted")
	}
	if err := sw.Write(StreamResponse{Task: &Task{}, Message: &Message{}}); err == nil {
		t.Fatal("two-arm frame accepted")
	}
}

func TestSSEWriterJSONRPCFraming(t *testing.T) {
	var buf bytes.Buffer
	sw := NewJSONRPCSSEWriter(&buf, json.RawMessage(`"req-7"`))
	if err := sw.WriteTask(NewTask(testTaskID, testCtxID)); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := sw.WriteStatus(statusFrame(TaskStateCompleted)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Every record is a full JSON-RPC envelope repeating the request id.
	for _, payload := range dataPayloads(t, buf.String()) {
		var resp Response
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if resp.JSONRPC != JSONRPCVersion {
			t.Errorf("jsonrpc = %q, want %q", resp.JSONRPC, JSONRPCVersion)
		}
		if string(resp.ID) != `"req-7"` {
			t.Errorf("id = %s, want \"req-7\"", resp.ID)
		}
		if resp.Result == nil {
			t.Error("envelope carries no result")
		}
	}

	// And the reader auto-detects the framing.
	frames := readFrames(t, buf.String())
	if len(frames) != 2 || frames[0].Kind() != StreamPayloadTask {
		t.Fatalf("reader did not unwrap the JSON-RPC framing: %+v", frames)
	}
}

func TestSSEWriterErrorFrames(t *testing.T) {
	t.Run("jsonrpc", func(t *testing.T) {
		var buf bytes.Buffer
		sw := NewJSONRPCSSEWriter(&buf, json.RawMessage(`1`))
		if err := sw.WriteTask(NewTask(testTaskID, testCtxID)); err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := sw.WriteError(ErrTaskNotFound(testTaskID)); err != nil {
			t.Fatalf("write error: %v", err)
		}
		if !sw.Closed() {
			t.Fatal("stream open after an error frame")
		}

		sr := NewSSEReader(strings.NewReader(buf.String()))
		if _, err := sr.Next(); err != nil {
			t.Fatalf("opening frame: %v", err)
		}
		_, err := sr.Next()
		var protoErr *Error
		if !errors.As(err, &protoErr) {
			t.Fatalf("got %v, want *Error", err)
		}
		if protoErr.Type != ErrorTypeTaskNotFound {
			t.Errorf("error type = %s, want TaskNotFoundError", protoErr.Type)
		}
	})

	t.Run("rest", func(t *testing.T) {
		var buf bytes.Buffer
		sw := NewSSEWriter(&buf)
		if err := sw.WriteTask(NewTask(testTaskID, testCtxID)); err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := sw.WriteError(ErrTaskNotCancelable(testTaskID, TaskStateCompleted)); err != nil {
			t.Fatalf("write error: %v", err)
		}

		sr := NewSSEReader(strings.NewReader(buf.String()))
		if _, err := sr.Next(); err != nil {
			t.Fatalf("opening frame: %v", err)
		}
		_, err := sr.Next()
		var protoErr *Error
		if !errors.As(err, &protoErr) {
			t.Fatalf("got %v, want *Error", err)
		}
		if protoErr.Type != ErrorTypeTaskNotCancelable {
			t.Errorf("error type = %s, want TaskNotCancelableError", protoErr.Type)
		}
	})
}

func TestSSEWriterCloseIsIdempotent(t *testing.T) {
	sw, _ := openStream(t)
	if err := sw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := sw.WriteStatus(statusFrame(TaskStateWorking)); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("got %v, want ErrStreamClosed", err)
	}
}

func TestSSEWriterFlushesEveryFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteSSEHeaders(rec.Header())
	sw := NewSSEWriter(rec)
	if err := sw.WriteTask(NewTask(testTaskID, testCtxID)); err != nil {
		t.Fatalf("open: %v", err)
	}
	if !rec.Flushed {
		t.Error("frame was not flushed")
	}
	if got := rec.Header().Get("Content-Type"); got != ContentTypeSSE {
		t.Errorf("Content-Type = %q, want %q", got, ContentTypeSSE)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q", got)
	}
}

func TestSSEWriterConcurrentWritesAreSerialized(t *testing.T) {
	var buf bytes.Buffer
	sw := NewSSEWriter(&buf)
	if err := sw.WriteTask(NewTask(testTaskID, testCtxID)); err != nil {
		t.Fatalf("open: %v", err)
	}

	const writers = 8
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func(i int) {
			defer wg.Done()
			// A mix of frame kinds, some of which will race to be the terminal
			// one. Errors are expected once the stream closes; a torn frame or
			// a data race is not.
			_ = sw.WriteArtifact(artifactFrame("a", strings.Repeat("x", 64)))
			_ = sw.WriteStatus(statusFrame(TaskStateWorking))
			if i == writers-1 {
				_ = sw.WriteStatus(statusFrame(TaskStateCompleted))
			}
			_ = sw.Ping()
		}(i)
	}
	wg.Wait()

	// Every record must still parse: no interleaved bytes.
	if _, err := NewSSEReader(bytes.NewReader(buf.Bytes())).ReadAll(); err != nil {
		t.Fatalf("stream did not parse after concurrent writes: %v", err)
	}
}

// ---- Reader ----

func TestSSEReaderHandlesCommentsAndMultilineData(t *testing.T) {
	frame, err := Encode(StreamTask(NewTask(testTaskID, testCtxID)))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	body := ": keep-alive\n\ndata: " + string(frame) + "\n\n"

	sr := NewSSEReader(strings.NewReader(body))
	got, err := sr.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if got.Kind() != StreamPayloadTask {
		t.Fatalf("kind = %q", got.Kind())
	}
	if _, err := sr.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

func TestSSEReaderAcceptsRecordWithoutTrailingBlankLine(t *testing.T) {
	frame, err := Encode(StreamTask(NewTask(testTaskID, testCtxID)))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	sr := NewSSEReader(strings.NewReader("data: " + string(frame) + "\n"))
	if _, err := sr.Next(); err != nil {
		t.Fatalf("next: %v", err)
	}
}

func TestSSEReaderTracksTerminationAndRejectsLateFrames(t *testing.T) {
	sw, buf := openStream(t)
	if err := sw.WriteStatus(statusFrame(TaskStateCanceled)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Splice on a frame no conforming server would send.
	late, err := Encode(StreamArtifactUpdate(artifactFrame("a9", "late")))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	body := buf.String() + "data: " + string(late) + "\n\n"

	sr := NewSSEReader(strings.NewReader(body))
	if _, err := sr.Next(); err != nil {
		t.Fatalf("frame 0: %v", err)
	}
	if _, err := sr.Next(); err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if !sr.Closed() {
		t.Fatal("reader did not mark the stream closed at CANCELED")
	}
	if sr.State() != TaskStateCanceled {
		t.Errorf("reader state = %s, want CANCELED", sr.State())
	}
	if sr.TaskID() != testTaskID {
		t.Errorf("reader task id = %q", sr.TaskID())
	}

	_, err = sr.Next()
	var protoErr *Error
	if !errors.As(err, &protoErr) {
		t.Fatalf("got %v, want *Error", err)
	}
	if protoErr.Type != ErrorTypeInvalidAgentResponse {
		t.Errorf("error type = %s, want InvalidAgentResponseError", protoErr.Type)
	}
}

func TestSSEReaderRejectsNonOpeningFirstFrame(t *testing.T) {
	frame, err := Encode(StreamStatusUpdate(statusFrame(TaskStateWorking)))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	sr := NewSSEReader(strings.NewReader("data: " + string(frame) + "\n\n"))
	_, err = sr.Next()
	var protoErr *Error
	if !errors.As(err, &protoErr) || protoErr.Type != ErrorTypeInvalidAgentResponse {
		t.Fatalf("got %v, want InvalidAgentResponseError", err)
	}
}

func TestSSEReaderRejectsMalformedFrame(t *testing.T) {
	sr := NewSSEReader(strings.NewReader("data: {not json\n\n"))
	_, err := sr.Next()
	var protoErr *Error
	if !errors.As(err, &protoErr) || protoErr.Type != ErrorTypeJSONParse {
		t.Fatalf("got %v, want JSONParseError", err)
	}
}

// TestSSERoundTripOverHTTP exercises writer and reader across a real HTTP
// server, which is how the a2aclient in E3-S1 will consume the stream.
func TestSSERoundTripOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteSSEHeaders(w.Header())
		w.WriteHeader(http.StatusOK)

		sw := NewSSEWriter(w)
		if err := sw.WriteTask(NewTask(testTaskID, testCtxID)); err != nil {
			t.Errorf("server open: %v", err)
			return
		}
		for _, step := range []string{"planning", "executing"} {
			status := statusFrame(TaskStateWorking)
			status.Status = status.Status.WithMessage(NewAgentMessage("s-"+step, step))
			if err := sw.WriteStatus(status); err != nil {
				t.Errorf("server status: %v", err)
				return
			}
		}
		if err := sw.WriteArtifact(artifactFrame("report", "done")); err != nil {
			t.Errorf("server artifact: %v", err)
			return
		}
		if err := sw.WriteStatus(statusFrame(TaskStateCompleted)); err != nil {
			t.Errorf("server complete: %v", err)
			return
		}
		if !sw.Closed() {
			t.Error("server stream did not close at COMPLETED")
		}
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	ServiceParams{Version: ProtocolVersion, Extensions: []string{NexusExtensionURI}}.Apply(req.Header)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); got != ContentTypeSSE {
		t.Errorf("Content-Type = %q, want %q", got, ContentTypeSSE)
	}

	sr := NewSSEReader(resp.Body)
	frames, err := sr.ReadAll()
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(frames) != 5 {
		t.Fatalf("got %d frames, want 5", len(frames))
	}
	if !sr.Closed() || sr.State() != TaskStateCompleted {
		t.Errorf("reader ended closed=%v state=%s", sr.Closed(), sr.State())
	}
	if frames[3].ArtifactUpdate == nil || !frames[3].ArtifactUpdate.LastChunk {
		t.Error("artifact frame missing or not marked as the last chunk")
	}
}

func TestParseStreamFrameRejectsEmptyJSONRPCEnvelope(t *testing.T) {
	_, err := ParseStreamFrame([]byte(`{"jsonrpc":"2.0","id":1}`))
	if err == nil || err.Type != ErrorTypeInvalidAgentResponse {
		t.Fatalf("got %v, want InvalidAgentResponseError", err)
	}
}

// dataPayloads extracts the raw data payload of every SSE record in a body.
func dataPayloads(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	have := false
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
			continue
		}
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			if have {
				cur.WriteByte('\n')
			}
			cur.WriteString(payload)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}
