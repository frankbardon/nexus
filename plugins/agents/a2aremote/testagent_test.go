package a2aremote

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// A minimal but conformant in-process A2A agent. Every byte it writes goes
// through pkg/a2a's own encoders and SSE writer, so these tests cannot assert
// against a hand-rolled wire shape the codec would reject.

const testJSONRPCPath = "/a2a"

type testAgentConfig struct {
	// cardStatus replaces the card response status; 0 means 200.
	cardStatus int
	// noStreaming makes the card declare capabilities.streaming = false.
	noStreaming bool
	// skills replaces the card's skill list.
	skills []a2a.AgentSkill
	// frames replaces the frames a streaming call answers with.
	frames func(*a2a.SendMessageRequest) []a2a.StreamResponse
	// sendMessage replaces the blocking SendMessage answer.
	sendMessage func(*a2a.SendMessageRequest) (a2a.SendMessageResponse, *a2a.Error)
	// frameDelay is applied before each streamed frame.
	frameDelay time.Duration
	// parkOpen HOLDS a non-terminal stream open after the last frame, writing
	// SSE keep-alive comments until the client goes away, instead of hanging up.
	//
	// That is what nexus.io.a2a does at an INPUT_REQUIRED park, deliberately (see
	// the "Streams parked on INPUT_REQUIRED" section of pkg/a2a/doc.go), and it is
	// the behaviour the ordinary test agent does NOT reproduce: it writes its
	// frames and returns, which closes the connection and hands the client an end
	// of stream it never has to ask for.
	parkOpen bool
	// securitySchemes are added to the card, so a test can drive the
	// credential/scheme mismatch check.
	securitySchemes map[string]a2a.SecurityScheme
	// authorize, when set, gates every request. Returning false answers 401,
	// which is how a test proves a credential actually reached the wire.
	authorize func(*http.Request) bool
	// tlsConf, when set, starts the agent over TLS with it — the mutual-TLS
	// path needs a real handshake, not a stub.
	tlsConf *tls.Config
}

type testAgent struct {
	t   *testing.T
	srv *httptest.Server
	cfg testAgentConfig

	mu       sync.Mutex
	cardHits int
	sendHits int
	// headers records every request's headers, so a test can assert what was
	// presented without the agent having to interpret it.
	headers []http.Header
	// bodies records every A2A request body, so a test can assert what was
	// actually sent to the remote.
	bodies [][]byte
	// cancelled records every task id CancelTask was called for, which is how a
	// test proves a local cancellation reached the remote.
	cancelled []string
	// released counts the parked streams whose client hung up on them. It is
	// how a test proves the client ABANDONED a held-open stream rather than
	// sitting on it until a deadline fired.
	released int
}

func newTestAgent(t *testing.T, cfg testAgentConfig) *testAgent {
	t.Helper()
	a := &testAgent{t: t, cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+a2a.AgentCardPath, a.handleCard)
	mux.HandleFunc("POST "+testJSONRPCPath, a.handleJSONRPC)

	handler := a.record(mux)

	if cfg.tlsConf != nil {
		a.srv = httptest.NewUnstartedServer(handler)
		a.srv.TLS = cfg.tlsConf
		a.srv.StartTLS()
	} else {
		a.srv = httptest.NewServer(handler)
	}
	t.Cleanup(a.srv.Close)
	return a
}

// record captures every request's headers and applies the authorize gate.
func (a *testAgent) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.headers = append(a.headers, r.Header.Clone())
		a.mu.Unlock()
		if a.cfg.authorize != nil && !a.cfg.authorize(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// seenHeaders returns every request header set the agent has observed.
func (a *testAgent) seenHeaders() []http.Header {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]http.Header(nil), a.headers...)
}

func (a *testAgent) URL() string { return a.srv.URL }

func (a *testAgent) counts() (cards, sends int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cardHits, a.sendHits
}

// releasedParks returns how many held-open streams the client hung up on.
func (a *testAgent) releasedParks() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.released
}

// cancelledTasks returns every task id the agent was asked to cancel.
func (a *testAgent) cancelledTasks() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.cancelled...)
}

func (a *testAgent) lastBody() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.bodies) == 0 {
		return nil
	}
	return a.bodies[len(a.bodies)-1]
}

