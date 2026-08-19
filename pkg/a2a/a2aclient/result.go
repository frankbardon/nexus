package a2aclient

import (
	"net/http"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// StreamResult is the accumulated outcome of a streaming call: every frame in
// order, plus the derived view a caller actually wants — who the task is, what
// state it ended in, and what it produced.
//
// The derived view exists because the frame sequence alone is not the answer. A
// task's output arrives as artifact frames that may be CHUNKED: several frames
// carrying the same artifactId, the later ones with append set, the last with
// lastChunk. Reassembling them is the client's job (specification section 11.7)
// and doing it once here is better than every caller doing it differently.
type StreamResult struct {
	// Operation is the A2A operation that produced the stream.
	Operation string
	// Header is the streaming response's header set, which carries the
	// A2A-Version the remote answered with and the extensions it activated.
	Header http.Header

	// Frames is every decoded frame, in arrival order.
	Frames []a2a.StreamResponse

	// TaskID and ContextID are the identity the stream opened on. TaskID is
	// empty for a message-only stream, which has no task.
	TaskID    string
	ContextID string

	// Task is the opening Task snapshot, nil for a message-only stream.
	Task *a2a.Task
	// Status is the latest status observed, from the opening snapshot or the
	// most recent status update.
	Status *a2a.TaskStatus
	// State is the latest task state observed.
	State a2a.TaskState
	// Message is the direct agent reply for a message-only stream, nil
	// otherwise.
	Message *a2a.Message

	// Artifacts are the task's outputs, reassembled from their chunks in first
	// appearance order.
	Artifacts []a2a.Artifact

	// Terminal reports whether the stream reached a terminal task state or a
	// direct message reply, as opposed to ending early.
	Terminal bool
}

// add folds one frame into the result.
func (r *StreamResult) add(frame a2a.StreamResponse) {
	r.Frames = append(r.Frames, frame)

	switch frame.Kind() {
	case a2a.StreamPayloadTask:
		task := *frame.Task
		r.Task = &task
		r.TaskID = task.ID
		r.ContextID = task.ContextID
		status := task.Status
		r.Status = &status
		r.State = task.Status.State
		// A subscribe stream opens on a task that may already carry artifacts;
		// they are the baseline the later chunks extend.
		for _, artifact := range task.Artifacts {
			r.mergeArtifact(artifact, false)
		}
		r.Terminal = task.Status.State.IsTerminal()

	case a2a.StreamPayloadMessage:
		msg := *frame.Message
		r.Message = &msg
		if r.ContextID == "" {
			r.ContextID = msg.ContextID
		}
		if r.TaskID == "" {
			r.TaskID = msg.TaskID
		}
		// A direct message reply is the whole interaction.
		r.Terminal = true

	case a2a.StreamPayloadStatusUpdate:
		e := frame.StatusUpdate
		if r.TaskID == "" {
			r.TaskID = e.TaskID
		}
		if r.ContextID == "" {
			r.ContextID = e.ContextID
		}
		status := e.Status
		r.Status = &status
		r.State = e.Status.State
		r.Terminal = e.Status.State.IsTerminal()

	case a2a.StreamPayloadArtifactUpdate:
		e := frame.ArtifactUpdate
		if r.TaskID == "" {
			r.TaskID = e.TaskID
		}
		if r.ContextID == "" {
			r.ContextID = e.ContextID
		}
		r.mergeArtifact(e.Artifact, e.Append)
	}
}

// mergeArtifact applies one artifact frame's chunk semantics: append extends the
// parts of an artifact already seen, anything else replaces it. An append naming
// an artifact that was never opened is treated as the opening chunk rather than
// dropped — a remote that mislabels its first chunk has still sent content, and
// discarding it would lose output to a cosmetic error.
func (r *StreamResult) mergeArtifact(artifact a2a.Artifact, appendChunk bool) {
	for i := range r.Artifacts {
		if r.Artifacts[i].ArtifactID != artifact.ArtifactID {
			continue
		}
		if !appendChunk {
			r.Artifacts[i] = cloneArtifact(artifact)
			return
		}
		existing := &r.Artifacts[i]
		existing.Parts = append(existing.Parts, artifact.Parts...)
		if artifact.Name != "" {
			existing.Name = artifact.Name
		}
		if artifact.Description != "" {
			existing.Description = artifact.Description
		}
		for k, v := range artifact.Metadata {
			if existing.Metadata == nil {
				existing.Metadata = map[string]any{}
			}
			existing.Metadata[k] = v
		}
		return
	}
	r.Artifacts = append(r.Artifacts, cloneArtifact(artifact))
}

// cloneArtifact copies an artifact deeply enough that appending to the copy's
// parts cannot write through to the frame the caller was handed.
func cloneArtifact(a a2a.Artifact) a2a.Artifact {
	out := a
	out.Parts = append([]a2a.Part(nil), a.Parts...)
	out.Extensions = append([]string(nil), a.Extensions...)
	if a.Metadata != nil {
		out.Metadata = make(map[string]any, len(a.Metadata))
		for k, v := range a.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

// clone returns a copy safe to hand out while the reader goroutine is still
// appending to the original.
func (r StreamResult) clone() StreamResult {
	out := r
	out.Frames = append([]a2a.StreamResponse(nil), r.Frames...)
	out.Artifacts = make([]a2a.Artifact, len(r.Artifacts))
	for i, artifact := range r.Artifacts {
		out.Artifacts[i] = cloneArtifact(artifact)
	}
	return out
}

// Interrupt returns the agent's question and true when the stream ended parked
// in an interrupted state — INPUT_REQUIRED or AUTH_REQUIRED. It is the anchor
// for the resume path: hand the returned message to a human (or an
// authenticator), then send the answer with ResumeRequest.
//
// The message may be nil even when the bool is true: a remote is not obliged to
// attach one, though a useful one does.
func (r StreamResult) Interrupt() (*a2a.Message, bool) {
	if !r.State.IsInterrupted() {
		return nil, false
	}
	if r.Status == nil {
		return nil, true
	}
	return r.Status.Message, true
}

// Failed reports whether the task ended in FAILED or REJECTED. Both are
// terminal and both mean the work did not happen, so a caller that only wants
// to know "did this succeed" checks this rather than enumerating states.
func (r StreamResult) Failed() bool {
	return r.State == a2a.TaskStateFailed || r.State == a2a.TaskStateRejected
}

// Succeeded reports whether the task ended in COMPLETED.
func (r StreamResult) Succeeded() bool { return r.State == a2a.TaskStateCompleted }

// ArtifactText concatenates the text parts of every reassembled artifact, in
// order, separated by newlines. It is the convenience for the common case where
// a remote agent's answer is prose; a caller needing files or structured data
// walks Artifacts itself.
func (r StreamResult) ArtifactText() string {
	var b strings.Builder
	for _, artifact := range r.Artifacts {
		for _, part := range artifact.Parts {
			text, ok := part.TextValue()
			if !ok {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text)
		}
	}
	return b.String()
}

// StatusText returns the text of the message attached to the latest status,
// empty when there is none. An INPUT_REQUIRED question and a FAILED
// explanation both travel this way.
func (r StreamResult) StatusText() string {
	if r.Status == nil || r.Status.Message == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range r.Status.Message.Parts {
		text, ok := part.TextValue()
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

// ActivatedExtensions returns the extension URIs the remote echoed in the
// response's A2A-Extensions header, which is how a server states which of the
// requested extensions it actually activated (specification section 3.5).
func (r StreamResult) ActivatedExtensions() []string {
	if r.Header == nil {
		return nil
	}
	return a2a.ParseExtensions(r.Header.Values(a2a.HeaderExtensions)...)
}
