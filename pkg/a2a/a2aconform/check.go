package a2aconform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// Check compares one observation against one vector and returns every way they
// disagree.
//
// It is a PURE function of the vector and the observation, with no *testing.T
// and no driver, for two reasons. It lets a test feed it a deliberately-wrong
// observation and assert that the disagreement is caught, which is the only way
// to know this harness is not vacuously green. And it keeps the whole oracle in
// one place, so a second mapping is judged by exactly the same code as the
// first rather than by a second copy of the rules that can drift.
func Check(v Vector, obs Observation) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if len(obs.Frames) == 0 {
		add("no frames were produced; the vector expects %d", len(v.Expect.Frames))
		return errs
	}
	if obs.Frames[0].Kind() != a2a.StreamPayloadTask {
		add("the stream opened with a %s frame; specification section 11.7 requires a task snapshot",
			obs.Frames[0].Kind())
		return errs
	}
	ids := identity{taskID: obs.Frames[0].Task.ID, contextID: obs.Frames[0].Task.ContextID}
	if ids.taskID == "" {
		add("the opening task frame carries no task id")
	}

	errs = append(errs, checkFrames(v, obs, ids)...)
	errs = append(errs, checkStreamContract(v, obs)...)
	errs = append(errs, checkOrdering(v, obs)...)
	errs = append(errs, checkAssertions(v, obs)...)
	errs = append(errs, checkSnapshot(v, obs, ids)...)
	return errs
}

// identity is the runtime-minted task and context the corpus refers to through
// the {taskId} and {contextId} placeholders.
type identity struct {
	taskID    string
	contextID string
}

func (id identity) expand(s string) string {
	s = strings.ReplaceAll(s, "{taskId}", id.taskID)
	return strings.ReplaceAll(s, "{contextId}", id.contextID)
}

// checkFrames compares the observed frame sequence against the expected one,
// position by position.
func checkFrames(v Vector, obs Observation, ids identity) []error {
	var errs []error
	want, got := v.Expect.Frames, obs.Frames
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		for _, err := range checkFrame(want[i], got[i], ids) {
			errs = append(errs, fmt.Errorf("frame %d: %w", i, err))
		}
	}
	switch {
	case len(got) > len(want):
		for _, extra := range got[len(want):] {
			errs = append(errs, fmt.Errorf("unexpected extra frame: %s", describe(extra)))
		}
	case len(got) < len(want):
		for _, missing := range want[len(got):] {
			errs = append(errs, fmt.Errorf("missing frame: %s %s", missing.Kind, missing.State))
		}
	}
	return errs
}

// checkFrame compares one frame.
func checkFrame(want FrameExpect, got a2a.StreamResponse, ids identity) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch want.Kind {
	case FrameTask:
		if got.Kind() != a2a.StreamPayloadTask {
			add("want a task frame, got %s (%s)", got.Kind(), describe(got))
			return errs
		}
		if want.State != "" && got.Task.Status.State != want.State {
			add("task state = %s, want %s", got.Task.Status.State, want.State)
		}
		errs = append(errs, checkStatusMessage(want, got.Task.Status, ids)...)
	case FrameMessage:
		if got.Kind() != a2a.StreamPayloadMessage {
			add("want a message frame, got %s (%s)", got.Kind(), describe(got))
		}
	case FrameStatus:
		if got.Kind() != a2a.StreamPayloadStatusUpdate {
			add("want a status frame, got %s (%s)", got.Kind(), describe(got))
			return errs
		}
		e := got.StatusUpdate
		if want.State != "" && e.Status.State != want.State {
			add("status state = %s, want %s", e.Status.State, want.State)
		}
		if e.TaskID != ids.taskID {
			add("status names task %q, want %q", e.TaskID, ids.taskID)
		}
		if e.ContextID != ids.contextID {
			add("status names context %q, want %q", e.ContextID, ids.contextID)
		}
		errs = append(errs, checkStatusMessage(want, e.Status, ids)...)
	case FrameArtifact:
		if got.Kind() != a2a.StreamPayloadArtifactUpdate {
			add("want an artifact frame, got %s (%s)", got.Kind(), describe(got))
			return errs
		}
		errs = append(errs, checkArtifactFrame(want, *got.ArtifactUpdate, ids)...)
	default:
		add("unknown expected frame kind %q", want.Kind)
	}
	return errs
}