func (a *testAgent) handleCard(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	a.cardHits++
	a.mu.Unlock()

	if a.cfg.cardStatus != 0 && a.cfg.cardStatus != http.StatusOK {
		http.Error(w, "card unavailable", a.cfg.cardStatus)
		return
	}

	card := a2a.NewAgentCard("Test Remote", "an in-process A2A agent for tests", "2.1.0")
	card.Capabilities.Streaming = !a.cfg.noStreaming
	card.SupportedInterfaces = []a2a.AgentInterface{{
		URL:             a.srv.URL + testJSONRPCPath,
		ProtocolBinding: a2a.BindingJSONRPC,
		ProtocolVersion: a2a.ProtocolVersion,
	}}
	skills := a.cfg.skills
	if skills == nil {
		skills = []a2a.AgentSkill{{
			ID:          "research",
			Name:        "deep research",
			Description: "searches many sources and synthesizes an answer",
			Tags:        []string{"research"},
		}}
	}
	for _, skill := range skills {
		card = card.WithSkill(skill)
	}
	for name, scheme := range a.cfg.securitySchemes {
		card = card.WithSecurityScheme(name, scheme)
	}

	body, err := a2a.EncodeAgentCard(&card)
	if err != nil {
		a.t.Errorf("encode card: %v", err)
		return
	}
	w.Header().Set("Content-Type", a2a.ContentTypeAgentCard)
	_, _ = w.Write(body)
}

func (a *testAgent) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.sendHits++
	a.bodies = append(a.bodies, body)
	a.mu.Unlock()

	call, protoErr := a2a.DecodeCall(body)
	if protoErr != nil {
		a.writeError(w, nil, protoErr)
		return
	}

	switch call.Method {
	case a2a.MethodSendMessage:
		req := call.Params.(*a2a.SendMessageRequest)
		resp := a2a.MessageResponse(a2a.NewAgentMessage("m-reply", "blocking reply"))
		if a.cfg.sendMessage != nil {
			var opErr *a2a.Error
			resp, opErr = a.cfg.sendMessage(req)
			if opErr != nil {
				a.writeError(w, call.ID(), opErr)
				return
			}
		}
		a.writeResult(w, call.ID(), resp)

	case a2a.MethodCancelTask:
		req := call.Params.(*a2a.CancelTaskRequest)
		a.mu.Lock()
		a.cancelled = append(a.cancelled, req.ID)
		a.mu.Unlock()
		task := a2a.NewTask(req.ID, "c1")
		task.Status = a2a.NewTaskStatus(a2a.TaskStateCanceled)
		a.writeResult(w, call.ID(), task)

	case a2a.MethodSendStreamingMessage:
		req := call.Params.(*a2a.SendMessageRequest)
		frames := completedRun("task-1", "ctx-1", "the remote's answer")
		if a.cfg.frames != nil {
			frames = a.cfg.frames(req)
		}
		a.stream(w, r, call.ID(), frames)

	default:
		a.writeError(w, call.ID(), a2a.ErrUnsupportedOperation(call.Method))
	}
}

func (a *testAgent) stream(w http.ResponseWriter, r *http.Request, id json.RawMessage, frames []a2a.StreamResponse) {
	a2a.WriteSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	sw := a2a.NewJSONRPCSSEWriter(w, id)
	for _, frame := range frames {
		if a.cfg.frameDelay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(a.cfg.frameDelay):
			}
		}
		if err := sw.Write(frame); err != nil {
			return
		}
	}
	if a.cfg.parkOpen && !sw.Closed() {
		a.hold(r, sw)
	}
}

// hold keeps a non-terminal stream alive with SSE comment records until the
// client hangs up, which is the parked-stream behaviour a conforming server is
// free to choose and nexus.io.a2a does choose.
func (a *testAgent) hold(r *http.Request, sw *a2a.SSEWriter) {
	keepalive := time.NewTicker(20 * time.Millisecond)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			a.mu.Lock()
			a.released++
			a.mu.Unlock()
			return
		case <-keepalive.C:
			if err := sw.Ping(); err != nil {
				return
			}
		}
	}
}

func (a *testAgent) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resp, err := a2a.NewResultResponse(id, result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, _ := resp.Encode()
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	_, _ = w.Write(body)
}

func (a *testAgent) writeError(w http.ResponseWriter, id json.RawMessage, protoErr *a2a.Error) {
	body, _ := a2a.NewErrorResponse(id, protoErr).Encode()
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	_, _ = w.Write(body)
}

// completedRun is the happy path: a submitted task, a working transition, one
// text artifact, then completion.
func completedRun(taskID, contextID, output string) []a2a.StreamResponse {
	return []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(taskID, contextID)),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateWorking))),
		a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(taskID, contextID,
			a2a.NewTextArtifact("art-1", "answer", output))),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateCompleted))),
	}
}

// failedRun ends the task in FAILED with an explanation.
func failedRun(taskID, contextID, why string) []a2a.StreamResponse {
	status := a2a.NewTaskStatus(a2a.TaskStateFailed).
		WithMessage(a2a.NewAgentMessage("m-fail", why))
	return []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(taskID, contextID)),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, status)),
	}
}

