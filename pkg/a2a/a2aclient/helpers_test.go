package a2aclient_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

// A conformant in-process A2A agent, used by every test that is not
// specifically about a NON-conformant remote. It serves the well-known card and
// both HTTP bindings from one httptest server, dispatching through pkg/a2a's
// own decoders so a test cannot accidentally assert against a hand-rolled wire
// shape the codec would reject.

const (
	testJSONRPCPath = "/a2a"
	testRESTPrefix  = "/a2a/v1"
)

type agentConfig struct {
	// noStreaming makes the card declare capabilities.streaming = false.
	noStreaming bool
	// cardBody replaces the served card bytes verbatim.
	cardBody []byte
	// cardStatus replaces the card response status.
	cardStatus int
	// cardInterfaces replaces the card's supported interfaces.
	cardInterfaces []a2a.AgentInterface
	// securitySchemes are added to the card.
	securitySchemes map[string]a2a.SecurityScheme
	// requireAuth, when set, refuses any A2A request whose Authorization
	// header does not match.
	requireAuth string

	sendMessage     func(*a2a.SendMessageRequest) (a2a.SendMessageResponse, *a2a.Error)
	streamFrames    func(*a2a.SendMessageRequest) []a2a.StreamResponse
	subscribeFrames func(*a2a.SubscribeToTaskRequest) []a2a.StreamResponse
	getTask         func(*a2a.GetTaskRequest) (a2a.Task, *a2a.Error)
	cancelTask      func(*a2a.CancelTaskRequest) (a2a.Task, *a2a.Error)

	// frameDelay is applied before each streamed frame.
	frameDelay time.Duration
	// holdOpen keeps a stream open after its frames until the request context
	// is cancelled, which is how a cancellation test gets something to cancel.
	holdOpen bool
}

type recorded struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type agent struct {
	t   *testing.T
	srv *httptest.Server
	cfg agentConfig

	mu       sync.Mutex
	requests []recorded
}

func newAgent(t *testing.T, cfg agentConfig) *agent {
	t.Helper()
	a := &agent{t: t, cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+a2a.AgentCardPath, a.handleCard)
	mux.HandleFunc("POST "+testJSONRPCPath, a.handleJSONRPC)
	mux.HandleFunc(testRESTPrefix+"/", a.handleREST)

	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *agent) URL() string { return a.srv.URL }

func (a *agent) seen() []recorded {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recorded(nil), a.requests...)
}

func (a *agent) record(r *http.Request, body []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, recorded{
		Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body,
	})
}

func (a *agent) card() a2a.AgentCard {
	card := a2a.NewAgentCard("test-agent", "an in-process A2A agent", "1.0.0")
	card.Capabilities.Streaming = !a.cfg.noStreaming
	if a.cfg.cardInterfaces != nil {
		card.SupportedInterfaces = a.cfg.cardInterfaces
	} else {
		card.SupportedInterfaces = []a2a.AgentInterface{
			{URL: a.srv.URL + testJSONRPCPath, ProtocolBinding: a2a.BindingJSONRPC, ProtocolVersion: a2a.ProtocolVersion},
			{URL: a.srv.URL + testRESTPrefix, ProtocolBinding: a2a.BindingHTTPJSON, ProtocolVersion: a2a.ProtocolVersion},
		}
	}
	card = card.WithSkill(a2a.AgentSkill{
		ID:          "chat",
		Name:        "chat",
		Description: "answers questions",
		Tags:        []string{"chat"},
	})
	for name, scheme := range a.cfg.securitySchemes {
		card = card.WithSecurityScheme(name, scheme).WithSecurityRequirement(name)
	}
	return card
}

func (a *agent) handleCard(w http.ResponseWriter, r *http.Request) {
	a.record(r, nil)
	if a.cfg.cardStatus != 0 && a.cfg.cardStatus != http.StatusOK {
		http.Error(w, "card unavailable", a.cfg.cardStatus)
		return
	}
	body := a.cfg.cardBody
	if body == nil {
		card := a.card()
		encoded, err := a2a.EncodeAgentCard(&card)
		if err != nil {
			a.t.Errorf("encode card: %v", err)
			return
		}
		body = encoded
	}
	w.Header().Set("Content-Type", a2a.ContentTypeAgentCard)
	_, _ = w.Write(body)
}