// checkStatusMessage compares the message riding a task or status frame.
func checkStatusMessage(want FrameExpect, status a2a.TaskStatus, ids identity) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if want.NoMessage {
		if status.Message != nil {
			add("status carries a message, want none")
		}
		return errs
	}
	needsMessage := want.MessageRole != "" || len(want.MessageContains) > 0 || len(want.MessageMetadata) > 0
	if !needsMessage {
		return errs
	}
	if status.Message == nil {
		add("status carries no message; want one containing %v", want.MessageContains)
		return errs
	}
	msg := status.Message
	if want.MessageRole != "" && msg.Role != want.MessageRole {
		add("status message role = %s, want %s", msg.Role, want.MessageRole)
	}
	text := messageText(*msg)
	for _, sub := range want.MessageContains {
		if !strings.Contains(text, ids.expand(sub)) {
			add("status message text %q does not contain %q", text, ids.expand(sub))
		}
	}
	for key, wantValue := range want.MessageMetadata {
		errs = append(errs, checkMetadataEntry("status message", msg.Metadata, key, wantValue, ids)...)
	}
	return errs
}

// checkArtifactFrame compares one artifact update.
func checkArtifactFrame(want FrameExpect, got a2a.TaskArtifactUpdateEvent, ids identity) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if got.TaskID != ids.taskID {
		add("artifact names task %q, want %q", got.TaskID, ids.taskID)
	}
	if got.ContextID != ids.contextID {
		add("artifact names context %q, want %q", got.ContextID, ids.contextID)
	}
	art := got.Artifact
	if want.ArtifactID != "" {
		if wantID := ids.expand(want.ArtifactID); art.ArtifactID != wantID {
			add("artifactId = %q, want %q", art.ArtifactID, wantID)
		}
	}
	if want.ArtifactName != "" && art.Name != want.ArtifactName {
		add("artifact name = %q, want %q", art.Name, want.ArtifactName)
	}
	if want.LastChunk != nil && got.LastChunk != *want.LastChunk {
		add("lastChunk = %v, want %v", got.LastChunk, *want.LastChunk)
	}
	if want.Append != nil && got.Append != *want.Append {
		add("append = %v, want %v", got.Append, *want.Append)
	}
	for key, wantValue := range want.Metadata {
		errs = append(errs, checkMetadataEntry("artifact", art.Metadata, key, wantValue, ids)...)
	}
	for _, key := range want.MetadataAbsent {
		if _, present := art.Metadata[key]; present {
			add("artifact metadata carries %q, want it absent", key)
		}
	}
	if want.Parts == nil {
		return errs
	}
	if len(art.Parts) != len(want.Parts) {
		add("artifact has %d part(s), want %d", len(art.Parts), len(want.Parts))
	}
	n := len(want.Parts)
	if len(art.Parts) < n {
		n = len(art.Parts)
	}
	for i := 0; i < n; i++ {
		for _, err := range checkPart(want.Parts[i], art.Parts[i], ids) {
			errs = append(errs, fmt.Errorf("part %d: %w", i, err))
		}
	}
	return errs
}

// checkPart compares one artifact part.
func checkPart(want PartExpect, got a2a.Part, ids identity) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if want.Kind != "" && got.Kind() != want.Kind {
		add("kind = %q, want %q", got.Kind(), want.Kind)
		return errs
	}
	if want.MediaType != "" && got.MediaType != want.MediaType {
		add("mediaType = %q, want %q", got.MediaType, want.MediaType)
	}
	if want.Filename != "" && got.Filename != want.Filename {
		add("filename = %q, want %q", got.Filename, want.Filename)
	}
	if want.Equals != nil {
		text, ok := got.TextValue()
		if !ok || text != ids.expand(*want.Equals) {
			add("text = %q, want %q", text, ids.expand(*want.Equals))
		}
	}
	for _, sub := range want.Contains {
		text, ok := got.TextValue()
		if !ok || !strings.Contains(text, ids.expand(sub)) {
			add("text %q does not contain %q", text, ids.expand(sub))
		}
	}
	if len(want.JSONEquals) > 0 {
		if !sameJSON(want.JSONEquals, got.Data) {
			add("data = %s, want %s", string(got.Data), string(want.JSONEquals))
		}
	}
	if want.Bytes != nil && len(got.Raw) != *want.Bytes {
		add("raw content is %d bytes, want %d", len(got.Raw), *want.Bytes)
	}
	return errs
}