// interruptedRun parks the task on INPUT_REQUIRED carrying a question.
func interruptedRun(taskID, contextID, question string) []a2a.StreamResponse {
	status := a2a.NewTaskStatus(a2a.TaskStateInputRequired).
		WithMessage(a2a.NewAgentMessage("m-ask", question))
	return []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(taskID, contextID)),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, status)),
	}
}

// narratedRun reports progress on a WORKING status message before completing,
// which is A2A's own extension-free progress channel.
func narratedRun(taskID, contextID, narration, output string) []a2a.StreamResponse {
	working := a2a.NewTaskStatus(a2a.TaskStateWorking).
		WithMessage(a2a.NewAgentMessage("m-progress", narration))
	return []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(taskID, contextID)),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, working)),
		a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(taskID, contextID,
			a2a.NewTextArtifact("art-1", "answer", output))),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateCompleted))),
	}
}

// telemetryRun carries a Nexus extension event in a status update's metadata,
// which is how a remote Nexus instance reports what A2A has no field for.
func telemetryRun(t *testing.T, taskID, contextID string, event a2a.NexusEvent) []a2a.StreamResponse {
	t.Helper()
	metadata, err := a2a.NexusEventMetadata(event)
	if err != nil {
		t.Fatalf("encode nexus event: %v", err)
	}
	update := a2a.NewStatusUpdate(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateWorking))
	update.Metadata = metadata
	return []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(taskID, contextID)),
		a2a.StreamStatusUpdate(update),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateCompleted))),
	}
}

// resumption records what a resuming message carried, so a test can assert the
// task identity A2A requires of a continuation.
type resumption struct {
	taskID    string
	contextID string
	text      string
}

// resumeRecorder collects the resuming messages a remote received. It is shared
// between the test goroutine and the agent's handler goroutines.
type resumeRecorder struct {
	mu   sync.Mutex
	seen []resumption
}

func (r *resumeRecorder) record(in resumption) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, in)
}

func (r *resumeRecorder) all() []resumption {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]resumption(nil), r.seen...)
}

// askThenAnswerFromSnapshot is askThenAnswer as nexus.io.a2a actually answers a
// resuming message: the continuation stream OPENS on a snapshot of the task as
// it stands, which is the INPUT_REQUIRED being answered, and only then carries
// the transitions the answer caused.
//
// A client that treats every interrupted frame as a fresh question would re-ask
// the human the question they just answered, round after round, so this shape is
// the regression guard for the opening frame of a continuation.
func askThenAnswerFromSnapshot(taskID, contextID, question string, rec *resumeRecorder) func(*a2a.SendMessageRequest) []a2a.StreamResponse {
	return func(req *a2a.SendMessageRequest) []a2a.StreamResponse {
		if req.Message.TaskID == "" {
			return interruptedRun(taskID, contextID, question)
		}
		rec.record(resumption{
			taskID:    req.Message.TaskID,
			contextID: req.Message.ContextID,
			text:      messageText(&req.Message),
		})
		parked := a2a.NewTask(taskID, contextID)
		parked.Status = a2a.NewTaskStatus(a2a.TaskStateInputRequired).
			WithMessage(a2a.NewAgentMessage("m-ask", question))
		return append([]a2a.StreamResponse{a2a.StreamTask(parked)},
			completedRun(taskID, contextID, "the answer was "+messageText(&req.Message))[1:]...)
	}
}

// askThenAnswer parks on a question, then completes once the resuming message
// arrives. The recorded resumption is what proves the continuation addressed the
// SAME task and context rather than starting a new conversation.
func askThenAnswer(taskID, contextID, question string, rec *resumeRecorder) func(*a2a.SendMessageRequest) []a2a.StreamResponse {
	return func(req *a2a.SendMessageRequest) []a2a.StreamResponse {
		if req.Message.TaskID == "" {
			return interruptedRun(taskID, contextID, question)
		}
		rec.record(resumption{
			taskID:    req.Message.TaskID,
			contextID: req.Message.ContextID,
			text:      messageText(&req.Message),
		})
		return completedRun(taskID, contextID, "the answer was "+messageText(&req.Message))
	}
}

// alwaysAsking never settles: every message, first or resuming, parks the task
// on another question. It is how the round cap is exercised.
func alwaysAsking(taskID, contextID, question string) func(*a2a.SendMessageRequest) []a2a.StreamResponse {
	return func(*a2a.SendMessageRequest) []a2a.StreamResponse {
		return interruptedRun(taskID, contextID, question)
	}
}
