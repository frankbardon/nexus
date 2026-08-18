package a2a

import (
	"encoding/json"
	"fmt"
	"time"
)

// The Nexus A2A extension.
//
// A2A's canonical data model is deliberately narrow: a task with a state, a
// history of messages, and artifacts. Nexus produces a great deal more —
// thinking steps, individual tool calls and their results, subagent progress,
// per-turn token accounting — none of which has a canonical A2A field. The
// specification's sanctioned escape hatch for exactly this is an extension
// declared in the agent card and opted into per request via the A2A-Extensions
// service parameter (section 8.4).
//
// This is the A2A analogue of the AG-UI Custom event that nexus.io.agui uses for
// the same class of data. The difference worth noting is that AG-UI's Custom
// event is untyped by design (a name plus an opaque value), whereas here the
// payload is a closed, typed union: NexusEvent. Callers therefore get compile
// time checking on the producing side and a real decode on the consuming side.
//
// # Where the payload rides
//
// One extension, two carriers, chosen by what the payload accompanies:
//
//   - As a Part inside an artifact or message, built with NexusEventPart. The
//     part is a data part tagged with the extension URI in its metadata, so a
//     client that does not speak the extension sees a structured data part it
//     can ignore rather than prose it would render.
//   - As an entry in a TaskStatusUpdateEvent's or TaskArtifactUpdateEvent's
//     metadata map under the extension URI as key, built with
//     NexusEventMetadata. This is the right carrier for telemetry that
//     annotates a state change rather than constituting output.
//
// A client that did not opt in must still receive a well-formed, canonical
// stream; the extension only ever adds. Nothing in the canonical model is
// withheld from a client that ignores it, which is why NexusExtension declares
// Required as false.

// NexusExtensionURI uniquely identifies the Nexus A2A extension. It is the
// value clients send in the A2A-Extensions service parameter to opt in, and the
// key under which extension payloads travel in metadata maps.
const NexusExtensionURI = "https://github.com/frankbardon/nexus/a2a/extensions/agent-events/v1"

// NexusExtensionVersion is the extension's own version, independent of the A2A
// protocol version.
//
// It is NOT published as a separate card field: AgentExtension has no version
// key, because specification section 4.6.3 requires the version to live inside
// the URI and requires a NEW URI for any breaking change. The "/v1" suffix on
// NexusExtensionURI is the wire-visible version; this constant exists so the
// two cannot drift silently and so the value is available to code that reports
// it (logs, the params block below).
const NexusExtensionVersion = "1.0"

// NexusExtensionDescription is the human-readable summary published in the
// agent card.
const NexusExtensionDescription = "Nexus agent telemetry: thinking steps, tool calls and results, " +
	"subagent progress, and token usage that have no canonical A2A representation."

// NexusEventKind names the kind of telemetry a NexusEvent carries, and selects
// which payload arm is set.
type NexusEventKind string

// Nexus extension event kinds.
const (
	NexusEventKindUnset      NexusEventKind = ""
	NexusEventKindThinking   NexusEventKind = "thinking"
	NexusEventKindToolCall   NexusEventKind = "tool_call"
	NexusEventKindToolResult NexusEventKind = "tool_result"
	NexusEventKindSubagent   NexusEventKind = "subagent"
	NexusEventKindUsage      NexusEventKind = "usage"
)

// nexusEventKinds is every addressable event kind, in declaration order.
var nexusEventKinds = []NexusEventKind{
	NexusEventKindThinking,
	NexusEventKindToolCall,
	NexusEventKindToolResult,
	NexusEventKindSubagent,
	NexusEventKindUsage,
}

// NexusEventKinds returns the event kinds the extension defines. The returned
// slice is a fresh copy.
func NexusEventKinds() []NexusEventKind {
	out := make([]NexusEventKind, len(nexusEventKinds))
	copy(out, nexusEventKinds)
	return out
}

// Valid reports whether the kind is one the extension defines.
func (k NexusEventKind) Valid() bool {
	for _, known := range nexusEventKinds {
		if k == known {
			return true
		}
	}
	return false
}

