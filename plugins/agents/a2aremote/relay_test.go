package a2aremote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

// The middle hop of a three-node chain.
//
// relayAgent is an A2A agent that is itself an A2A client: it forwards the task
// it is given to a downstream agent and mirrors that agent's INPUT_REQUIRED as
// its OWN INPUT_REQUIRED, carrying the question up unchanged. That is precisely
// what a Nexus instance running nexus.io.a2a does when the turn it is serving
// asks a human — its hitl.requested becomes an INPUT_REQUIRED status — so a
// chain built from this relay exercises the same composition a Nexus->Nexus->X
// chain does, without a second engine in the test process.
//
// It speaks the wire through pkg/a2a and pkg/a2a/a2aclient on both legs; nothing
// here hand-rolls a frame.

const (
	relayTaskID    = "relay-task"
	relayContextID = "relay-ctx"
)

type relayAgent struct {
	t      *testing.T
	srv    *httptest.Server
	client *a2aclient.Client
	rec    *resumeRecorder

	mu sync.Mutex
	// downTask / downContext identify the downstream task this relay is
	// shadowing, learned when the downstream first parks.
	downTask    string
	downContext string
}

func newRelayAgent(t *testing.T, downstreamURL string, rec *resumeRecorder) *relayAgent {
	t.Helper()
	client, err := a2aclient.New(downstreamURL)
	if err != nil {
		t.Fatalf("relay: build downstream client: %v", err)
	}
	r := &relayAgent{t: t, client: client, rec: rec}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+a2a.AgentCardPath, r.handleCard)
	mux.HandleFunc("POST "+testJSONRPCPath, r.handleJSONRPC)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *relayAgent) URL() string { return r.srv.URL }

func (r *relayAgent) handleCard(w http.ResponseWriter, _ *http.Request) {
	card := a2a.NewAgentCard("Relay", "forwards work to a downstream A2A agent", "1.0.0")
	card.Capabilities.Streaming = true
	card.SupportedInterfaces = []a2a.AgentInterface{{
		URL:             r.srv.URL + testJSONRPCPath,
		ProtocolBinding: a2a.BindingJSONRPC,
		ProtocolVersion: a2a.ProtocolVersion,
	}}
	card = card.WithSkill(a2a.AgentSkill{ID: "relay", Name: "relay", Description: "forwards a task downstream"})
	body, err := a2a.EncodeAgentCard(&card)
	if err != nil {
		r.t.Errorf("relay: encode card: %v", err)
		return
	}
	w.Header().Set("Content-Type", a2a.ContentTypeAgentCard)
	_, _ = w.Write(body)
}

func (r *relayAgent) handleJSONRPC(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	call, protoErr := a2a.DecodeCall(body)
	if protoErr != nil {
		r.writeError(w, nil, protoErr)
		return
	}
	if call.Method != a2a.MethodSendStreamingMessage {
		r.writeError(w, call.ID(), a2a.ErrUnsupportedOperation(call.Method))
		return
	}

	in := call.Params.(*a2a.SendMessageRequest)
	frames := r.forward(req.Context(), in)

	a2a.WriteSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	sw := a2a.NewJSONRPCSSEWriter(w, call.ID())
	for _, frame := range frames {
		if err := sw.Write(frame); err != nil {
			return
		}
	}
}

// forward runs one leg downstream and renders the result as this relay's own
// task frames.
func (r *relayAgent) forward(ctx context.Context, in *a2a.SendMessageRequest) []a2a.StreamResponse {
	text := messageText(&in.Message)

	var downstream a2a.SendMessageRequest
	if in.Message.TaskID == "" {
		downstream = a2a.SendMessageRequest{Message: a2a.NewUserMessage("relay-1", text)}
	} else {
		// This relay is being resumed. Record it — the test asserts the middle
		// hop was continued under its OWN task id — and resume the downstream
		// task under ITS id. Two hops, two identities, one answer.
		r.rec.record(resumption{
			taskID:    in.Message.TaskID,
			contextID: in.Message.ContextID,
			text:      text,
		})
		r.mu.Lock()
		downTask, downContext := r.downTask, r.downContext
		r.mu.Unlock()
		downstream = a2aclient.ResumeText(downTask, downContext, "relay-2", text)
	}

	res, err := r.client.Run(ctx, downstream)
	if err != nil {
		status := a2a.NewTaskStatus(a2a.TaskStateFailed).
			WithMessage(a2a.NewAgentMessage("relay-fail", "downstream failed: "+err.Error()))
		return []a2a.StreamResponse{
			a2a.StreamTask(a2a.NewTask(relayTaskID, relayContextID)),
			a2a.StreamStatusUpdate(a2a.NewStatusUpdate(relayTaskID, relayContextID, status)),
		}
	}

	if res.State.IsInterrupted() {
		r.mu.Lock()
		r.downTask, r.downContext = res.TaskID, res.ContextID
		r.mu.Unlock()

		// The downstream question becomes THIS agent's question, unchanged. A
		// Nexus middle node does the same thing by a different route: its own
		// hitl.requested is what parks its task.
		status := a2a.NewTaskStatus(a2a.TaskStateInputRequired).
			WithMessage(a2a.NewAgentMessage("relay-ask", res.StatusText()))
		return []a2a.StreamResponse{
			a2a.StreamTask(a2a.NewTask(relayTaskID, relayContextID)),
			a2a.StreamStatusUpdate(a2a.NewStatusUpdate(relayTaskID, relayContextID, status)),
		}
	}

	return []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(relayTaskID, relayContextID)),
		a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(relayTaskID, relayContextID,
			a2a.NewTextArtifact("relay-art", "downstream answer", res.ArtifactText()))),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(relayTaskID, relayContextID,
			a2a.NewTaskStatus(a2a.TaskStateCompleted))),
	}
}

func (r *relayAgent) writeError(w http.ResponseWriter, id json.RawMessage, protoErr *a2a.Error) {
	body, _ := a2a.NewErrorResponse(id, protoErr).Encode()
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	_, _ = w.Write(body)
}
