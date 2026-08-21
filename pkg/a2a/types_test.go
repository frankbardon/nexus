package a2a

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// specFixtures are hand-written JSON documents transcribed from the A2A 1.0
// specification's own worked examples (sections 6.2, 6.3, 6.7, 6.8, 11.4) and
// from the field lists in the canonical a2a.proto. Each decodes into a typed
// value, and each typed value re-encodes to semantically identical JSON.
type specFixture struct {
	name string
	json string
	// target returns a fresh zero value of the type under test.
	target func() any
	// check optionally asserts on the decoded value.
	check func(t *testing.T, v any)
}

func specFixtures() []specFixture {
	return []specFixture{
		{
			name:   "task/streaming-open-frame",
			json:   `{"id":"task-uuid","contextId":"context-uuid","status":{"state":"TASK_STATE_WORKING"}}`,
			target: func() any { return new(Task) },
			check: func(t *testing.T, v any) {
				task := v.(*Task)
				if task.ID != "task-uuid" || task.ContextID != "context-uuid" {
					t.Fatalf("ids = %q/%q", task.ID, task.ContextID)
				}
				if task.Status.State != TaskStateWorking {
					t.Fatalf("state = %q", task.Status.State)
				}
			},
		},
		{
			name: "task/input-required-with-question",
			json: `{"id":"task-uuid","status":{"state":"TASK_STATE_INPUT_REQUIRED",` +
				`"message":{"messageId":"q-1","role":"ROLE_AGENT",` +
				`"parts":[{"text":"I need more details. Where would you like to fly from and to?"}]}}}`,
			target: func() any { return new(Task) },
			check: func(t *testing.T, v any) {
				task := v.(*Task)
				if !task.Status.State.IsInterrupted() {
					t.Fatalf("state %q should be interrupted", task.Status.State)
				}
				if task.Status.Message == nil {
					t.Fatal("status message missing")
				}
				text, ok := task.Status.Message.Parts[0].TextValue()
				if !ok || text == "" {
					t.Fatalf("question text = %q, ok=%v", text, ok)
				}
			},
		},
		{
			name: "task/with-artifacts-and-history",
			json: `{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED",` +
				`"timestamp":"2025-10-28T10:30:00.000Z"},` +
				`"artifacts":[{"artifactId":"a-1","name":"report","parts":[{"text":"# Report"}]}],` +
				`"history":[{"messageId":"m-1","role":"ROLE_USER","parts":[{"text":"hi"}]}],` +
				`"metadata":{"source":"nexus"}}`,
			target: func() any { return new(Task) },
			check: func(t *testing.T, v any) {
				task := v.(*Task)
				if len(task.Artifacts) != 1 || len(task.History) != 1 {
					t.Fatalf("artifacts=%d history=%d", len(task.Artifacts), len(task.History))
				}
				if task.Status.Timestamp == nil {
					t.Fatal("timestamp missing")
				}
				want := time.Date(2025, 10, 28, 10, 30, 0, 0, time.UTC)
				if !task.Status.Timestamp.Equal(want) {
					t.Fatalf("timestamp = %v, want %v", task.Status.Timestamp.Time, want)
				}
				if task.Metadata["source"] != "nexus" {
					t.Fatalf("metadata = %v", task.Metadata)
				}
			},
		},
		{
			name:   "message/user-text",
			json:   `{"messageId":"msg-uuid","role":"ROLE_USER","parts":[{"text":"Write a detailed report on climate change"}]}`,
			target: func() any { return new(Message) },
		},
		{
			name: "message/resume-carries-task-and-context",
			json: `{"messageId":"msg-2","contextId":"context-uuid","taskId":"task-uuid","role":"ROLE_USER",` +
				`"parts":[{"text":"From San Francisco to New York"}],"referenceTaskIds":["task-0"],` +
				`"extensions":["https://example.com/ext/v1"]}`,
			target: func() any { return new(Message) },
			check: func(t *testing.T, v any) {
				m := v.(*Message)
				if m.TaskID != "task-uuid" || m.ContextID != "context-uuid" {
					t.Fatalf("taskId=%q contextId=%q", m.TaskID, m.ContextID)
				}
				if len(m.ReferenceTaskIDs) != 1 || len(m.Extensions) != 1 {
					t.Fatalf("refs=%v exts=%v", m.ReferenceTaskIDs, m.Extensions)
				}
			},
		},
		{
			name:   "part/file-by-url",
			json:   `{"url":"https://example.com/report.pdf","mediaType":"application/pdf","filename":"report.pdf"}`,
			target: func() any { return new(Part) },
			check: func(t *testing.T, v any) {
				p := v.(*Part)
				if p.Kind() != PartKindURL {
					t.Fatalf("kind = %q", p.Kind())
				}
				if got, ok := p.URLValue(); !ok || got != "https://example.com/report.pdf" {
					t.Fatalf("url = %q ok=%v", got, ok)
				}
			},
		},
		{
			name:   "part/file-inline-base64",
			json:   `{"raw":"aGVsbG8=","mediaType":"text/plain","filename":"hello.txt"}`,
			target: func() any { return new(Part) },
			check: func(t *testing.T, v any) {
				p := v.(*Part)
				if p.Kind() != PartKindRaw {
					t.Fatalf("kind = %q", p.Kind())
				}
				if string(p.Raw) != "hello" {
					t.Fatalf("raw = %q, want base64-decoded %q", p.Raw, "hello")
				}
			},
		},
		{
			name:   "part/structured-data",
			json:   `{"data":{"temperature":22,"unit":"celsius"},"mediaType":"application/json"}`,
			target: func() any { return new(Part) },
			check: func(t *testing.T, v any) {
				p := v.(*Part)
				if p.Kind() != PartKindData {
					t.Fatalf("kind = %q", p.Kind())
				}
			},
		},
		{
			name:   "part/empty-text-is-not-unset",
			json:   `{"text":""}`,
			target: func() any { return new(Part) },
			check: func(t *testing.T, v any) {
				p := v.(*Part)
				if p.Kind() != PartKindText {
					t.Fatalf("kind = %q, an explicitly empty text part must still be a text part", p.Kind())
				}
			},
		},
		{
			name: "artifact/streamed-chunk",
			json: `{"taskId":"task-uuid","contextId":"context-uuid",` +
				`"artifact":{"artifactId":"a-1","parts":[{"text":"# Climate Change Report\n\n"}]},` +
				`"append":true,"lastChunk":true}`,
			target: func() any { return new(TaskArtifactUpdateEvent) },
			check: func(t *testing.T, v any) {
				e := v.(*TaskArtifactUpdateEvent)
				if !e.Append || !e.LastChunk {
					t.Fatalf("append=%v lastChunk=%v", e.Append, e.LastChunk)
				}
			},
		},
		{
			name:   "statusUpdate/terminal",
			json:   `{"taskId":"task-uuid","contextId":"context-uuid","status":{"state":"TASK_STATE_COMPLETED"},"metadata":{"turn":"1"}}`,
			target: func() any { return new(TaskStatusUpdateEvent) },
			check: func(t *testing.T, v any) {
				e := v.(*TaskStatusUpdateEvent)
				if !e.Status.State.IsTerminal() {
					t.Fatal("completed must be terminal")
				}
			},
		},
		{
			name: "sendMessageRequest/with-configuration",
			json: `{"message":{"messageId":"msg-uuid","role":"ROLE_USER","parts":[{"text":"Hello"}]},` +
				`"configuration":{"acceptedOutputModes":["text/plain"],"historyLength":10,"returnImmediately":true}}`,
			target: func() any { return new(SendMessageRequest) },
			check: func(t *testing.T, v any) {
				r := v.(*SendMessageRequest)
				if r.Configuration == nil || r.Configuration.HistoryLength == nil || *r.Configuration.HistoryLength != 10 {
					t.Fatalf("configuration = %+v", r.Configuration)
				}
				if !r.Configuration.ReturnImmediately {
					t.Fatal("returnImmediately should be true")
				}
			},
		},
		{
			name:   "sendMessageResponse/task-arm",
			json:   `{"task":{"id":"task-uuid","contextId":"context-uuid","status":{"state":"TASK_STATE_COMPLETED"}}}`,
			target: func() any { return new(SendMessageResponse) },
		},
		{
			name:   "sendMessageResponse/message-arm",
			json:   `{"message":{"messageId":"m-1","role":"ROLE_AGENT","parts":[{"text":"pong"}]}}`,
			target: func() any { return new(SendMessageResponse) },
		},
		{
			name: "streamResponse/artifact-update-arm",
			json: `{"artifactUpdate":{"taskId":"task-uuid","contextId":"context-uuid",` +
				`"artifact":{"artifactId":"a-1","parts":[{"text":"chunk"}]}}}`,
			target: func() any { return new(StreamResponse) },
			check: func(t *testing.T, v any) {
				s := v.(*StreamResponse)
				if s.Kind() != StreamPayloadArtifactUpdate {
					t.Fatalf("kind = %q", s.Kind())
				}
			},
		},
		{
			name: "listTasksResponse/paginated",
			json: `{"tasks":[{"id":"t-1","status":{"state":"TASK_STATE_WORKING"}}],` +
				`"nextPageToken":"cursor-token","pageSize":50,"totalSize":137}`,
			target: func() any { return new(ListTasksResponse) },
			check: func(t *testing.T, v any) {
				r := v.(*ListTasksResponse)
				if r.TotalSize != 137 || r.PageSize != 50 || r.NextPageToken != "cursor-token" {
					t.Fatalf("pagination = %+v", r)
				}
			},
		},
	}
}

