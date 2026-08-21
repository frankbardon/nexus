package a2aconform

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"sync"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// corpus is the embedded vector set. Adding a file under vectors/ adds a vector;
// nothing else has to be edited, which is what keeps "add a vector first" a
// data-only change.
//
//go:embed vectors/*.json
var corpus embed.FS

// Vector is one conformance case: an abstract scenario plus the exact A2A output
// any mapping replaying that scenario must produce.
type Vector struct {
	// ID names the vector and is the subtest name. Unique across the corpus.
	ID string `json:"id"`
	// Title is a one-line human summary.
	Title string `json:"title"`
	// Rationale says why the expectation is what it is. It is the field a future
	// implementer reads before deciding a vector is "wrong", so it is required.
	Rationale string `json:"rationale"`
	// Features are the capabilities a mapping must have to replay this vector. A
	// driver declaring fewer skips it, visibly.
	Features []Feature `json:"features"`
	// Policy is the artifact policy the mapping must apply for this vector. It
	// is transport-neutral: it describes the A2A artifact bound, not how any
	// mapping is configured. Nil means the mapping's defaults.
	Policy *Policy `json:"policy,omitempty"`
	// Steps are the abstract scenario, applied in order.
	Steps []Step `json:"steps"`
	// Expect is the A2A output the scenario must produce.
	Expect Expect `json:"expect"`
}

// Policy is the artifact bound a vector runs under. Nil fields keep the
// mapping's own default, so a vector states only the knob it is about.
type Policy struct {
	// MaxFileBytes caps the inline content of one file artifact. A file over it
	// must degrade to a metadata note rather than being dropped or inlined.
	MaxFileBytes *int `json:"maxFileBytes,omitempty"`
	// MaxToolOutputBytes caps one tool-result artifact's text. Zero disables it.
	MaxToolOutputBytes *int `json:"maxToolOutputBytes,omitempty"`
	// MaxTaskBytes is one task's total artifact budget, excluding its final
	// response artifact. Zero disables it.
	MaxTaskBytes *int `json:"maxTaskBytes,omitempty"`
}

// StepKind names one abstract thing that happens during a turn.
//
// The vocabulary is deliberately at the level of what the AGENT DID, not at the
// level of any transport's payloads: every kind below has an obvious realization
// both from engine bus events and from the broker's flat IO envelope.
type StepKind string

// The step vocabulary.
const (
	// StepMessage is the client sending the message that creates the task.
	StepMessage StepKind = "message"
	// StepTurnStart is the agent turn beginning.
	StepTurnStart StepKind = "turn_start"
	// StepThinking is one reasoning step.
	StepThinking StepKind = "thinking"
	// StepToolCall is the agent deciding to invoke a tool.
	StepToolCall StepKind = "tool_call"
	// StepToolResult is a tool reporting its outcome, optionally naming a file
	// it wrote.
	StepToolResult StepKind = "tool_result"
	// StepAgentText is the model producing the turn's final text. It is NOT the
	// end of the task: see StepOutput and StepTurnEnd.
	StepAgentText StepKind = "agent_text"
	// StepOutput is the text the agent actually published, after whatever
	// output filtering the host applies.
	StepOutput StepKind = "output"
	// StepAskUser is the agent asking the human a question.
	StepAskUser StepKind = "ask_user"
	// StepAnswer is the client answering that question, resuming the task.
	StepAnswer StepKind = "answer"
	// StepTurnEnd is the agent turn ending, which is what completes the task.
	StepTurnEnd StepKind = "turn_end"
	// StepFailure is an unrecoverable error ending the turn.
	StepFailure StepKind = "failure"
	// StepCancel is the client canceling the task.
	StepCancel StepKind = "cancel"

	// StepAssertActive asserts that no terminal frame has been produced yet. It
	// is evaluated by the runner and never reaches a driver.
	StepAssertActive StepKind = "assert_active"
	// StepAssertParked asserts the task is currently interrupted at
	// INPUT_REQUIRED and the stream is still open.
	StepAssertParked StepKind = "assert_parked"
)

