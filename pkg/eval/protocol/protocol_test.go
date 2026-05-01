package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRequest_RoundTrip(t *testing.T) {
	in := &Request{
		Schema:       SchemaVersion,
		ConfigInline: "core:\n  log_level: warn\n",
		UserInput:    "hello",
		MaxTurns:     4,
		Metadata: map[string]any{
			"case_id": "swe-bench-1234",
			"nested":  map[string]any{"x": float64(1)},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := ParseRequest(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if got.Schema != in.Schema {
		t.Errorf("schema=%d want %d", got.Schema, in.Schema)
	}
	if got.UserInput != in.UserInput {
		t.Errorf("user_input=%q want %q", got.UserInput, in.UserInput)
	}
	if got.MaxTurns != in.MaxTurns {
		t.Errorf("max_turns=%d want %d", got.MaxTurns, in.MaxTurns)
	}
	if got.ConfigInline != in.ConfigInline {
		t.Errorf("config_inline mismatch")
	}
	if got.Metadata["case_id"] != "swe-bench-1234" {
		t.Errorf("metadata round-trip lost case_id: %v", got.Metadata)
	}
}

func TestParseRequest_RejectsUnknownFields(t *testing.T) {
	body := `{"schema":1,"config_inline":"x","user_input":"y","extra_field":"nope"}`
	_, err := ParseRequest(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "extra_field") {
		t.Errorf("error should name the unknown field: %v", err)
	}
}

func TestParseRequest_EmptyStdin(t *testing.T) {
	_, err := ParseRequest(strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for empty stdin")
	}
}

func TestParseRequest_TrailingData(t *testing.T) {
	body := `{"schema":1,"config_inline":"x","user_input":"y"}{"another":"obj"}`
	_, err := ParseRequest(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error for trailing data")
	}
}

func TestRequest_Validate(t *testing.T) {
	cases := []struct {
		name    string
		req     *Request
		wantErr string // substring; "" means no error expected
		code    string
	}{
		{
			name:    "missing both config sources",
			req:     &Request{Schema: SchemaVersion, UserInput: "hi"},
			wantErr: "config_path or config_inline",
			code:    CodeInvalidRequest,
		},
		{
			name: "both config sources",
			req: &Request{
				Schema:       SchemaVersion,
				ConfigPath:   "/tmp/x.yaml",
				ConfigInline: "core: {}",
				UserInput:    "hi",
			},
			wantErr: "mutually exclusive",
			code:    CodeInvalidRequest,
		},
		{
			name:    "missing user_input",
			req:     &Request{Schema: SchemaVersion, ConfigInline: "core: {}"},
			wantErr: "user_input",
			code:    CodeInvalidRequest,
		},
		{
			name:    "wrong schema",
			req:     &Request{Schema: 99, ConfigInline: "core: {}", UserInput: "hi"},
			wantErr: "schema=99",
			code:    CodeInvalidRequest,
		},
		{
			name:    "negative max_turns",
			req:     &Request{Schema: SchemaVersion, ConfigInline: "core: {}", UserInput: "hi", MaxTurns: -1},
			wantErr: "max_turns",
			code:    CodeInvalidRequest,
		},
		{
			name: "valid",
			req: &Request{
				Schema:       SchemaVersion,
				ConfigInline: "core: {}",
				UserInput:    "hi",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q missing %q", err.Error(), tc.wantErr)
			}
			code, _ := MapError(err)
			if code != tc.code {
				t.Errorf("code=%q want %q", code, tc.code)
			}
		})
	}
}

func TestWriteResponse_RoundTripsMetadata(t *testing.T) {
	resp := &Response{
		Schema:                SchemaVersion,
		SessionID:             "abc",
		FinalAssistantMessage: "the answer",
		ToolCalls: []ToolCall{
			{
				Tool:          "shell",
				Args:          map[string]any{"cmd": "ls"},
				ResultSummary: "file1\nfile2",
				DurationMs:    42,
			},
		},
		Tokens:    Tokens{Input: 10, Output: 5},
		LatencyMs: 1234,
		Metadata: map[string]any{
			"case_id": "round-trip",
		},
	}
	var buf bytes.Buffer
	if err := WriteResponse(&buf, resp); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	var decoded Response
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.SessionID != "abc" {
		t.Errorf("session_id mismatch")
	}
	if decoded.Tokens.Input != 10 || decoded.Tokens.Output != 5 {
		t.Errorf("tokens mismatch: %+v", decoded.Tokens)
	}
	if decoded.LatencyMs != 1234 {
		t.Errorf("latency mismatch")
	}
	if got := decoded.Metadata["case_id"]; got != "round-trip" {
		t.Errorf("metadata round-trip lost: %v", got)
	}
	if len(decoded.ToolCalls) != 1 || decoded.ToolCalls[0].Tool != "shell" {
		t.Errorf("tool_calls round-trip lost: %+v", decoded.ToolCalls)
	}
}

func TestMapError_ContextErrors(t *testing.T) {
	code, msg := MapError(errors.New("plain"))
	if code != CodeInternal {
		t.Errorf("plain error code=%q want %q", code, CodeInternal)
	}
	_ = msg

	code, _ = MapError(ErrTimeout("x"))
	if code != CodeTimeout {
		t.Errorf("timeout code=%q want %q", code, CodeTimeout)
	}

	code, _ = MapError(ErrEngineBoot(errors.New("boot")))
	if code != CodeEngineBoot {
		t.Errorf("engine boot code=%q want %q", code, CodeEngineBoot)
	}
}

func TestResponseFromError_PopulatesError(t *testing.T) {
	resp := ResponseFromError(ErrConfigLoad(errors.New("missing file")), map[string]any{"k": "v"})
	if resp.Error == nil {
		t.Fatal("expected populated error")
	}
	if resp.Error.Code != CodeConfigLoad {
		t.Errorf("code=%q want %q", resp.Error.Code, CodeConfigLoad)
	}
	if resp.Metadata["k"] != "v" {
		t.Errorf("metadata mismatch")
	}
	if resp.Schema != SchemaVersion {
		t.Errorf("schema=%d want %d", resp.Schema, SchemaVersion)
	}
}
