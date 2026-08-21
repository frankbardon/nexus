package a2a

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNexusExtensionDeclaration(t *testing.T) {
	ext := NexusExtension()
	if ext.URI != NexusExtensionURI {
		t.Errorf("uri = %q, want %q", ext.URI, NexusExtensionURI)
	}
	// The version rides the URI (section 4.6.3); AgentExtension has no version
	// field, so the declaration republishes it as params data instead.
	if !strings.HasSuffix(ext.URI, "/v"+strings.SplitN(NexusExtensionVersion, ".", 2)[0]) {
		t.Errorf("uri %q does not carry the extension major version %q", ext.URI, NexusExtensionVersion)
	}
	if ext.Params["version"] != NexusExtensionVersion {
		t.Errorf("params.version = %v, want %q", ext.Params["version"], NexusExtensionVersion)
	}
	if ext.Required {
		t.Error("the nexus extension must be optional; everything it carries is supplementary")
	}
	if ext.Description == "" {
		t.Error("declaration carries no description")
	}

	kinds, ok := ext.Params["eventKinds"].([]any)
	if !ok {
		t.Fatalf("params.eventKinds = %v", ext.Params["eventKinds"])
	}
	got := map[string]bool{}
	for _, k := range kinds {
		got[k.(string)] = true
	}
	for _, want := range []NexusEventKind{
		NexusEventKindThinking,
		NexusEventKindToolCall,
		NexusEventKindToolResult,
		NexusEventKindSubagent,
		NexusEventKindUsage,
	} {
		if !got[string(want)] {
			t.Errorf("declaration does not advertise event kind %q", want)
		}
	}

	// The declaration must survive a card round trip untouched.
	card := NewAgentCard("n", "d", "1").
		WithInterface(BindingJSONRPC, "https://a.test").
		WithSkill(AgentSkill{ID: "s", Name: "S", Description: "d"}).
		WithExtension(ext)
	data, err := EncodeAgentCard(&card)
	if err != nil {
		t.Fatalf("encode card: %v", err)
	}
	back, err := DecodeAgentCard(data)
	if err != nil {
		t.Fatalf("decode card: %v", err)
	}
	decoded, ok := back.Extension(NexusExtensionURI)
	if !ok {
		t.Fatal("extension lost in the card round trip")
	}
	if decoded.URI != ext.URI || decoded.Required != ext.Required {
		t.Errorf("declaration changed across the round trip: %+v", decoded)
	}
	if decoded.Params["metadataKey"] != NexusExtensionURI {
		t.Errorf("params.metadataKey = %v", decoded.Params["metadataKey"])
	}
}

func TestNexusEventKinds(t *testing.T) {
	kinds := NexusEventKinds()
	if len(kinds) != 5 {
		t.Fatalf("got %d kinds, want 5", len(kinds))
	}
	kinds[0] = "mutated"
	if NexusEventKinds()[0] != NexusEventKindThinking {
		t.Error("NexusEventKinds() shares its backing array")
	}
	if NexusEventKindUnset.Valid() {
		t.Error("the unset kind reported valid")
	}
	if NexusEventKind("nope").Valid() {
		t.Error("an unknown kind reported valid")
	}
}