// Step is one entry in a vector's scenario. Only the fields relevant to Kind are
// populated; it is a flat union for the same reason the broker's IO envelope is.
type Step struct {
	Kind StepKind `json:"kind"`
	// Note is a human comment. The runner ignores it.
	Note string `json:"note,omitempty"`

	// Text carries the message text (message, answer), the assistant text
	// (agent_text, output) or the reasoning text (thinking).
	Text string `json:"text,omitempty"`
	// ContextID is the A2A context the creating message names.
	ContextID string `json:"contextId,omitempty"`
	// MessageID is the client's message id.
	MessageID string `json:"messageId,omitempty"`
	// TurnID correlates turn_start with turn_end.
	TurnID string `json:"turnId,omitempty"`

	// CallID and Tool identify a tool call and its result.
	CallID string `json:"callId,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// Arguments are the tool call's arguments.
	Arguments map[string]any `json:"arguments,omitempty"`
	// Output is the tool's text output.
	Output string `json:"output,omitempty"`
	// OutputRepeat renders Output repeated n times, so a vector can express a
	// large payload without embedding one.
	OutputRepeat int `json:"outputRepeat,omitempty"`
	// Structured is the tool's typed result.
	Structured map[string]any `json:"structured,omitempty"`
	// Error is the tool's failure text.
	Error string `json:"error,omitempty"`
	// File is a file the tool wrote. The runner materializes it under
	// Env.FileDir before the step is applied, so a driver only has to report it.
	File *StepFile `json:"file,omitempty"`

	// RequestID correlates ask_user with the answer that resolves it.
	RequestID string `json:"requestId,omitempty"`
	// Prompt is the question text.
	Prompt string `json:"prompt,omitempty"`
	// Choices are the options a multiple-choice question offers.
	Choices []StepChoice `json:"choices,omitempty"`

	// Reason is the failure or cancellation reason.
	Reason string `json:"reason,omitempty"`
}

// StepFile is a file the scenario says a tool wrote.
type StepFile struct {
	// Path is relative to Env.FileDir.
	Path string `json:"path"`
	// Bytes is the file's size. The content is filler; no vector asserts it.
	Bytes int `json:"bytes"`
}

// StepChoice is one option of a multiple-choice question.
type StepChoice struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// Drives reports whether this step is realized by a driver. Assertion steps are
// evaluated by the runner alone.
func (s Step) Drives() bool {
	switch s.Kind {
	case StepAssertActive, StepAssertParked:
		return false
	default:
		return true
	}
}

// Expect is the A2A output a vector's scenario must produce.
type Expect struct {
	// Frames is the exact frame sequence, compared position by position. Exact
	// comparison is the point: two mappings replaying one scenario must produce
	// the SAME stream, not merely compatible ones.
	Frames []FrameExpect `json:"frames"`
	// FinalState is the task's state once the scenario has been replayed, as the
	// mapping's own task snapshot reports it.
	FinalState a2a.TaskState `json:"finalState"`
	// StreamClosed is whether the frame sequence closes the stream under
	// specification section 11.7's terminal-close rule. It is always asserted:
	// false is the meaningful expectation for a parked task.
	StreamClosed bool `json:"streamClosed"`
	// StreamInterrupted is whether the stream ends parked at an interrupted
	// state. Always asserted, for the same reason.
	StreamInterrupted bool `json:"streamInterrupted"`
	// ArtifactsPrecedeTerminal requires every artifact frame to arrive before
	// the terminal status frame. False means "not asserted by this vector".
	ArtifactsPrecedeTerminal bool `json:"artifactsPrecedeTerminal,omitempty"`
	// StatusFollowsArtifact requires at least one status frame AFTER an artifact
	// frame, which is what pins the interleaving deviation open. False means
	// "not asserted by this vector".
	StatusFollowsArtifact bool `json:"statusFollowsArtifact,omitempty"`
}

// Frame kinds a FrameExpect may name.
const (
	FrameTask     = "task"
	FrameStatus   = "status"
	FrameArtifact = "artifact"
	FrameMessage  = "message"
)

// FrameExpect is one expected A2A stream frame.
//
// String fields and Contains entries may use the placeholders {taskId} and
// {contextId}, since both are minted by the mapping at runtime and cannot be
// written into a static corpus.
type FrameExpect struct {
	// Kind is one of task, status, artifact, message.
	Kind string `json:"kind"`
	// Note is a human comment. The runner ignores it.
	Note string `json:"note,omitempty"`

	// State is the task state a task or status frame reports.
	State a2a.TaskState `json:"state,omitempty"`
	// MessageRole, when set, requires a status message with that role.
	MessageRole a2a.Role `json:"messageRole,omitempty"`
	// MessageContains are substrings the status message's text must contain.
	MessageContains []string `json:"messageContains,omitempty"`
	// MessageMetadata are key/value pairs the status message's metadata must
	// carry.
	MessageMetadata map[string]any `json:"messageMetadata,omitempty"`
	// NoMessage requires the status to carry no message at all.
	NoMessage bool `json:"noMessage,omitempty"`

	// ArtifactID is the artifact's exact id.
	ArtifactID string `json:"artifactId,omitempty"`
	// ArtifactName is the artifact's human-readable label.
	ArtifactName string `json:"artifactName,omitempty"`
	// LastChunk and Append pin the chunking flags when a vector cares.
	LastChunk *bool `json:"lastChunk,omitempty"`
	Append    *bool `json:"append,omitempty"`
	// Parts is the artifact's part list, compared position by position.
	Parts []PartExpect `json:"parts,omitempty"`
	// Metadata are key/value pairs the artifact metadata must carry. Comparison
	// is by JSON value, so 4096 matches whether the mapping produced an int, an
	// int64 or a float64.
	Metadata map[string]any `json:"metadata,omitempty"`
	// MetadataAbsent are keys the artifact metadata must NOT carry.
	MetadataAbsent []string `json:"metadataAbsent,omitempty"`
}

// PartExpect is one expected Part of an artifact.
type PartExpect struct {
	// Kind is the part's content arm: text, raw, url or data.
	Kind a2a.PartKind `json:"kind"`
	// MediaType is the part's declared media type.
	MediaType string `json:"mediaType,omitempty"`
	// Filename is the part's declared filename.
	Filename string `json:"filename,omitempty"`
	// Contains are substrings the text content must contain.
	Contains []string `json:"contains,omitempty"`
	// Equals is the exact text content.
	Equals *string `json:"equals,omitempty"`
	// JSONEquals is the exact structured content, compared as JSON values.
	JSONEquals json.RawMessage `json:"jsonEquals,omitempty"`
	// Bytes is the exact length of a raw part's content.
	Bytes *int `json:"bytes,omitempty"`
}

var (
	loadOnce sync.Once
	loaded   []Vector
	loadErr  error
)

// Vectors returns the corpus in a stable order, panicking if it does not decode.
// A malformed corpus is a build-level defect: every caller is a test, and a test
// that silently ran zero vectors is worse than one that failed to start.
func Vectors() []Vector {
	loadOnce.Do(func() { loaded, loadErr = load() })
	if loadErr != nil {
		panic("a2aconform: " + loadErr.Error())
	}
	out := make([]Vector, len(loaded))
	copy(out, loaded)
	return out
}

// Vector returns the named vector.
func VectorByID(id string) (Vector, bool) {
	for _, v := range Vectors() {
		if v.ID == id {
			return v, true
		}
	}
	return Vector{}, false
}

// load decodes and validates every embedded vector document.
func load() ([]Vector, error) {
	entries, err := fs.ReadDir(corpus, "vectors")
	if err != nil {
		return nil, fmt.Errorf("reading the vector corpus: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	seen := make(map[string]string, len(names))
	out := make([]Vector, 0, len(names))
	for _, name := range names {
		raw, err := corpus.ReadFile(path.Join("vectors", name))
		if err != nil {
			return nil, fmt.Errorf("reading vector %s: %w", name, err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		// Strict: a mistyped key in a vector must fail the build rather than
		// leaving an expectation that quietly never runs.
		dec.DisallowUnknownFields()
		var v Vector
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("decoding vector %s: %w", name, err)
		}
		if err := validate(v, name); err != nil {
			return nil, err
		}
		if prior, dup := seen[v.ID]; dup {
			return nil, fmt.Errorf("vector id %q is declared by both %s and %s", v.ID, prior, name)
		}
		seen[v.ID] = name
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the vector corpus is empty")
	}
	return out, nil
}

// validate rejects a structurally impossible vector at load time.
func validate(v Vector, file string) error {
	switch {
	case v.ID == "":
		return fmt.Errorf("vector %s: id is required", file)
	case v.Title == "":
		return fmt.Errorf("vector %s: title is required", file)
	case v.Rationale == "":
		return fmt.Errorf("vector %s: rationale is required; a vector nobody can justify is a vector somebody will delete", file)
	case len(v.Features) == 0:
		return fmt.Errorf("vector %s: at least one feature is required", file)
	case len(v.Steps) == 0:
		return fmt.Errorf("vector %s: at least one step is required", file)
	case len(v.Expect.Frames) == 0:
		return fmt.Errorf("vector %s: at least one expected frame is required", file)
	}
	for _, f := range v.Features {
		if !f.Known() {
			return fmt.Errorf("vector %s: unknown feature %q", file, f)
		}
	}
	for i, s := range v.Steps {
		if !knownStepKinds[s.Kind] {
			return fmt.Errorf("vector %s: step %d: unknown kind %q", file, i, s.Kind)
		}
	}
	if v.Steps[0].Kind != StepMessage {
		return fmt.Errorf("vector %s: the first step must be %q: a task begins with the client's message", file, StepMessage)
	}
	for i, f := range v.Expect.Frames {
		switch f.Kind {
		case FrameTask, FrameStatus, FrameArtifact, FrameMessage:
		default:
			return fmt.Errorf("vector %s: frame %d: unknown kind %q", file, i, f.Kind)
		}
	}
	if v.Expect.Frames[0].Kind != FrameTask {
		return fmt.Errorf("vector %s: the first frame must be a task snapshot (specification section 11.7)", file)
	}
	if !v.Expect.FinalState.Valid() {
		return fmt.Errorf("vector %s: finalState %q is not a task state", file, v.Expect.FinalState)
	}
	return nil
}

var knownStepKinds = map[StepKind]bool{
	StepMessage: true, StepTurnStart: true, StepThinking: true,
	StepToolCall: true, StepToolResult: true, StepAgentText: true,
	StepOutput: true, StepAskUser: true, StepAnswer: true,
	StepTurnEnd: true, StepFailure: true, StepCancel: true,
	StepAssertActive: true, StepAssertParked: true,
}