// TestSpecFixtureRoundTrip decodes each fixture, re-encodes it, and requires the
// result to be semantically identical to the input JSON. This catches wrong
// field names, dropped fields and enum drift in one assertion.
func TestSpecFixtureRoundTrip(t *testing.T) {
	for _, fx := range specFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			target := fx.target()
			if err := json.Unmarshal([]byte(fx.json), target); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if fx.check != nil {
				fx.check(t, target)
			}

			encoded, err := Encode(target)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			assertJSONEqual(t, fx.json, string(encoded))

			// Decoding the re-encoded form must reproduce the same Go value.
			reDecoded := fx.target()
			if err := json.Unmarshal(encoded, reDecoded); err != nil {
				t.Fatalf("re-decode: %v", err)
			}
			if !reflect.DeepEqual(target, reDecoded) {
				t.Fatalf("value drifted across round trip:\n first: %+v\nsecond: %+v", target, reDecoded)
			}
		})
	}
}

// assertJSONEqual compares two JSON documents structurally, ignoring key order.
func assertJSONEqual(t *testing.T, want, got string) {
	t.Helper()
	var wantVal, gotVal any
	if err := json.Unmarshal([]byte(want), &wantVal); err != nil {
		t.Fatalf("parse want: %v", err)
	}
	if err := json.Unmarshal([]byte(got), &gotVal); err != nil {
		t.Fatalf("parse got: %v", err)
	}
	if !reflect.DeepEqual(wantVal, gotVal) {
		t.Fatalf("JSON mismatch:\nwant %s\n got %s", want, got)
	}
}