// checkMetadataEntry compares one metadata value as a JSON value, so an int, an
// int64 and a float64 carrying the same number all match the corpus's number.
func checkMetadataEntry(what string, meta map[string]any, key string, wantValue any, ids identity) []error {
	got, present := meta[key]
	if !present {
		return []error{fmt.Errorf("%s metadata has no %q; want %v", what, key, wantValue)}
	}
	if s, ok := wantValue.(string); ok {
		wantValue = ids.expand(s)
	}
	if !sameValue(got, wantValue) {
		return []error{fmt.Errorf("%s metadata[%q] = %#v, want %#v", what, key, got, wantValue)}
	}
	return nil
}

// checkStreamContract replays the observed frames through a2a.SSEWriter.
//
// This is the independent half of the oracle. The frame comparison above says
// "the mapping produced what the vector describes"; this says "what it produced
// is a legal A2A stream" — opens with a task, keeps task and context identity
// stable, makes only legal state transitions, and writes nothing after a
// terminal frame — as judged by the codec both mappings already share rather
// than by a rule restated here.
func checkStreamContract(v Vector, obs Observation) []error {
	var errs []error
	sink := &bytes.Buffer{}
	sse := a2a.NewSSEWriter(sink)
	for i, frame := range obs.Frames {
		if err := sse.Write(frame); err != nil {
			errs = append(errs, fmt.Errorf("frame %d (%s) is not admissible on an A2A stream: %w",
				i, describe(frame), err))
			return errs
		}
	}
	if sse.Closed() != v.Expect.StreamClosed {
		if v.Expect.StreamClosed {
			errs = append(errs, fmt.Errorf(
				"the stream is still open; the vector expects the terminal-close rule to have closed it at %s",
				v.Expect.FinalState))
		} else {
			errs = append(errs, fmt.Errorf(
				"the stream closed; the vector expects it to stay open (a non-terminal state must park a stream, not close it)"))
		}
	}
	if sse.Interrupted() != v.Expect.StreamInterrupted {
		errs = append(errs, fmt.Errorf("stream interrupted = %v, want %v",
			sse.Interrupted(), v.Expect.StreamInterrupted))
	}
	return errs
}

// checkOrdering asserts the two ordering deviations this effort made
// deliberately: artifacts precede the terminal status, and status frames may
// follow artifact frames rather than being phase-gated ahead of them.
func checkOrdering(v Vector, obs Observation) []error {
	var errs []error
	lastArtifact, firstStatusAfterArtifact, terminal := -1, -1, -1
	for i, f := range obs.Frames {
		switch f.Kind() {
		case a2a.StreamPayloadArtifactUpdate:
			lastArtifact = i
		case a2a.StreamPayloadStatusUpdate:
			if lastArtifact >= 0 && firstStatusAfterArtifact < 0 {
				firstStatusAfterArtifact = i
			}
			if f.StatusUpdate.Status.State.IsTerminal() {
				terminal = i
			}
		case a2a.StreamPayloadTask:
			if f.Task.Status.State.IsTerminal() {
				terminal = i
			}
		}
	}
	if v.Expect.ArtifactsPrecedeTerminal {
		switch {
		case terminal < 0:
			errs = append(errs, fmt.Errorf(
				"the vector asserts artifacts precede the terminal status, but no terminal frame was produced"))
		case lastArtifact > terminal:
			errs = append(errs, fmt.Errorf(
				"artifact frame %d follows the terminal status at frame %d; a client that stopped at the terminal state would never see it",
				lastArtifact, terminal))
		}
	}
	if v.Expect.StatusFollowsArtifact && firstStatusAfterArtifact < 0 {
		errs = append(errs, fmt.Errorf(
			"no status frame follows an artifact frame; this vector exists to pin the interleaving deviation open"))
	}
	return errs
}