// authorized enforces the configured bearer token and the mandatory A2A-Version
// service parameter. Both refusals are the ones a real agent makes, so a client
// that forgets either is caught here rather than in production.
func (a *agent) authorized(r *http.Request) *a2a.Error {
	if _, protoErr := a2a.ParseServiceParams(r.Header, r.URL.Query()); protoErr != nil {
		return protoErr
	}
	if a.cfg.requireAuth == "" {
		return nil
	}
	if r.Header.Get("Authorization") != a.cfg.requireAuth {
		return a2a.Errorf(a2a.ErrorTypeUnsupportedOperation, "not authorized")
	}
	return nil
}

func (a *agent) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.record(r, body)

	if protoErr := a.authorized(r); protoErr != nil {
		writeJSONRPCError(w, nil, protoErr)
		return
	}
	call, protoErr := a2a.DecodeCall(body)
	if protoErr != nil {
		writeJSONRPCError(w, nil, protoErr)
		return
	}

	switch call.Method {
	case a2a.MethodSendMessage:
		resp, opErr := a.doSend(call.Params.(*a2a.SendMessageRequest))
		if opErr != nil {
			writeJSONRPCError(w, call.ID(), opErr)
			return
		}
		writeJSONRPCResult(w, call.ID(), resp)
	case a2a.MethodSendStreamingMessage:
		a.streamOut(w, r, call.ID(), a.frames(call.Params.(*a2a.SendMessageRequest)))
	case a2a.MethodSubscribeToTask:
		a.streamOut(w, r, call.ID(), a.subFrames(call.Params.(*a2a.SubscribeToTaskRequest)))
	case a2a.MethodGetTask:
		task, opErr := a.doGet(call.Params.(*a2a.GetTaskRequest))
		if opErr != nil {
			writeJSONRPCError(w, call.ID(), opErr)
			return
		}
		writeJSONRPCResult(w, call.ID(), task)
	case a2a.MethodCancelTask:
		task, opErr := a.doCancel(call.Params.(*a2a.CancelTaskRequest))
		if opErr != nil {
			writeJSONRPCError(w, call.ID(), opErr)
			return
		}
		writeJSONRPCResult(w, call.ID(), task)
	default:
		writeJSONRPCError(w, call.ID(), a2a.ErrUnsupportedOperation(call.Method))
	}
}

func (a *agent) handleREST(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.record(r, body)

	if protoErr := a.authorized(r); protoErr != nil {
		writeRESTError(w, protoErr)
		return
	}

	suffix := strings.TrimPrefix(r.URL.Path, testRESTPrefix)
	route, vars, found, _ := a2a.MatchRoute(r.Method, suffix)
	if !found {
		writeRESTError(w, a2a.Errorf(a2a.ErrorTypeMethodNotFound, "no operation at %s", suffix))
		return
	}

	switch route.Operation {
	case a2a.MethodSendMessage:
		req, protoErr := a2a.DecodeSendMessageRequest(body)
		if protoErr != nil {
			writeRESTError(w, protoErr)
			return
		}
		resp, opErr := a.doSend(req)
		if opErr != nil {
			writeRESTError(w, opErr)
			return
		}
		writeRESTResult(w, resp)
	case a2a.MethodSendStreamingMessage:
		req, protoErr := a2a.DecodeSendMessageRequest(body)
		if protoErr != nil {
			writeRESTError(w, protoErr)
			return
		}
		a.streamOut(w, r, nil, a.frames(req))
	case a2a.MethodSubscribeToTask:
		req, protoErr := a2a.DecodeSubscribeToTaskRequest(vars["id"], body)
		if protoErr != nil {
			writeRESTError(w, protoErr)
			return
		}
		a.streamOut(w, r, nil, a.subFrames(req))
	case a2a.MethodGetTask:
		req, protoErr := a2a.ParseGetTaskQuery(vars["id"], r.URL.Query())
		if protoErr != nil {
			writeRESTError(w, protoErr)
			return
		}
		task, opErr := a.doGet(req)
		if opErr != nil {
			writeRESTError(w, opErr)
			return
		}
		writeRESTResult(w, task)
	case a2a.MethodCancelTask:
		req, protoErr := a2a.DecodeCancelTaskRequest(vars["id"], body)
		if protoErr != nil {
			writeRESTError(w, protoErr)
			return
		}
		task, opErr := a.doCancel(req)
		if opErr != nil {
			writeRESTError(w, opErr)
			return
		}
		writeRESTResult(w, task)
	default:
		writeRESTError(w, a2a.ErrUnsupportedOperation(route.Operation))
	}
}

func (a *agent) doSend(req *a2a.SendMessageRequest) (a2a.SendMessageResponse, *a2a.Error) {
	if a.cfg.sendMessage != nil {
		return a.cfg.sendMessage(req)
	}
	return a2a.MessageResponse(a2a.NewAgentMessage("m-reply", "ack")), nil
}