// TestPartIsFlattened pins the A2A 1.0 Part shape: one struct with a content
// oneof, not the 0.3-era TextPart/FilePart/DataPart hierarchy. A Part must never
// serialize a "kind" or "type" discriminator, and must never nest its file
// fields under a "file" object.
func TestPartIsFlattened(t *testing.T) {
	tests := []struct {
		name     string
		part     Part
		wantKeys []string
	}{
		{name: "text", part: TextPart("hello"), wantKeys: []string{"text"}},
		{name: "url", part: URLPart("https://example.com/a.pdf", "application/pdf"), wantKeys: []string{"url", "mediaType"}},
		{name: "raw", part: RawPart([]byte("hello"), "text/plain", "hello.txt"), wantKeys: []string{"raw", "mediaType", "filename"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := Encode(tc.part)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var obj map[string]any
			if err := json.Unmarshal(data, &obj); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(obj) != len(tc.wantKeys) {
				t.Fatalf("keys = %v, want exactly %v", obj, tc.wantKeys)
			}
			for _, key := range tc.wantKeys {
				if _, ok := obj[key]; !ok {
					t.Fatalf("missing key %q in %v", key, obj)
				}
			}
			for _, forbidden := range []string{"kind", "type", "file"} {
				if _, ok := obj[forbidden]; ok {
					t.Fatalf("part carries 0.3-era key %q: %v", forbidden, obj)
				}
			}
		})
	}
}

