package icm

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// TestEmitHITLAndWait_RoundTrip verifies the request → response loop
// works end-to-end: emit hitl.requested, simulate a hitl.responded
// arrival, get the response back from the waiting goroutine.
func TestEmitHITLAndWait_RoundTrip(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{bus: bus, instanceID: "nexus.workflows.icm", hitlWait: make(map[string]chan events.HITLResponse)}
	bus.Subscribe("hitl.responded", p.handleHITLResponded)

	var seenReq events.HITLRequest
	var seenMu sync.Mutex
	gotReq := make(chan struct{}, 1)
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		seenMu.Lock()
		seenReq, _ = ev.Payload.(events.HITLRequest)
		seenMu.Unlock()
		select {
		case gotReq <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	respCh := make(chan events.HITLResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := p.emitHITLAndWait(ctx, events.HITLRequest{
			SchemaVersion: events.HITLRequestVersion,
			ID:            "icm-predicate-runX-stageY-foo-deadbeef",
			Mode:          events.HITLModeBoth,
			Prompt:        "approve?",
			Choices: []events.HITLChoice{
				{ID: "pass", Label: "Pass"},
				{ID: "fail", Label: "Fail"},
			},
		})
		respCh <- resp
		errCh <- err
	}()

	// Wait for the request to emit, then simulate a response.
	<-gotReq
	seenMu.Lock()
	id := seenReq.ID
	seenMu.Unlock()
	bus.Emit("hitl.responded", events.HITLResponse{
		SchemaVersion: events.HITLResponseVersion,
		RequestID:     id,
		ChoiceID:      "pass",
		FreeText:      "looks good",
	})

	select {
	case resp := <-respCh:
		if resp.ChoiceID != "pass" {
			t.Fatalf("ChoiceID = %q; want pass", resp.ChoiceID)
		}
		if resp.FreeText != "looks good" {
			t.Fatalf("FreeText = %q", resp.FreeText)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emitHITLAndWait did not return")
	}
}

// TestEmitHITLAndWait_CancelEmitsHITLCancel verifies that a context
// cancellation while waiting triggers a hitl.cancel emission so the
// HITL plugin can clean up its persisted request file.
func TestEmitHITLAndWait_CancelEmitsHITLCancel(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{bus: bus, instanceID: "nexus.workflows.icm", hitlWait: make(map[string]chan events.HITLResponse)}

	var cancelSeen events.HITLCancel
	cancelCh := make(chan struct{}, 1)
	bus.Subscribe("hitl.cancel", func(ev engine.Event[any]) {
		cancelSeen, _ = ev.Payload.(events.HITLCancel)
		select {
		case cancelCh <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		_, err := p.emitHITLAndWait(ctx, events.HITLRequest{
			SchemaVersion: events.HITLRequestVersion,
			ID:            "icm-predicate-runX-stageY-bar-cafebabe",
			Mode:          events.HITLModeFreeText,
			Prompt:        "x",
		})
		doneCh <- err
	}()

	// Give the goroutine a moment to register + emit.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-cancelCh:
	case <-time.After(2 * time.Second):
		t.Fatal("hitl.cancel never emitted")
	}
	if cancelSeen.RequestID != "icm-predicate-runX-stageY-bar-cafebabe" {
		t.Fatalf("hitl.cancel RequestID = %q", cancelSeen.RequestID)
	}
	if cancelSeen.Reason == "" {
		t.Fatal("hitl.cancel Reason empty; want ctx error string")
	}

	select {
	case err := <-doneCh:
		if err == nil {
			t.Fatal("emitHITLAndWait returned nil err on ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emitHITLAndWait did not return after cancel")
	}
}

// TestDispatchHumanPredicate_PassChoice covers the happy path: human
// chooses `pass` with optional feedback; dispatcher returns verdict=true.
func TestDispatchHumanPredicate_PassChoice(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{bus: bus, instanceID: "nexus.workflows.icm", hitlWait: make(map[string]chan events.HITLResponse)}
	bus.Subscribe("hitl.responded", p.handleHITLResponded)

	gotReq := make(chan events.HITLRequest, 1)
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.HITLRequest); ok {
			select {
			case gotReq <- r:
			default:
			}
		}
	})

	type result struct {
		verdict  bool
		feedback string
		err      error
	}
	resCh := make(chan result, 1)
	go func() {
		v, fb, err := p.dispatchHumanPredicate(context.Background(), &workspace.Predicate{
			Type:   workspace.PredHuman,
			Name:   "ack",
			Prompt: "do you approve?",
		}, []byte("artifact"), predicates.StageEvalContext{
			RunID: "run-1", StageID: "02_script", Container: "output.validators",
		})
		resCh <- result{v, fb, err}
	}()

	req := <-gotReq
	if req.ActionKind != "icm.predicate" {
		t.Fatalf("ActionKind = %q", req.ActionKind)
	}
	if req.Mode != events.HITLModeBoth {
		t.Fatalf("Mode = %q", req.Mode)
	}
	if len(req.Choices) != 2 {
		t.Fatalf("Choices len = %d", len(req.Choices))
	}
	bus.Emit("hitl.responded", events.HITLResponse{
		SchemaVersion: events.HITLResponseVersion,
		RequestID:     req.ID,
		ChoiceID:      "pass",
		FreeText:      "approved",
	})

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("err = %v", r.err)
		}
		if !r.verdict {
			t.Fatal("verdict = false")
		}
		if r.feedback != "approved" {
			t.Fatalf("feedback = %q", r.feedback)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchHumanPredicate did not return")
	}
}

