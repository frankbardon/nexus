package a2a

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aconform"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file is nexus.io.a2a's side of the shared A2A conformance corpus.
//
// The corpus lives in pkg/a2a/a2aconform and describes A2A OUTPUT only: it
// names abstract steps ("the agent produced final text", "the agent asked the
// human a question") and pins the exact frame sequence each must produce. This
// driver is the translation from that vocabulary into THIS mapping's, which is
// the engine bus — and it is deliberately the only place in the repository
// where the two meet, because a2aconform must never import a mapping.
//
// The driver drives the plugin through its REAL surfaces rather than reaching
// into the run: a task is created by startTurn, resumed by resumeTurn and
// settled by cancelTask, and everything a turn does in between is a bus event
// that reaches the plugin's own subscriptions. What it skips is HTTP — the
// frames are read off the same attached stream the SSE pump drains, one layer
// below the socket, because a vector asserts the frame sequence and the socket
// contributes nothing to it. tasks_test.go and interrupt_test.go cover the
// wire.

// TestA2AConformance replays the whole shared corpus against this mapping.
func TestA2AConformance(t *testing.T) {
	a2aconform.Run(t, conformDriver{})
}

// conformDriver is the a2aconform.Driver for nexus.io.a2a.
type conformDriver struct{}

func (conformDriver) Name() string { return pluginID }

// Features declares what this mapping can express, which is everything the
// vocabulary names.
//
// It watches the engine bus, so it sees tool results (artifacts), the files
// those results report writing, HITL questions and the errors that fail a turn;
// it owns the per-task artifact budget; and CancelTask is wired. Nothing here is
// declared to make a vector run: each entry is backed by the code the
// corresponding vector exercises.
func (conformDriver) Features() []a2aconform.Feature {
	return []a2aconform.Feature{
		a2aconform.FeatureTurn,
		a2aconform.FeatureFailure,
		a2aconform.FeatureCancel,
		a2aconform.FeatureHITL,
		a2aconform.FeatureToolArtifacts,
		a2aconform.FeatureFileArtifacts,
		a2aconform.FeatureArtifactBudget,
	}
}

// Begin boots one plugin per vector, configured to the vector's artifact policy.
// It deliberately does not start a task: the vector's first step does that, so
// the opening frame is produced by the mapping.
func (conformDriver) Begin(t *testing.T, v a2aconform.Vector, env a2aconform.Env) (a2aconform.Session, error) {
	p, bus := newTestPlugin(t, conformConfig(env))
	s := &conformSession{p: p, bus: bus, vector: v, input: make(chan struct{}, 1)}
	// The plugin emits io.input from a goroutine, so a step that returned before
	// that dispatch had happened would make the replay racy. This is how Apply
	// stays synchronous for the one step that is not.
	bus.Subscribe("io.input", func(engine.Event[any]) {
		select {
		case s.input <- struct{}{}:
		default:
		}
	}, engine.WithSource("test.a2aconform"))
	t.Cleanup(s.Close)
	return s, nil
}

// conformConfig renders a vector's transport-neutral artifact policy as this
// plugin's `artifacts:` block.
//
// A nil field in the vector's policy means "the mapping's own default", so it is
// simply not written: parseArtifacts starts from the defaults and overwrites
// only the keys present.
//
// The input deadline is disabled for every vector. It is a serving-layer
// concern (an unanswered question must not pin the agent loop for ever) and it
// fires from a timer goroutine, so leaving it armed would let a vector that
// ends parked — hitl-parks-stream-open — be failed by the clock instead of
// asserted.
func conformConfig(env a2aconform.Env) map[string]any {
	artifacts := map[string]any{
		// Files a vector says a tool wrote are materialized here by the runner,
		// so this is the directory reported paths must resolve against.
		"file_base_dir": env.FileDir,
	}
	if v := env.Policy.MaxFileBytes; v != nil {
		artifacts["max_file_bytes"] = *v
	}
	if v := env.Policy.MaxToolOutputBytes; v != nil {
		artifacts["max_tool_output_bytes"] = *v
	}
	if v := env.Policy.MaxTaskBytes; v != nil {
		artifacts["max_task_bytes"] = *v
	}
	return map[string]any{
		"artifacts": artifacts,
		"tasks":     map[string]any{"input_timeout": "0s"},
	}
}

