package a2aremote

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/events"
)

// Republishing a remote run onto the local bus.
//
// A delegated call takes as long as the remote's work does, and until this file
// existed the only thing a local transport saw was a subagent.started, a long
// silence, and a subagent.complete. That is a black box: an operator watching a
// TUI cannot tell a remote that is working from one that has hung, and the
// browser, AG-UI and A2A-serve transports have nothing to render either.
//
// So each frame the remote streams is mapped onto the bus as it arrives,
// following the precedent nexus.agent.agui_remote set for the AG-UI wire: text
// becomes io.output, discrete progress becomes subagent.iteration, and the
// started/complete pair still brackets the whole call. Nothing here is a new
// event type — a local transport that can already render a local subagent can
// render a remote one without learning anything.
//
// # Which frames say what
//
//   - A non-terminal status update carrying a message is the remote NARRATING.
//     That is A2A's own extension-free progress channel (section 3.1.1), and it
//     becomes io.output.
//   - The Nexus extension's telemetry, riding in a status update's metadata,
//     carries what A2A has no field for: the remote's own tool calls and its own
//     subagent activity. Those become subagent.iteration. This is why the
//     extension is requested by default; see parseConfig.
//   - Artifact frames are NOT republished. An artifact is OUTPUT, and the whole
//     of it is folded into the tool result the calling model reads; emitting it
//     twice would put the remote's answer in the local conversation before the
//     delegating agent had decided what to do with it.
//   - Thinking steps and token accounting are NOT republished. The first is the
//     remote's reasoning, which belongs in the remote's transcript, and the
//     second is the remote's spend under the remote's budget — surfacing either
//     as local progress would misattribute it.
//
// # The INPUT_REQUIRED frame is deliberately silent here
//
// The question a remote parks on is not progress: it is a question, and it is
// routed to a human by hitl.go. Emitting it as io.output as well would show it
// twice and, worse, would put it in front of the delegating MODEL, which is
// exactly the confabulation this feature exists to prevent.

// session is one delegated call in flight: the identity it publishes progress
// under, the remote task it has learned about, and the handle the cancellation
// path needs to reach it.
//
// It is written from the goroutine running the call and read from the bus
// dispatch goroutine handling cancel.active, so every field that both touch is
// behind mu.
type session struct {
	p          *Plugin
	ra         *remote
	spawnID    string
	parentTurn string
	// remoteTurn is the turn id the republished progress is published under. It
	// is derived from the spawn id rather than reusing the caller's turn so a
	// transport can group a remote's output separately from the local turn that
	// asked for it.
	remoteTurn string
	progress   bool

	// cancel aborts the call's context. Set once, before the session is
	// registered, and never reassigned.
	cancel context.CancelFunc

	mu        sync.Mutex
	taskID    string
	contextID string
	iteration int
	// hitlID is the request id of the question currently in front of a human,
	// empty when none is. The cancellation path retracts it.
	hitlID string
	// terminal records that the remote task reached a terminal state, which is
	// what stops the abandonment path from cancelling a task that is finished.
	terminal bool
	// aborted records that this session has already been abandoned, so a cancel
	// arriving twice does not send CancelTask twice.
	aborted bool
}

// newSession builds a session for one call.
func newSession(p *Plugin, ra *remote, spawnID, parentTurn string, cancel context.CancelFunc) *session {
	return &session{
		p:          p,
		ra:         ra,
		spawnID:    spawnID,
		parentTurn: parentTurn,
		remoteTurn: "a2a_remote_" + spawnID,
		progress:   ra.cfg.transport.republishProgress(),
		cancel:     cancel,
	}
}

// task returns the remote task identity observed so far.
func (s *session) task() (taskID, contextID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taskID, s.contextID
}

// noteTask records the task identity the first frame that carries one reveals.
// It is what makes cancellation and resumption possible: neither can address a
// task whose id the client never learned.
func (s *session) noteTask(taskID, contextID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if taskID != "" {
		s.taskID = taskID
	}
	if contextID != "" {
		s.contextID = contextID
	}
}

// noteState records whether the task has settled.
func (s *session) noteState(state a2a.TaskState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state.IsTerminal() {
		s.terminal = true
	}
}

// setHITL records (or clears) the question currently in front of a human.
func (s *session) setHITL(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hitlID = requestID
}