// NexusEvent is the typed payload of the Nexus A2A extension. Kind selects
// exactly one payload arm; the remaining envelope fields apply to every kind.
type NexusEvent struct {
	// Kind selects the payload arm. Required.
	Kind NexusEventKind `json:"kind"`
	// TaskID is the A2A task the telemetry belongs to.
	TaskID string `json:"taskId,omitempty"`
	// ContextID is the A2A context the telemetry belongs to.
	ContextID string `json:"contextId,omitempty"`
	// Sequence orders events within a task. It is monotonic per task and lets a
	// client reassemble ordering across the two carriers, since a part and a
	// metadata entry do not share a delivery channel.
	Sequence int `json:"sequence,omitempty"`
	// Timestamp records when the event was observed.
	Timestamp *Timestamp `json:"timestamp,omitempty"`
	// Source names the originating Nexus bus event type, e.g. "tool.result".
	// It is informational: clients must key behavior off Kind, not Source.
	Source string `json:"source,omitempty"`

	// Thinking is set when Kind is NexusEventKindThinking.
	Thinking *NexusThinking `json:"thinking,omitempty"`
	// ToolCall is set when Kind is NexusEventKindToolCall.
	ToolCall *NexusToolCall `json:"toolCall,omitempty"`
	// ToolResult is set when Kind is NexusEventKindToolResult.
	ToolResult *NexusToolResult `json:"toolResult,omitempty"`
	// Subagent is set when Kind is NexusEventKindSubagent.
	Subagent *NexusSubagentProgress `json:"subagent,omitempty"`
	// Usage is set when Kind is NexusEventKindUsage.
	Usage *NexusTokenUsage `json:"usage,omitempty"`
}

// NexusThinking is one reasoning step emitted by the agent loop.
type NexusThinking struct {
	// Step is the 1-based index of the step within the turn.
	Step int `json:"step,omitempty"`
	// Content is the reasoning text. Empty when Redacted is true.
	Content string `json:"content,omitempty"`
	// Redacted reports that the provider withheld the reasoning text, which
	// several providers do for safety-filtered thinking blocks.
	Redacted bool `json:"redacted,omitempty"`
	// Signature is the provider's opaque integrity signature over the step,
	// when one was supplied.
	Signature string `json:"signature,omitempty"`
}

// NexusToolCall is a tool invocation the agent decided to make.
type NexusToolCall struct {
	// CallID correlates the call with its NexusToolResult. Required.
	CallID string `json:"callId"`
	// Name is the tool name. Required.
	Name string `json:"name"`
	// Arguments is the tool's JSON argument object.
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Risk is the Nexus risk classification that drove any approval prompt.
	Risk string `json:"risk,omitempty"`
	// Approved reports the outcome of a human approval gate: nil when no gate
	// applied, otherwise the decision.
	Approved *bool `json:"approved,omitempty"`
}

// NexusToolResult is the outcome of a tool invocation.
type NexusToolResult struct {
	// CallID correlates the result with its NexusToolCall. Required.
	CallID string `json:"callId"`
	// Name is the tool name. Required.
	Name string `json:"name"`
	// Output is the tool's textual result.
	Output string `json:"output,omitempty"`
	// Error describes the failure when the tool failed. Empty on success.
	Error string `json:"error,omitempty"`
	// DurationMS is the wall-clock execution time in milliseconds.
	DurationMS int64 `json:"durationMs,omitempty"`
}

// NexusSubagentPhase names a point in a subagent's lifecycle.
type NexusSubagentPhase string

// Subagent lifecycle phases.
const (
	NexusSubagentStarted   NexusSubagentPhase = "started"
	NexusSubagentIteration NexusSubagentPhase = "iteration"
	NexusSubagentComplete  NexusSubagentPhase = "complete"
	NexusSubagentFailed    NexusSubagentPhase = "failed"
)

// Valid reports whether the phase is one the extension defines.
func (p NexusSubagentPhase) Valid() bool {
	switch p {
	case NexusSubagentStarted, NexusSubagentIteration, NexusSubagentComplete, NexusSubagentFailed:
		return true
	default:
		return false
	}
}

// NexusSubagentProgress reports the progress of delegated work: a delegate call,
// an orchestrator worker, or a remote agent invoked as a tool.
type NexusSubagentProgress struct {
	// SubagentID uniquely identifies the delegated run. Required.
	SubagentID string `json:"subagentId"`
	// Name is the human-readable subagent or posture name.
	Name string `json:"name,omitempty"`
	// Phase is the lifecycle point being reported. Required.
	Phase NexusSubagentPhase `json:"phase"`
	// Iteration is the subagent's current loop iteration, 1-based.
	Iteration int `json:"iteration,omitempty"`
	// MaxIterations is the subagent's iteration budget, when one applies.
	MaxIterations int `json:"maxIterations,omitempty"`
	// Depth is the delegation depth, 1 for a direct child of the root agent.
	Depth int `json:"depth,omitempty"`
	// Detail is a human-readable progress note.
	Detail string `json:"detail,omitempty"`
	// Error describes the failure when Phase is NexusSubagentFailed.
	Error string `json:"error,omitempty"`
}