// TestDataPart covers the structured-data arm, which marshals eagerly.
func TestDataPart(t *testing.T) {
	p, err := DataPart(map[string]any{"temperature": 22})
	if err != nil {
		t.Fatalf("DataPart: %v", err)
	}
	if p.Kind() != PartKindData {
		t.Fatalf("kind = %q", p.Kind())
	}
	if _, err := DataPart(func() {}); err == nil {
		t.Fatal("expected an error for an unmarshalable data value")
	}
}

// TestTimestampFormat pins specification section 5.6.1: ISO 8601, UTC, 'Z'
// suffix, millisecond precision.
func TestTimestampFormat(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "utc",
			in:   time.Date(2025, 10, 28, 10, 30, 0, 0, time.UTC),
			want: `"2025-10-28T10:30:00.000Z"`,
		},
		{
			name: "milliseconds preserved",
			in:   time.Date(2025, 10, 28, 14, 25, 33, 142_000_000, time.UTC),
			want: `"2025-10-28T14:25:33.142Z"`,
		},
		{
			name: "non-utc offset is normalized to Z",
			in:   time.Date(2025, 10, 28, 12, 30, 0, 0, time.FixedZone("CET", 2*60*60)),
			want: `"2025-10-28T10:30:00.000Z"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(NewTimestamp(tc.in))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("marshal = %s, want %s", got, tc.want)
			}
			var back Timestamp
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !back.Equal(tc.in) {
				t.Fatalf("round trip = %v, want %v", back.Time, tc.in)
			}
		})
	}
}

// TestTimestampDecodeTolerance covers inputs a peer may legitimately send.
func TestTimestampDecodeTolerance(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		want    time.Time
	}{
		{name: "no fractional seconds", in: `"2023-10-27T10:00:00Z"`, want: time.Date(2023, 10, 27, 10, 0, 0, 0, time.UTC)},
		{name: "offset normalized", in: `"2023-10-27T12:00:00+02:00"`, want: time.Date(2023, 10, 27, 10, 0, 0, 0, time.UTC)},
		{name: "nanosecond precision", in: `"2023-10-27T10:00:00.123456789Z"`, want: time.Date(2023, 10, 27, 10, 0, 0, 123456789, time.UTC)},
		{name: "not a string", in: `12345`, wantErr: true},
		{name: "not a timestamp", in: `"yesterday"`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ts Timestamp
			err := json.Unmarshal([]byte(tc.in), &ts)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !ts.Equal(tc.want) {
				t.Fatalf("= %v, want %v", ts.Time, tc.want)
			}
		})
	}
}

// TestStreamResponseTerminalState checks the helper a stream writer uses to
// decide when to close, per specification section 11.7.
func TestStreamResponseTerminalState(t *testing.T) {
	tests := []struct {
		name         string
		frame        StreamResponse
		wantState    TaskState
		wantTerminal bool
	}{
		{
			name:         "task snapshot working",
			frame:        StreamTask(Task{ID: "t", Status: TaskStatus{State: TaskStateWorking}}),
			wantState:    TaskStateWorking,
			wantTerminal: false,
		},
		{
			name:         "task snapshot failed",
			frame:        StreamTask(Task{ID: "t", Status: TaskStatus{State: TaskStateFailed}}),
			wantState:    TaskStateFailed,
			wantTerminal: true,
		},
		{
			name:         "status update canceled",
			frame:        StreamStatusUpdate(NewStatusUpdate("t", "c", TaskStatus{State: TaskStateCanceled})),
			wantState:    TaskStateCanceled,
			wantTerminal: true,
		},
		{
			name:         "status update input required is not terminal",
			frame:        StreamStatusUpdate(NewStatusUpdate("t", "c", TaskStatus{State: TaskStateInputRequired})),
			wantState:    TaskStateInputRequired,
			wantTerminal: false,
		},
		{
			name:         "artifact update carries no state",
			frame:        StreamArtifactUpdate(NewArtifactUpdate("t", "c", NewTextArtifact("a", "n", "x"))),
			wantState:    TaskStateUnspecified,
			wantTerminal: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, terminal := tc.frame.TerminalState()
			if state != tc.wantState || terminal != tc.wantTerminal {
				t.Fatalf("= (%q, %v), want (%q, %v)", state, terminal, tc.wantState, tc.wantTerminal)
			}
		})
	}
}