// checkAssertions evaluates the assert_* steps against the frames observed at
// the moment the runner reached them.
func checkAssertions(v Vector, obs Observation) []error {
	var errs []error
	for i, step := range v.Steps {
		if step.Drives() {
			continue
		}
		mark, ok := obs.Marks[i]
		if !ok {
			errs = append(errs, fmt.Errorf("step %d (%s): the runner recorded no frame mark for it", i, step.Kind))
			continue
		}
		if mark > len(obs.Frames) {
			mark = len(obs.Frames)
		}
		prefix := obs.Frames[:mark]
		switch step.Kind {
		case StepAssertActive:
			if idx, state, found := terminalIn(prefix); found {
				errs = append(errs, fmt.Errorf(
					"step %d (assert_active): the task was already %s at frame %d; it must still be live here%s",
					i, state, idx, activeHint(step)))
			}
		case StepAssertParked:
			if idx, state, found := terminalIn(prefix); found {
				errs = append(errs, fmt.Errorf(
					"step %d (assert_parked): the task was already %s at frame %d; an interruption is not a termination",
					i, state, idx))
				continue
			}
			if len(prefix) == 0 {
				errs = append(errs, fmt.Errorf("step %d (assert_parked): no frames had been produced", i))
				continue
			}
			last := prefix[len(prefix)-1]
			state := frameState(last)
			if !state.IsInterrupted() {
				errs = append(errs, fmt.Errorf(
					"step %d (assert_parked): the last frame reports %s, want an interrupted state", i, state))
			}
		}
	}
	return errs
}

// activeHint names the deviation an assert_active failure most often means.
func activeHint(step Step) string {
	if step.Note == "" {
		return ""
	}
	return ": " + step.Note
}

// checkSnapshot compares the mapping's own task snapshot, which is what a
// non-streaming client is answered with. The two views of one turn must agree.
func checkSnapshot(v Vector, obs Observation, ids identity) []error {
	var errs []error
	if obs.Task.ID != "" && obs.Task.ID != ids.taskID {
		errs = append(errs, fmt.Errorf("the task snapshot names task %q, but the stream opened on %q",
			obs.Task.ID, ids.taskID))
	}
	if obs.Task.Status.State != v.Expect.FinalState {
		errs = append(errs, fmt.Errorf("final task state = %s, want %s",
			obs.Task.Status.State, v.Expect.FinalState))
	}
	wantIDs := map[string]bool{}
	for _, f := range v.Expect.Frames {
		if f.Kind == FrameArtifact && f.ArtifactID != "" {
			wantIDs[ids.expand(f.ArtifactID)] = true
		}
	}
	gotIDs := map[string]bool{}
	for _, a := range obs.Task.Artifacts {
		gotIDs[a.ArtifactID] = true
	}
	for id := range wantIDs {
		if !gotIDs[id] {
			errs = append(errs, fmt.Errorf("the task snapshot is missing artifact %q, which the stream delivered", id))
		}
	}
	for id := range gotIDs {
		if !wantIDs[id] {
			errs = append(errs, fmt.Errorf("the task snapshot carries artifact %q, which the stream never delivered", id))
		}
	}
	return errs
}

// ---- small helpers ----

// terminalIn reports the first terminal state in a frame prefix.
func terminalIn(frames []a2a.StreamResponse) (int, a2a.TaskState, bool) {
	for i, f := range frames {
		if state, terminal := f.TerminalState(); terminal {
			return i, state, true
		}
	}
	return 0, a2a.TaskStateUnspecified, false
}

// frameState reports the task state a frame carries, if any.
func frameState(f a2a.StreamResponse) a2a.TaskState {
	switch f.Kind() {
	case a2a.StreamPayloadTask:
		return f.Task.Status.State
	case a2a.StreamPayloadStatusUpdate:
		return f.StatusUpdate.Status.State
	default:
		return a2a.TaskStateUnspecified
	}
}

// messageText concatenates a message's text parts.
func messageText(m a2a.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if text, ok := p.TextValue(); ok {
			b.WriteString(text)
		}
	}
	return b.String()
}

// describe renders a frame for a failure message.
func describe(f a2a.StreamResponse) string {
	switch f.Kind() {
	case a2a.StreamPayloadTask:
		return fmt.Sprintf("task %s", f.Task.Status.State)
	case a2a.StreamPayloadStatusUpdate:
		return fmt.Sprintf("status %s", f.StatusUpdate.Status.State)
	case a2a.StreamPayloadArtifactUpdate:
		return fmt.Sprintf("artifact %s", f.ArtifactUpdate.Artifact.ArtifactID)
	case a2a.StreamPayloadMessage:
		return "message"
	default:
		return "empty frame"
	}
}

// sameValue compares two values as JSON values, so numeric types that differ
// only in Go representation compare equal.
func sameValue(got, want any) bool {
	gotJSON, err := json.Marshal(got)
	if err != nil {
		return false
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return false
	}
	return sameJSON(gotJSON, wantJSON)
}

// sameJSON compares two JSON documents structurally.
func sameJSON(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