// conformSession is one vector's replay against this mapping.
//
// Every driving step either calls a plugin entry point or emits a bus event, and
// both are synchronous: an engine bus dispatch runs its handlers on the calling
// goroutine, and the run enqueues its frames inside that dispatch. So by the
// time Apply returns, every frame the step caused is already in the attached
// stream's buffer and Frames can see it.
type conformSession struct {
	p      *Plugin
	bus    engine.EventBus
	vector a2aconform.Vector

	// input is signalled once the plugin's io.input dispatch has happened.
	input chan struct{}

	run    *run
	stream *stream
	// frames accumulates what has been drained off the stream, opening snapshot
	// first. It is accumulated rather than re-read because the channel is
	// consuming.
	frames []a2a.StreamResponse
	// turnID is the turn the vector bound to, echoed onto the payloads that
	// carry one.
	turnID string
}

// Apply realizes one abstract step in engine-bus vocabulary.
func (s *conformSession) Apply(step a2aconform.Step) error {
	switch step.Kind {
	case a2aconform.StepMessage:
		return s.start(step)
	case a2aconform.StepTurnStart:
		s.turnID = step.TurnID
		return s.emit("agent.turn.start", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion,
			TurnID:        step.TurnID,
		})
	case a2aconform.StepThinking:
		return s.emit("thinking.step", events.ThinkingStep{
			SchemaVersion: events.ThinkingStepVersion,
			TurnID:        s.turnID,
			Source:        "test.agent",
			Content:       step.Text,
		})
	case a2aconform.StepToolCall:
		return s.emit("tool.invoke", events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            step.CallID,
			Name:          step.Tool,
			Arguments:     step.Arguments,
			TurnID:        s.turnID,
		})
	case a2aconform.StepToolResult:
		return s.emit("tool.result", s.toolResult(step))
	case a2aconform.StepAgentText:
		return s.emit("llm.response", events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       step.Text,
			FinishReason:  "end_turn",
		})
	case a2aconform.StepOutput:
		return s.emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion,
			Content:       step.Text,
			Role:          "assistant",
			TurnID:        s.turnID,
		})
	case a2aconform.StepAskUser:
		return s.emit("hitl.requested", events.HITLRequest{
			SchemaVersion:   events.HITLRequestVersion,
			ID:              step.RequestID,
			TurnID:          s.turnID,
			RequesterPlugin: "nexus.control.hitl",
			Prompt:          step.Prompt,
			Choices:         conformChoices(step.Choices),
		})
	case a2aconform.StepAnswer:
		return s.answer(step)
	case a2aconform.StepTurnEnd:
		return s.emit("agent.turn.end", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion,
			TurnID:        step.TurnID,
		})
	case a2aconform.StepFailure:
		// A fatal core.error is the bus shape of "nothing is going to retry
		// this", which is the only kind handleError acts on.
		return s.emit("core.error", events.ErrorInfo{
			SchemaVersion: events.ErrorInfoVersion,
			Source:        "test.agent",
			Err:           errors.New(step.Reason),
			Fatal:         true,
		})
	case a2aconform.StepCancel:
		return s.cancel()
	default:
		return fmt.Errorf("nexus.io.a2a cannot realize step kind %q", step.Kind)
	}
}

// start creates the task, exactly as an inbound SendMessage does.
func (s *conformSession) start(step a2aconform.Step) error {
	if s.run != nil {
		return errors.New("the vector started a second task; this mapping serves one at a time")
	}
	r, sub, opening, protoErr := s.p.startTurn(turnInput{
		contextID: step.ContextID,
		text:      step.Text,
		messageID: step.MessageID,
	}, nexusauth.Principal{}, streamOptions{
		// A driver must attach as a NON-extension observer: the corpus pins the
		// canonical stream, and Nexus extension telemetry is delivered only to
		// clients that asked for it.
		nexusExtension: false,
	})
	if protoErr != nil {
		return fmt.Errorf("startTurn refused the message: %s", protoErr.Message)
	}
	s.run, s.stream = r, sub
	// The opening Task snapshot is the first thing an A2A stream carries
	// (specification section 11.7); the SSE pump writes exactly this.
	s.frames = append(s.frames, a2a.StreamTask(opening))

	select {
	case <-s.input:
	case <-time.After(5 * time.Second):
		return errors.New("the plugin never emitted io.input for the creating message")
	}
	return nil
}