// NexusTokenUsage reports token accounting for a turn or for the session so far.
type NexusTokenUsage struct {
	// Model names the model the counts apply to.
	Model string `json:"model,omitempty"`
	// InputTokens is the prompt token count.
	InputTokens int `json:"inputTokens,omitempty"`
	// OutputTokens is the completion token count.
	OutputTokens int `json:"outputTokens,omitempty"`
	// CachedInputTokens is the portion of InputTokens served from a provider
	// prompt cache.
	CachedInputTokens int `json:"cachedInputTokens,omitempty"`
	// ReasoningTokens is the portion of OutputTokens spent on reasoning, for
	// providers that report it separately.
	ReasoningTokens int `json:"reasoningTokens,omitempty"`
	// TotalTokens is the sum for this accounting unit.
	TotalTokens int `json:"totalTokens,omitempty"`
	// Cumulative reports that the counts are session totals rather than the
	// cost of a single turn.
	Cumulative bool `json:"cumulative,omitempty"`
}

// ---- Declaration ----

// NexusExtension returns the agent card declaration for the Nexus extension.
// It is never Required: everything it carries is supplementary, so a client
// that ignores it still receives a complete canonical stream.
//
// The declaration's params publish the event kinds and the two carriers, so
// a client can tell from the card alone what to expect and where to look for it
// without fetching an out-of-band schema.
func NexusExtension() AgentExtension {
	kinds := make([]any, 0, len(nexusEventKinds))
	for _, k := range nexusEventKinds {
		kinds = append(kinds, string(k))
	}
	return AgentExtension{
		URI:         NexusExtensionURI,
		Description: NexusExtensionDescription,
		Required:    false,
		// params is the proto's google.protobuf.Struct for extension-defined
		// configuration. The version is republished here as data rather than as
		// a card field, since AgentExtension has none (section 4.6.3).
		Params: map[string]any{
			"version":     NexusExtensionVersion,
			"eventKinds":  kinds,
			"carriers":    []any{"part", "metadata"},
			"metadataKey": NexusExtensionURI,
		},
	}
}

// ---- Constructors ----

// NewNexusEvent builds an event of the given kind stamped with the current
// time. Callers set the matching payload arm on the returned value.
func NewNexusEvent(kind NexusEventKind, taskID, contextID string) NexusEvent {
	return NexusEvent{
		Kind:      kind,
		TaskID:    taskID,
		ContextID: contextID,
		Timestamp: NewTimestamp(time.Now()),
	}
}

// ThinkingEvent builds a thinking-step event.
func ThinkingEvent(taskID, contextID string, step NexusThinking) NexusEvent {
	e := NewNexusEvent(NexusEventKindThinking, taskID, contextID)
	e.Thinking = &step
	return e
}

// ToolCallEvent builds a tool-invocation event.
func ToolCallEvent(taskID, contextID string, call NexusToolCall) NexusEvent {
	e := NewNexusEvent(NexusEventKindToolCall, taskID, contextID)
	e.ToolCall = &call
	return e
}

// ToolResultEvent builds a tool-result event.
func ToolResultEvent(taskID, contextID string, result NexusToolResult) NexusEvent {
	e := NewNexusEvent(NexusEventKindToolResult, taskID, contextID)
	e.ToolResult = &result
	return e
}

// SubagentEvent builds a subagent-progress event.
func SubagentEvent(taskID, contextID string, progress NexusSubagentProgress) NexusEvent {
	e := NewNexusEvent(NexusEventKindSubagent, taskID, contextID)
	e.Subagent = &progress
	return e
}

// UsageEvent builds a token-usage event.
func UsageEvent(taskID, contextID string, usage NexusTokenUsage) NexusEvent {
	e := NewNexusEvent(NexusEventKindUsage, taskID, contextID)
	e.Usage = &usage
	return e
}

// At stamps a specific observation time onto the event and returns it, for
// deterministic tests and replayed histories.
func (e NexusEvent) At(t time.Time) NexusEvent {
	e.Timestamp = NewTimestamp(t)
	return e
}

// From stamps the originating Nexus bus event type onto the event and returns
// it, for chaining.
func (e NexusEvent) From(busEventType string) NexusEvent {
	e.Source = busEventType
	return e
}

// Seq stamps a sequence number onto the event and returns it, for chaining.
func (e NexusEvent) Seq(n int) NexusEvent {
	e.Sequence = n
	return e
}

// ---- Validation ----