func TestNexusEventRoundTrip(t *testing.T) {
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	approved := true

	events := []NexusEvent{
		ThinkingEvent(testTaskID, testCtxID, NexusThinking{
			Step: 1, Content: "Consider the README first.", Signature: "sig",
		}).At(at).From("thinking.step").Seq(1),
		ToolCallEvent(testTaskID, testCtxID, NexusToolCall{
			CallID:    "c1",
			Name:      "shell",
			Arguments: json.RawMessage(`{"command":"ls"}`),
			Risk:      "medium",
			Approved:  &approved,
		}).At(at).From("tool.invoke").Seq(2),
		ToolResultEvent(testTaskID, testCtxID, NexusToolResult{
			CallID: "c1", Name: "shell", Output: "README.md", DurationMS: 12,
		}).At(at).From("tool.result").Seq(3),
		SubagentEvent(testTaskID, testCtxID, NexusSubagentProgress{
			SubagentID: "s1", Name: "researcher", Phase: NexusSubagentIteration,
			Iteration: 2, MaxIterations: 5, Depth: 1, Detail: "searching",
		}).At(at).From("subagent.iteration").Seq(4),
		UsageEvent(testTaskID, testCtxID, NexusTokenUsage{
			Model: "claude", InputTokens: 100, OutputTokens: 40,
			CachedInputTokens: 60, ReasoningTokens: 10, TotalTokens: 140,
		}).At(at).From("llm.response").Seq(5),
	}

	for _, e := range events {
		t.Run(string(e.Kind), func(t *testing.T) {
			if err := e.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			data, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			back, err := DecodeNexusEvent(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if back.Kind != e.Kind || back.Sequence != e.Sequence || back.Source != e.Source {
				t.Errorf("envelope changed: %+v", back)
			}
			if !back.Timestamp.Time.Equal(at) {
				t.Errorf("timestamp = %v, want %v", back.Timestamp.Time, at)
			}
			if back.TaskID != testTaskID || back.ContextID != testCtxID {
				t.Errorf("identity changed: %q/%q", back.TaskID, back.ContextID)
			}
		})
	}

	// Spot-check that each payload arm actually survives.
	data, err := json.Marshal(events[1])
	if err != nil {
		t.Fatalf("marshal tool call: %v", err)
	}
	back, err := DecodeNexusEvent(data)
	if err != nil {
		t.Fatalf("decode tool call: %v", err)
	}
	if back.ToolCall.Risk != "medium" || back.ToolCall.Approved == nil || !*back.ToolCall.Approved {
		t.Errorf("tool call payload changed: %+v", back.ToolCall)
	}
	if string(back.ToolCall.Arguments) != `{"command":"ls"}` {
		t.Errorf("arguments = %s", back.ToolCall.Arguments)
	}
}

func TestNexusEventValidateRejectsMismatchedArms(t *testing.T) {
	cases := map[string]NexusEvent{
		"unknown kind": {Kind: "nope"},
		"unset kind":   {},
		"missing arm":  {Kind: NexusEventKindThinking},
		"wrong arm":    {Kind: NexusEventKindThinking, Usage: &NexusTokenUsage{}},
		"extra arm":    {Kind: NexusEventKindUsage, Usage: &NexusTokenUsage{}, Thinking: &NexusThinking{}},
		"tool no id":   {Kind: NexusEventKindToolCall, ToolCall: &NexusToolCall{Name: "shell"}},
		"tool no name": {Kind: NexusEventKindToolCall, ToolCall: &NexusToolCall{CallID: "c"}},
		"result no id": {Kind: NexusEventKindToolResult, ToolResult: &NexusToolResult{Name: "shell"}},
		"subagent no id": {
			Kind:     NexusEventKindSubagent,
			Subagent: &NexusSubagentProgress{Phase: NexusSubagentStarted},
		},
		"subagent bad phase": {
			Kind:     NexusEventKindSubagent,
			Subagent: &NexusSubagentProgress{SubagentID: "s", Phase: "spinning"},
		},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if err := e.Validate(); err == nil {
				t.Fatal("invalid event accepted")
			}
		})
	}
}

func TestNexusSubagentPhaseValid(t *testing.T) {
	for _, p := range []NexusSubagentPhase{
		NexusSubagentStarted, NexusSubagentIteration, NexusSubagentComplete, NexusSubagentFailed,
	} {
		if !p.Valid() {
			t.Errorf("%q reported invalid", p)
		}
	}
	if NexusSubagentPhase("").Valid() {
		t.Error("empty phase reported valid")
	}
}