// answer resumes an interrupted task the way A2A defines it: a new message
// naming the same taskId and contextId (specification section 3.4).
//
// The stream that call attaches is dropped immediately. It exists because a real
// client resuming over HTTP wants the rest of the turn on its own connection;
// here the vector is already watching the stream the creating message opened,
// and a second observer would be a second copy of the same frames.
func (s *conformSession) answer(step a2aconform.Step) error {
	if s.run == nil {
		return errors.New("the vector answered a question before any task existed")
	}
	_, sub, _, protoErr := s.p.resumeTurn(turnInput{
		taskID:    s.run.taskID,
		contextID: s.run.contextID,
		text:      step.Text,
		messageID: step.MessageID,
	}, nexusauth.Principal{}, streamOptions{})
	if protoErr != nil {
		return fmt.Errorf("resumeTurn refused the answer: %s", protoErr.Message)
	}
	s.run.detach(sub)
	return nil
}

// cancel settles the task through the plugin's CancelTask path.
//
// The vector's reason is deliberately not passed through: A2A's CancelTask
// carries no client reason, so this mapping states its own. The vector asserts
// only that the status message says the task was canceled, which is exactly the
// part a client can rely on.
func (s *conformSession) cancel() error {
	if s.run == nil {
		return errors.New("the vector canceled a task that was never created")
	}
	if _, protoErr := s.p.cancelTask(nexusauth.Principal{}, s.run.taskID); protoErr != nil {
		return fmt.Errorf("cancelTask refused: %s", protoErr.Message)
	}
	return nil
}

// toolResult renders a tool_result step as the bus payload.
//
// A file the step says the tool wrote is reported through
// events.ToolResult.OutputFile, which is the engine's own field for exactly
// that and applies to every tool. Any structured output the step carries is
// passed through untouched, so a step whose tool also reports its path the
// artifacts.file_sources way (write_file's "path") exercises both routes; the
// mapping deduplicates them by resolved path.
func (s *conformSession) toolResult(step a2aconform.Step) events.ToolResult {
	res := events.ToolResult{
		SchemaVersion:    events.ToolResultVersion,
		ID:               step.CallID,
		Name:             step.Tool,
		Output:           step.Output,
		Error:            step.Error,
		OutputStructured: step.Structured,
		TurnID:           s.turnID,
	}
	if step.File != nil {
		res.OutputFile = step.File.Path
	}
	return res
}

// emit puts one payload on the bus and waits for the dispatch to finish, which
// is what makes Apply synchronous.
func (s *conformSession) emit(eventType string, payload any) error {
	if s.run == nil {
		return fmt.Errorf("the vector emitted %s before any task existed", eventType)
	}
	if err := s.bus.Emit(eventType, payload); err != nil {
		return fmt.Errorf("emitting %s: %w", eventType, err)
	}
	return nil
}

// Frames drains whatever the run has queued and returns the canonical stream so
// far, opening snapshot included.
func (s *conformSession) Frames() []a2a.StreamResponse {
	if s.stream != nil {
		for draining := true; draining; {
			select {
			case frame := <-s.stream.frames:
				s.frames = append(s.frames, frame)
			default:
				draining = false
			}
		}
	}
	return append([]a2a.StreamResponse(nil), s.frames...)
}

// Task returns the run's own task snapshot: the same fold of the same frames a
// blocking SendMessage answers with.
func (s *conformSession) Task() a2a.Task {
	if s.run == nil {
		return a2a.Task{}
	}
	return s.run.snapshotTask()
}

// Close detaches the observer. It is safe on a task that never terminated, and
// safe to call twice: the plugin's own shutdown is registered by newTestPlugin.
func (s *conformSession) Close() {
	if s.run != nil && s.stream != nil {
		s.run.detach(s.stream)
		s.stream = nil
	}
}

// conformChoices maps a vector's multiple-choice options onto the bus payload.
func conformChoices(in []a2aconform.StepChoice) []events.HITLChoice {
	if len(in) == 0 {
		return nil
	}
	out := make([]events.HITLChoice, 0, len(in))
	for _, c := range in {
		out = append(out, events.HITLChoice{ID: c.ID, Label: c.Label})
	}
	return out
}