func (a *agent) doGet(req *a2a.GetTaskRequest) (a2a.Task, *a2a.Error) {
	if a.cfg.getTask != nil {
		return a.cfg.getTask(req)
	}
	return a2a.NewTask(req.ID, "ctx-1"), nil
}

func (a *agent) doCancel(req *a2a.CancelTaskRequest) (a2a.Task, *a2a.Error) {
	if a.cfg.cancelTask != nil {
		return a.cfg.cancelTask(req)
	}
	task := a2a.NewTask(req.ID, "ctx-1")
	task.Status = a2a.NewTaskStatus(a2a.TaskStateCanceled)
	return task, nil
}

func (a *agent) frames(req *a2a.SendMessageRequest) []a2a.StreamResponse {
	if a.cfg.streamFrames != nil {
		return a.cfg.streamFrames(req)
	}
	return completedRun("task-1", "ctx-1", "done")
}

func (a *agent) subFrames(req *a2a.SubscribeToTaskRequest) []a2a.StreamResponse {
	if a.cfg.subscribeFrames != nil {
		return a.cfg.subscribeFrames(req)
	}
	return completedRun(req.ID, "ctx-1", "done")
}

// streamOut writes frames through the codec's own SSE writer, so the test agent
// cannot emit a sequence the specification forbids.
func (a *agent) streamOut(w http.ResponseWriter, r *http.Request, id json.RawMessage, frames []a2a.StreamResponse) {
	a2a.WriteSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)

	var sw *a2a.SSEWriter
	if id != nil {
		sw = a2a.NewJSONRPCSSEWriter(w, id)
	} else {
		sw = a2a.NewSSEWriter(w)
	}

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
	if a.cfg.holdOpen {
		<-r.Context().Done()
	}
}

func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resp, err := a2a.NewResultResponse(id, result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, _ := resp.Encode()
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	_, _ = w.Write(body)
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, protoErr *a2a.Error) {
	body, _ := a2a.NewErrorResponse(id, protoErr).Encode()
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	_, _ = w.Write(body)
}

func writeRESTResult(w http.ResponseWriter, result any) {
	body, _ := a2a.Encode(result)
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	_, _ = w.Write(body)
}

func writeRESTError(w http.ResponseWriter, protoErr *a2a.Error) {
	status, envelope := protoErr.RESTError()
	body, _ := a2a.Encode(envelope)
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ---- Frame builders ----

// completedRun is the canonical happy path: a submitted task, a working
// transition, one artifact, then completion.
func completedRun(taskID, contextID, output string) []a2a.StreamResponse {
	return []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(taskID, contextID)),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateWorking))),
		a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(taskID, contextID,
			a2a.NewTextArtifact("art-1", "answer", output))),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateCompleted))),
	}
}

// interruptedRun parks a task on INPUT_REQUIRED carrying a question.
func interruptedRun(taskID, contextID, question string) []a2a.StreamResponse {
	status := a2a.NewTaskStatus(a2a.TaskStateInputRequired).
		WithMessage(a2a.NewAgentMessage("m-ask", question))
	return []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(taskID, contextID)),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateWorking))),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(taskID, contextID, status)),
	}
}

// ---- Raw servers, for testing non-conformant remotes ----

// rawServer serves whatever the handler writes, with a card that points both
// bindings at the same path. It is how a test produces wire shapes the codec
// would refuse to emit.
func rawServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// sseBody frames each payload as one SSE data record.
func sseBody(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: ")
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	return b.String()
}

// mustClient builds a client pinned to an endpoint, which skips discovery.
func mustClient(t *testing.T, opts ...a2aclient.Option) *a2aclient.Client {
	t.Helper()
	c, err := a2aclient.New("", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// ---- Goroutine leak detection ----
//
// The repo vendors no goroutine-leak library, so this asserts directly: sample
// the count, run the scenario, and wait for it to come back down. The wait is
// necessary because a cancelled request's goroutines unwind asynchronously; a
// leak is a count that never returns, not one that is briefly high.

func goroutineBaseline() int { return runtime.NumGoroutine() }

func assertNoLeakedGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		current := runtime.NumGoroutine()
		if current <= baseline {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine leak: baseline %d, still %d after 3s\n%s", baseline, current, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// noKeepAlive builds an HTTP client whose transport keeps no idle connections,
// so the goroutine baseline is not polluted by pooled connection readers.
func noKeepAlive() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}