// TestDispatchHumanPredicate_FailWithoutFeedbackDefaults ensures the
// dispatcher fills in a feedback string when the operator rejects with
// no free-text rationale.
func TestDispatchHumanPredicate_FailWithoutFeedbackDefaults(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{bus: bus, instanceID: "nexus.workflows.icm", hitlWait: make(map[string]chan events.HITLResponse)}
	bus.Subscribe("hitl.responded", p.handleHITLResponded)

	gotReq := make(chan events.HITLRequest, 1)
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.HITLRequest); ok {
			select {
			case gotReq <- r:
			default:
			}
		}
	})

	type result struct {
		verdict  bool
		feedback string
	}
	resCh := make(chan result, 1)
	go func() {
		v, fb, _ := p.dispatchHumanPredicate(context.Background(), &workspace.Predicate{
			Type: workspace.PredHuman, Name: "ack", Prompt: "approve?",
		}, nil, predicates.StageEvalContext{RunID: "r", StageID: "s"})
		resCh <- result{v, fb}
	}()
	req := <-gotReq
	bus.Emit("hitl.responded", events.HITLResponse{
		SchemaVersion: events.HITLResponseVersion,
		RequestID:     req.ID,
		ChoiceID:      "fail",
		// FreeText intentionally empty
	})
	select {
	case r := <-resCh:
		if r.verdict {
			t.Fatal("verdict = true")
		}
		if r.feedback == "" {
			t.Fatal("feedback empty; expected default 'rejected by operator'")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchHumanPredicate did not return")
	}
}

// TestNewHITLID_FormatStable smoke-tests the id format so request-id
// prefix filtering keeps working downstream.
func TestNewHITLID_FormatStable(t *testing.T) {
	id := newHITLID("predicate", "runX", "stageY", "foo")
	if !strings.HasPrefix(id, "icm-predicate-runX-stageY-foo-") {
		t.Fatalf("id %q missing expected prefix", id)
	}
	if len(id) < len("icm-predicate-runX-stageY-foo-")+8 {
		t.Fatalf("id %q too short to contain 8-byte hex", id)
	}
}

// TestJudgeResponseSchemaJSON_Valid ensures the fixed judge response
// schema parses and contains the verdict/feedback fields.
func TestJudgeResponseSchemaJSON_Valid(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(judgeResponseSchemaJSON), &m); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	for _, key := range []string{"verdict", "feedback", "score"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema missing property %q", key)
		}
	}
}

// TestBuildJudgeUserMessage covers the XML user-message shape the judge
// sub-agent receives.
func TestBuildJudgeUserMessage(t *testing.T) {
	msg := buildJudgeUserMessage("Score quality 0..1.", []byte(`{"draft": "..."}`))
	if !strings.Contains(msg, "<judge_task>") || !strings.Contains(msg, "</judge_task>") {
		t.Fatalf("msg missing judge_task tags: %s", msg)
	}
	if !strings.Contains(msg, "<rubric>") || !strings.Contains(msg, "</rubric>") {
		t.Fatalf("msg missing rubric tags: %s", msg)
	}
	if !strings.Contains(msg, "<artifact>") || !strings.Contains(msg, "</artifact>") {
		t.Fatalf("msg missing artifact tags: %s", msg)
	}
	if !strings.Contains(msg, judgeResponseSchemaName) {
		t.Fatalf("msg missing response_schema name reference: %s", msg)
	}
}
