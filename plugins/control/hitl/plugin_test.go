package hitl

import (
	"encoding/json"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestBuildRequestFromToolCallFreeText(t *testing.T) {
	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "call-1",
		Name:   "ask_user",
		TurnID: "turn-1",
		Arguments: map[string]any{
			"prompt": "what is your name?",
		},
	}
	req, errMsg := buildRequestFromToolCall(tc)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if req.Mode != events.HITLModeFreeText {
		t.Fatalf("Mode = %q; want free_text", req.Mode)
	}
	if req.Prompt != "what is your name?" {
		t.Fatalf("Prompt = %q", req.Prompt)
	}
	if req.ID == "" {
		t.Fatal("ID empty")
	}
	if len(req.Choices) != 0 {
		t.Fatalf("Choices = %v; want empty", req.Choices)
	}
	if req.ActionKind != "free_text" {
		t.Fatalf("ActionKind = %q; want free_text", req.ActionKind)
	}
}

func TestBuildRequestFromToolCallChoices(t *testing.T) {
	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "call-2",
		Name:   "ask_user",
		TurnID: "turn-2",
		Arguments: map[string]any{
			"prompt": "approve?",
			"mode":   "choices",
			"choices": []any{
				map[string]any{"id": "allow", "label": "Approve"},
				map[string]any{"id": "reject", "label": "Reject"},
			},
			"default_choice_id": "reject",
		},
	}
	req, errMsg := buildRequestFromToolCall(tc)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if req.Mode != events.HITLModeChoices {
		t.Fatalf("Mode = %q; want choices", req.Mode)
	}
	if len(req.Choices) != 2 || req.Choices[0].ID != "allow" || req.Choices[1].ID != "reject" {
		t.Fatalf("Choices = %v; want allow,reject", req.Choices)
	}
	if req.DefaultChoiceID != "reject" {
		t.Fatalf("DefaultChoiceID = %q; want reject", req.DefaultChoiceID)
	}
	if req.ActionKind != "ask_user.choices" {
		t.Fatalf("ActionKind = %q; want ask_user.choices", req.ActionKind)
	}
}

func TestBuildRequestRejectsUnknownDefault(t *testing.T) {
	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "call-3",
		Name:   "ask_user",
		TurnID: "turn-3",
		Arguments: map[string]any{
			"prompt": "pick",
			"mode":   "choices",
			"choices": []any{
				map[string]any{"id": "a", "label": "A"},
			},
			"default_choice_id": "missing",
		},
	}
	_, errMsg := buildRequestFromToolCall(tc)
	if errMsg == "" {
		t.Fatal("expected error for unknown default_choice_id")
	}
}

func TestBuildRequestRejectsChoicesModeWithoutChoices(t *testing.T) {
	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "call-4",
		Name:   "ask_user",
		TurnID: "turn-4",
		Arguments: map[string]any{
			"prompt": "pick",
			"mode":   "choices",
		},
	}
	_, errMsg := buildRequestFromToolCall(tc)
	if errMsg == "" {
		t.Fatal("expected error when choices missing in choices mode")
	}
}

func TestBuildRequestRejectsDuplicateChoiceID(t *testing.T) {
	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "call-5",
		Name:   "ask_user",
		TurnID: "turn-5",
		Arguments: map[string]any{
			"prompt": "pick",
			"mode":   "choices",
			"choices": []any{
				map[string]any{"id": "a", "label": "A"},
				map[string]any{"id": "a", "label": "Another A"},
			},
		},
	}
	_, errMsg := buildRequestFromToolCall(tc)
	if errMsg == "" {
		t.Fatal("expected error for duplicate choice ID")
	}
}

func TestEncodeResponseForLLMShapes(t *testing.T) {
	cases := []struct {
		name   string
		resp   events.HITLResponse
		want   map[string]string
		errStr string
	}{
		{
			name: "freetext",
			resp: events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: "r1", FreeText: "hi"},
			want: map[string]string{"free_text": "hi"},
		},
		{
			name: "choice",
			resp: events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: "r2", ChoiceID: "allow"},
			want: map[string]string{"choice_id": "allow"},
		},
		{
			name: "both",
			resp: events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: "r3", ChoiceID: "edit", FreeText: "trim"},
			want: map[string]string{"choice_id": "edit", "free_text": "trim"},
		},
		{
			name:   "cancelled",
			resp:   events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: "r4", Cancelled: true, CancelReason: "deadline"},
			errStr: "deadline",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut := encodeResponseForLLM(tc.resp)
			if tc.errStr != "" {
				if errOut != tc.errStr {
					t.Fatalf("err = %q; want %q", errOut, tc.errStr)
				}
				return
			}
			if errOut != "" {
				t.Fatalf("unexpected err = %q", errOut)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("json.Unmarshal(%q): %v", out, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v; want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got[%q] = %q; want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestToHITLResponseFromMap(t *testing.T) {
	m := map[string]any{
		"request_id": "r1",
		"choice_id":  "allow",
		"free_text":  "ok",
	}
	got, ok := toHITLResponse(m)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.RequestID != "r1" || got.ChoiceID != "allow" || got.FreeText != "ok" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestToHITLResponseRejectsMissingID(t *testing.T) {
	m := map[string]any{
		"choice_id": "allow",
	}
	if _, ok := toHITLResponse(m); ok {
		t.Fatal("expected ok=false for missing request_id")
	}
}