// pendingHITL returns the question currently in front of a human.
func (s *session) pendingHITL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hitlID
}

// nextIteration returns the next observability ordinal for this call.
func (s *session) nextIteration() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.iteration
	s.iteration++
	return n
}

// observe maps one streamed frame onto the local bus and records what the frame
// says about the task's identity and lifecycle.
//
// The recording half runs even when progress republishing is switched off: a
// task id is what CancelTask and the resume path address, and losing it because
// an operator did not want a chatty TUI would break both.
//
// It reports whether this frame PARKED the task on an interruption. That is the
// signal the streaming read loop stops on: a task at INPUT_REQUIRED or
// AUTH_REQUIRED is waiting for its caller, and a caller that keeps reading is
// waiting for it. See exchange.
func (s *session) observe(frame a2a.StreamResponse) (parked bool) {
	switch frame.Kind() {
	case a2a.StreamPayloadTask:
		s.noteTask(frame.Task.ID, frame.Task.ContextID)
		s.noteState(frame.Task.Status.State)
		return frame.Task.Status.State.IsInterrupted()

	case a2a.StreamPayloadMessage:
		s.noteTask(frame.Message.TaskID, frame.Message.ContextID)

	case a2a.StreamPayloadArtifactUpdate:
		s.noteTask(frame.ArtifactUpdate.TaskID, frame.ArtifactUpdate.ContextID)

	case a2a.StreamPayloadStatusUpdate:
		update := frame.StatusUpdate
		s.noteTask(update.TaskID, update.ContextID)
		s.noteState(update.Status.State)
		if s.progress {
			s.republishTelemetry(update.Metadata)
			s.republishNarration(update.Status)
		}
		return update.Status.State.IsInterrupted()
	}
	return false
}

// republishNarration emits a remote's own progress commentary as local output.
//
// Only a non-terminal, non-interrupted status message qualifies. A terminal
// message is the answer (or the failure explanation) and is folded into the
// tool result; an interrupted one is a question and goes to a human.
func (s *session) republishNarration(status a2a.TaskStatus) {
	if status.State.IsTerminal() || status.State.IsInterrupted() {
		return
	}
	text := strings.TrimSpace(messageText(status.Message))
	if text == "" {
		return
	}
	s.p.emitOutput(text, s.remoteTurn)
}

// republishTelemetry maps a Nexus extension payload onto the subagent
// observability family.
//
// A decode failure is logged and dropped rather than surfaced: telemetry is
// additive, and a remote that mislabels an optional extension payload has still
// done the work the caller asked for.
func (s *session) republishTelemetry(metadata map[string]any) {
	if len(metadata) == 0 {
		return
	}
	event, tagged, err := a2a.NexusEventFromMetadata(metadata)
	if !tagged {
		return
	}
	if err != nil {
		s.p.logger.Debug("a2a_remote could not decode remote telemetry",
			"agent", s.ra.cfg.name, "spawn_id", s.spawnID, "error", err)
		return
	}

	switch event.Kind {
	case a2a.NexusEventKindToolCall:
		call := event.ToolCall
		if call == nil {
			return
		}
		s.p.emitIteration(s.spawnID, s.parentTurn, s.nextIteration(), "",
			[]events.ToolCallRequest{{
				ID:        call.CallID,
				Name:      call.Name,
				Arguments: argumentsText(call.Arguments),
			}})

	case a2a.NexusEventKindSubagent:
		sub := event.Subagent
		if sub == nil {
			return
		}
		detail := strings.TrimSpace(sub.Detail)
		if sub.Error != "" {
			detail = strings.TrimSpace(detail + " " + sub.Error)
		}
		if detail == "" {
			return
		}
		s.p.emitIteration(s.spawnID, s.parentTurn, s.nextIteration(),
			s.ra.cfg.name+" subagent "+string(sub.Phase)+": "+oneLine(detail), nil)

	default:
		// tool_result duplicates the artifact already folded into the tool
		// result; thinking and usage belong to the remote. See the file comment.
	}
}

// argumentsText renders a tool call's JSON arguments as the string the
// SubagentIteration payload carries. An absent or unreadable argument object
// becomes an empty string rather than a Go error rendering, because this is
// observability and a mangled blob is worse than nothing.
func argumentsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if !json.Valid(raw) {
		return ""
	}
	return string(raw)
}