func TestNexusEventPartCarrier(t *testing.T) {
	e := ThinkingEvent(testTaskID, testCtxID, NexusThinking{Step: 1, Content: "hmm"})
	part, err := NexusEventPart(e)
	if err != nil {
		t.Fatalf("build part: %v", err)
	}
	if err := ValidatePart(part, "part"); err != nil {
		t.Fatalf("part is not a valid A2A part: %v", err)
	}
	if part.Kind() != PartKindData {
		t.Errorf("part kind = %q, want data", part.Kind())
	}

	// The part rides inside a normal artifact, which must stay valid.
	artifact := NewArtifact("a1", TextPart("summary"), part)
	if err := ValidateArtifact(&artifact, "artifact"); err != nil {
		t.Fatalf("artifact carrying the extension is invalid: %v", err)
	}

	back, tagged, err := NexusEventFromPart(part)
	if err != nil {
		t.Fatalf("decode part: %v", err)
	}
	if !tagged {
		t.Fatal("part was not recognized as carrying the extension")
	}
	if back.Thinking == nil || back.Thinking.Content != "hmm" {
		t.Errorf("payload changed: %+v", back)
	}

	// A plain part, and one tagged for a different extension, are both skipped.
	for name, other := range map[string]Part{
		"plain":           TextPart("hello"),
		"other extension": {Data: json.RawMessage(`{}`), Metadata: map[string]any{"a2aExtension": "https://example.test/x"}},
	} {
		t.Run(name, func(t *testing.T) {
			got, tagged, err := NexusEventFromPart(other)
			if err != nil || tagged || got != nil {
				t.Fatalf("got %v, %v, %v; want nil, false, nil", got, tagged, err)
			}
		})
	}

	// A tagged part with no data is a hard error, not a silent skip.
	empty := Part{Metadata: map[string]any{"a2aExtension": NexusExtensionURI}}
	if _, tagged, err := NexusEventFromPart(empty); err == nil || !tagged {
		t.Fatalf("empty tagged part: tagged=%v err=%v", tagged, err)
	}

	// An invalid event never becomes a part.
	if _, err := NexusEventPart(NexusEvent{Kind: NexusEventKindUsage}); err == nil {
		t.Fatal("invalid event was turned into a part")
	}
}

func TestNexusEventMetadataCarrier(t *testing.T) {
	e := UsageEvent(testTaskID, testCtxID, NexusTokenUsage{
		InputTokens: 10, OutputTokens: 5, TotalTokens: 15, Cumulative: true,
	})
	md, err := NexusEventMetadata(e)
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	if _, ok := md[NexusExtensionURI]; !ok {
		t.Fatalf("metadata is not keyed by the extension uri: %v", md)
	}

	// Attach it to a status update and round-trip the whole event through JSON,
	// which is what actually happens on the wire.
	update := NewStatusUpdate(testTaskID, testCtxID, NewTaskStatus(TaskStateWorking))
	update.Metadata = md
	if err := ValidateStatusUpdate(&update); err != nil {
		t.Fatalf("status update carrying the extension is invalid: %v", err)
	}

	data, err := Encode(update)
	if err != nil {
		t.Fatalf("encode update: %v", err)
	}
	decodedUpdate, err := DecodeStatusUpdate(data)
	if err != nil {
		t.Fatalf("decode update: %v", err)
	}

	back, present, err := NexusEventFromMetadata(decodedUpdate.Metadata)
	if err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if !present {
		t.Fatal("extension payload lost across the wire round trip")
	}
	if back.Usage == nil || back.Usage.TotalTokens != 15 || !back.Usage.Cumulative {
		t.Errorf("usage payload changed: %+v", back.Usage)
	}

	if got, present, err := NexusEventFromMetadata(map[string]any{"other": 1}); got != nil || present || err != nil {
		t.Errorf("unrelated metadata: got %v, %v, %v", got, present, err)
	}
	if got, present, err := NexusEventFromMetadata(nil); got != nil || present || err != nil {
		t.Errorf("nil metadata: got %v, %v, %v", got, present, err)
	}
}

// TestNexusExtensionOptInViaServiceParams pins the negotiation path: a client
// opts in through the A2A-Extensions service parameter.
func TestNexusExtensionOptInViaServiceParams(t *testing.T) {
	params := ServiceParams{Version: ProtocolVersion, Extensions: []string{NexusExtensionURI}}
	if !params.SupportsExtension(NexusExtensionURI) {
		t.Fatal("SupportsExtension missed the nexus extension")
	}
	if params.SupportsExtension("https://example.test/other") {
		t.Error("SupportsExtension matched an undeclared uri")
	}
	if got := ParseExtensions(NexusExtensionURI + ", https://example.test/x"); len(got) != 2 || got[0] != NexusExtensionURI {
		t.Errorf("ParseExtensions = %v", got)
	}
}

func TestDecodeNexusEventRejectsGarbage(t *testing.T) {
	if _, err := DecodeNexusEvent([]byte(`{"kind":"thinking"}`)); err == nil {
		t.Fatal("event with no payload arm accepted")
	}
	if _, err := DecodeNexusEvent([]byte(`not json`)); err == nil {
		t.Fatal("malformed json accepted")
	}
}