// Validate checks that the event's kind is known and that exactly the matching
// payload arm is set.
func (e NexusEvent) Validate() *Error {
	if !e.Kind.Valid() {
		return ErrInvalidParams(FieldViolation{
			Field:       "nexusEvent.kind",
			Description: fmt.Sprintf("unknown nexus event kind %q", string(e.Kind)),
		})
	}

	arms := map[NexusEventKind]bool{
		NexusEventKindThinking:   e.Thinking != nil,
		NexusEventKindToolCall:   e.ToolCall != nil,
		NexusEventKindToolResult: e.ToolResult != nil,
		NexusEventKindSubagent:   e.Subagent != nil,
		NexusEventKindUsage:      e.Usage != nil,
	}
	for kind, present := range arms {
		if present != (kind == e.Kind) {
			return ErrInvalidParams(FieldViolation{
				Field:       "nexusEvent",
				Description: fmt.Sprintf("kind %q requires exactly the %q payload to be set", string(e.Kind), string(e.Kind)),
			})
		}
	}

	switch e.Kind {
	case NexusEventKindToolCall:
		if e.ToolCall.CallID == "" || e.ToolCall.Name == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       "nexusEvent.toolCall",
				Description: "call id and tool name are required",
			})
		}
	case NexusEventKindToolResult:
		if e.ToolResult.CallID == "" || e.ToolResult.Name == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       "nexusEvent.toolResult",
				Description: "call id and tool name are required",
			})
		}
	case NexusEventKindSubagent:
		if e.Subagent.SubagentID == "" {
			return ErrInvalidParams(FieldViolation{
				Field:       "nexusEvent.subagent.subagentId",
				Description: "subagent id is required",
			})
		}
		if !e.Subagent.Phase.Valid() {
			return ErrInvalidParams(FieldViolation{
				Field:       "nexusEvent.subagent.phase",
				Description: fmt.Sprintf("unknown subagent phase %q", string(e.Subagent.Phase)),
			})
		}
	}
	return nil
}

// ---- Carriers ----

// NexusEventPart builds a data Part carrying the event, tagged with the
// extension URI in its metadata so a client can recognize and skip it without
// decoding.
func NexusEventPart(e NexusEvent) (Part, error) {
	if err := e.Validate(); err != nil {
		return Part{}, err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return Part{}, fmt.Errorf("a2a: encode nexus event: %w", err)
	}
	return Part{
		Data:      json.RawMessage(raw),
		MediaType: ContentTypeJSON,
		Metadata:  map[string]any{extensionMetadataKey: NexusExtensionURI},
	}, nil
}

// extensionMetadataKey is the metadata key naming the extension a part belongs
// to. It mirrors the Extensions field A2A puts on messages and artifacts, which
// Part itself lacks.
const extensionMetadataKey = "a2aExtension"

// NexusEventFromPart decodes a Part carrying a Nexus extension event. The
// second return value reports whether the part is tagged with this extension at
// all; a part belonging to another extension yields (nil, false, nil).
func NexusEventFromPart(p Part) (*NexusEvent, bool, error) {
	uri, _ := p.Metadata[extensionMetadataKey].(string)
	if uri != NexusExtensionURI {
		return nil, false, nil
	}
	if p.Data == nil {
		return nil, true, fmt.Errorf("a2a: nexus extension part carries no data")
	}
	e, err := DecodeNexusEvent(p.Data)
	if err != nil {
		return nil, true, err
	}
	return e, true, nil
}

// NexusEventMetadata builds the metadata map entry carrying the event, keyed by
// the extension URI. Merge it into a status or artifact update's metadata.
func NexusEventMetadata(e NexusEvent) (map[string]any, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("a2a: encode nexus event: %w", err)
	}
	return map[string]any{NexusExtensionURI: json.RawMessage(raw)}, nil
}

// NexusEventFromMetadata decodes a Nexus extension event out of a metadata map.
// The second return value reports whether the map carried one.
func NexusEventFromMetadata(md map[string]any) (*NexusEvent, bool, error) {
	value, ok := md[NexusExtensionURI]
	if !ok {
		return nil, false, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, true, fmt.Errorf("a2a: re-encode nexus extension metadata: %w", err)
	}
	e, err := DecodeNexusEvent(raw)
	if err != nil {
		return nil, true, err
	}
	return e, true, nil
}

// DecodeNexusEvent parses and validates a Nexus extension event payload.
func DecodeNexusEvent(data []byte) (*NexusEvent, error) {
	e, err := decode[NexusEvent](data, "nexus event")
	if err != nil {
		return nil, err
	}
	if verr := e.Validate(); verr != nil {
		return nil, verr
	}
	return e, nil
}
