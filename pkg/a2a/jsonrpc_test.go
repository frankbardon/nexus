package a2a

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestMethodNamesAreOneZeroPascalCase pins the exact JSON-RPC method strings
// from specification section 9.4, which mandates "PascalCase method names
// matching gRPC conventions". A2A 1.0 removed the 0.3-era dotted forms; this
// test exists so a regression to them is loud.
func TestMethodNamesAreOneZeroPascalCase(t *testing.T) {
	want := map[string]string{
		"SendMessage":                      MethodSendMessage,
		"SendStreamingMessage":             MethodSendStreamingMessage,
		"GetTask":                          MethodGetTask,
		"ListTasks":                        MethodListTasks,
		"CancelTask":                       MethodCancelTask,
		"SubscribeToTask":                  MethodSubscribeToTask,
		"CreateTaskPushNotificationConfig": MethodCreateTaskPushNotificationConfig,
		"GetTaskPushNotificationConfig":    MethodGetTaskPushNotificationConfig,
		"ListTaskPushNotificationConfigs":  MethodListTaskPushNotificationConfigs,
		"DeleteTaskPushNotificationConfig": MethodDeleteTaskPushNotificationConfig,
		"GetExtendedAgentCard":             MethodGetExtendedAgentCard,
	}
	for literal, constant := range want {
		if literal != constant {
			t.Errorf("method constant = %q, want the spec's verbatim %q", constant, literal)
		}
	}

	// The 0.3 dotted forms must not be accepted by the codec.
	for _, legacy := range []string{"message/send", "message/stream", "tasks/get", "tasks/list", "tasks/cancel", "tasks/resubscribe"} {
		body := `{"jsonrpc":"2.0","id":1,"method":"` + legacy + `","params":{"id":"t-1"}}`
		_, err := DecodeCall([]byte(body))
		if err == nil {
			t.Errorf("DecodeCall accepted 0.3-era method %q", legacy)
			continue
		}
		if err.Type != ErrorTypeMethodNotFound {
			t.Errorf("method %q: error type = %q, want %q", legacy, err.Type, ErrorTypeMethodNotFound)
		}
	}
}

// TestIsStreamingMethod pins which operations answer with SSE.
func TestIsStreamingMethod(t *testing.T) {
	tests := map[string]bool{
		MethodSendMessage:          false,
		MethodSendStreamingMessage: true,
		MethodGetTask:              false,
		MethodListTasks:            false,
		MethodCancelTask:           false,
		MethodSubscribeToTask:      true,
	}
	for method, want := range tests {
		if got := IsStreamingMethod(method); got != want {
			t.Errorf("IsStreamingMethod(%q) = %v, want %v", method, got, want)
		}
	}
}

// TestDecodeCallSuccess covers each supported operation, asserting the concrete
// parameter type and the fields that matter.
func TestDecodeCallSuccess(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantMethod    string
		wantStreaming bool
		check         func(t *testing.T, params any)
	}{
		{
			name: "SendMessage",
			body: `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m-1",` +
				`"role":"ROLE_USER","parts":[{"text":"Hello"}]},"configuration":{"acceptedOutputModes":["text/plain"]}}}`,
			wantMethod: MethodSendMessage,
			check: func(t *testing.T, params any) {
				p, ok := params.(*SendMessageRequest)
				if !ok {
					t.Fatalf("params type = %T, want *SendMessageRequest", params)
				}
				if p.Message.MessageID != "m-1" || p.Message.Role != RoleUser {
					t.Fatalf("message = %+v", p.Message)
				}
				if text, _ := p.Message.Parts[0].TextValue(); text != "Hello" {
					t.Fatalf("text = %q", text)
				}
			},
		},
		{
			name: "SendStreamingMessage shares the SendMessage params",
			body: `{"jsonrpc":"2.0","id":"abc","method":"SendStreamingMessage","params":{"message":{"messageId":"m-1",` +
				`"role":"ROLE_USER","parts":[{"text":"Write a report"}]}}}`,
			wantMethod:    MethodSendStreamingMessage,
			wantStreaming: true,
			check: func(t *testing.T, params any) {
				if _, ok := params.(*SendMessageRequest); !ok {
					t.Fatalf("params type = %T, want *SendMessageRequest", params)
				}
			},
		},
		{
			name:       "GetTask",
			body:       `{"jsonrpc":"2.0","id":2,"method":"GetTask","params":{"id":"task-uuid","historyLength":10}}`,
			wantMethod: MethodGetTask,
			check: func(t *testing.T, params any) {
				p, ok := params.(*GetTaskRequest)
				if !ok {
					t.Fatalf("params type = %T, want *GetTaskRequest", params)
				}
				if p.ID != "task-uuid" || p.HistoryLength == nil || *p.HistoryLength != 10 {
					t.Fatalf("params = %+v", p)
				}
			},
		},
		{
			name: "ListTasks",
			body: `{"jsonrpc":"2.0","id":3,"method":"ListTasks","params":{"contextId":"context-uuid",` +
				`"status":"TASK_STATE_WORKING","pageSize":50,"pageToken":"cursor-token"}}`,
			wantMethod: MethodListTasks,
			check: func(t *testing.T, params any) {
				p, ok := params.(*ListTasksRequest)
				if !ok {
					t.Fatalf("params type = %T, want *ListTasksRequest", params)
				}
				if p.ContextID != "context-uuid" || p.Status != TaskStateWorking {
					t.Fatalf("params = %+v", p)
				}
				if p.PageSize == nil || *p.PageSize != 50 || p.PageToken != "cursor-token" {
					t.Fatalf("pagination = %+v", p)
				}
			},
		},
		{
			name:       "ListTasks with no filters",
			body:       `{"jsonrpc":"2.0","id":3,"method":"ListTasks","params":{}}`,
			wantMethod: MethodListTasks,
			check: func(t *testing.T, params any) {
				p := params.(*ListTasksRequest)
				if p.PageSize != nil || p.Status != "" {
					t.Fatalf("expected an empty filter set, got %+v", p)
				}
			},
		},
		{
			name:       "CancelTask",
			body:       `{"jsonrpc":"2.0","id":4,"method":"CancelTask","params":{"id":"task-uuid"}}`,
			wantMethod: MethodCancelTask,
			check: func(t *testing.T, params any) {
				p, ok := params.(*CancelTaskRequest)
				if !ok {
					t.Fatalf("params type = %T, want *CancelTaskRequest", params)
				}
				if p.ID != "task-uuid" {
					t.Fatalf("id = %q", p.ID)
				}
			},
		},
		{
			name:          "SubscribeToTask",
			body:          `{"jsonrpc":"2.0","id":5,"method":"SubscribeToTask","params":{"id":"task-uuid"}}`,
			wantMethod:    MethodSubscribeToTask,
			wantStreaming: true,
			check: func(t *testing.T, params any) {
				p, ok := params.(*SubscribeToTaskRequest)
				if !ok {
					t.Fatalf("params type = %T, want *SubscribeToTaskRequest", params)
				}
				if p.ID != "task-uuid" {
					t.Fatalf("id = %q", p.ID)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			call, err := DecodeCall([]byte(tc.body))
			if err != nil {
				t.Fatalf("DecodeCall: %v", err)
			}
			if call.Method != tc.wantMethod {
				t.Fatalf("method = %q, want %q", call.Method, tc.wantMethod)
			}
			if call.Streaming() != tc.wantStreaming {
				t.Fatalf("Streaming() = %v, want %v", call.Streaming(), tc.wantStreaming)
			}
			tc.check(t, call.Params)
		})
	}
}

// TestDecodeCallErrors pins the JSON-RPC error object produced for every
// malformed or unsupported call shape the specification calls out.
func TestDecodeCallErrors(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantType ErrorType
		wantCode int
	}{
		{
			name:     "not JSON at all",
			body:     `{"jsonrpc": "2.0", `,
			wantType: ErrorTypeJSONParse,
			wantCode: CodeJSONParse,
		},
		{
			name:     "wrong jsonrpc version",
			body:     `{"jsonrpc":"1.0","id":1,"method":"GetTask","params":{"id":"t"}}`,
			wantType: ErrorTypeInvalidRequest,
			wantCode: CodeInvalidRequest,
		},
		{
			name:     "missing jsonrpc field",
			body:     `{"id":1,"method":"GetTask","params":{"id":"t"}}`,
			wantType: ErrorTypeInvalidRequest,
			wantCode: CodeInvalidRequest,
		},
		{
			name:     "missing method",
			body:     `{"jsonrpc":"2.0","id":1,"params":{}}`,
			wantType: ErrorTypeInvalidRequest,
			wantCode: CodeInvalidRequest,
		},
		{
			name:     "unknown method",
			body:     `{"jsonrpc":"2.0","id":1,"method":"Teleport","params":{}}`,
			wantType: ErrorTypeMethodNotFound,
			wantCode: CodeMethodNotFound,
		},
		{
			name:     "push notification config method is unsupported",
			body:     `{"jsonrpc":"2.0","id":1,"method":"CreateTaskPushNotificationConfig","params":{"url":"https://x"}}`,
			wantType: ErrorTypePushNotificationNotSupported,
			wantCode: CodePushNotificationNotSupported,
		},
		{
			name:     "extended agent card is unsupported",
			body:     `{"jsonrpc":"2.0","id":1,"method":"GetExtendedAgentCard"}`,
			wantType: ErrorTypeUnsupportedOperation,
			wantCode: CodeUnsupportedOperation,
		},
		{
			name:     "missing params",
			body:     `{"jsonrpc":"2.0","id":1,"method":"GetTask"}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name:     "null params",
			body:     `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":null}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name:     "params of the wrong shape",
			body:     `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":["task-1"]}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name:     "GetTask without an id",
			body:     `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{}}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name:     "CancelTask without an id",
			body:     `{"jsonrpc":"2.0","id":1,"method":"CancelTask","params":{}}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name:     "SendMessage without a message",
			body:     `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{}}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name:     "SendMessage with an empty parts array",
			body:     `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","parts":[]}}}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name: "SendMessage with a part setting two content arms",
			body: `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER",` +
				`"parts":[{"text":"hi","url":"https://example.com"}]}}}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name: "SendMessage with an unknown role",
			body: `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"user",` +
				`"parts":[{"text":"hi"}]}}}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name:     "ListTasks with an out-of-range page size",
			body:     `{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{"pageSize":500}}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name:     "ListTasks with an unknown status filter",
			body:     `{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{"status":"TASK_STATE_NOPE"}}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
		{
			name:     "GetTask with a negative history length",
			body:     `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"t","historyLength":-1}}`,
			wantType: ErrorTypeInvalidParams,
			wantCode: CodeInvalidParams,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			call, err := DecodeCall([]byte(tc.body))
			if err == nil {
				t.Fatalf("DecodeCall succeeded, want an error; call = %+v", call)
			}
			if err.Type != tc.wantType {
				t.Fatalf("error type = %q, want %q (message %q)", err.Type, tc.wantType, err.Message)
			}
			if err.Code() != tc.wantCode {
				t.Fatalf("error code = %d, want %d", err.Code(), tc.wantCode)
			}

			// Every failure must render into a well-formed JSON-RPC error
			// response.
			resp := NewErrorResponse(nil, err)
			data, encodeErr := resp.Encode()
			if encodeErr != nil {
				t.Fatalf("encode error response: %v", encodeErr)
			}
			var obj struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Error   struct {
					Code    int              `json:"code"`
					Message string           `json:"message"`
					Data    []map[string]any `json:"data"`
				} `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if unmarshalErr := json.Unmarshal(data, &obj); unmarshalErr != nil {
				t.Fatalf("decode error response: %v", unmarshalErr)
			}
			if obj.JSONRPC != JSONRPCVersion {
				t.Errorf("jsonrpc = %q", obj.JSONRPC)
			}
			if string(obj.ID) != "null" {
				t.Errorf("id = %s, want null for an unrecoverable request id", obj.ID)
			}
			if obj.Error.Code != tc.wantCode {
				t.Errorf("rendered code = %d, want %d", obj.Error.Code, tc.wantCode)
			}
			if obj.Error.Message == "" {
				t.Error("rendered error message is empty")
			}
			if obj.Result != nil {
				t.Error("error response must not carry a result")
			}
		})
	}
}

// TestErrorDetailShape pins the google.rpc.ErrorInfo detail object required for
// A2A-specific errors by specification sections 9.5 and 11.6.
func TestErrorDetailShape(t *testing.T) {
	err := ErrTaskNotFound("nonexistent-task-id")
	rpc := err.RPCError()

	if rpc.Code != CodeTaskNotFound {
		t.Fatalf("code = %d, want %d", rpc.Code, CodeTaskNotFound)
	}
	if len(rpc.Data) != 1 {
		t.Fatalf("data = %v, want exactly one ErrorInfo detail", rpc.Data)
	}
	detail := rpc.Data[0]
	if detail["@type"] != TypeErrorInfo {
		t.Errorf("@type = %v, want %q", detail["@type"], TypeErrorInfo)
	}
	if detail["reason"] != "TASK_NOT_FOUND" {
		t.Errorf("reason = %v, want TASK_NOT_FOUND", detail["reason"])
	}
	if detail["domain"] != ErrorDomain {
		t.Errorf("domain = %v, want %q", detail["domain"], ErrorDomain)
	}
	metadata, ok := detail["metadata"].(map[string]string)
	if !ok || metadata["taskId"] != "nonexistent-task-id" {
		t.Errorf("metadata = %v, want the offending task id", detail["metadata"])
	}
}

// TestErrorReasonsCoverTaxonomy checks that every A2A-specific error type
// carries the ErrorInfo reason and per-binding codes from section 5.4, and that
// transport-level errors deliberately carry no reason.
func TestErrorReasonsCoverTaxonomy(t *testing.T) {
	tests := []struct {
		errType    ErrorType
		code       int
		httpStatus int
		grpcStatus string
		reason     string
	}{
		{ErrorTypeTaskNotFound, -32001, 404, "NOT_FOUND", "TASK_NOT_FOUND"},
		{ErrorTypeTaskNotCancelable, -32002, 400, "FAILED_PRECONDITION", "TASK_NOT_CANCELABLE"},
		{ErrorTypePushNotificationNotSupported, -32003, 400, "FAILED_PRECONDITION", "PUSH_NOTIFICATION_NOT_SUPPORTED"},
		{ErrorTypeUnsupportedOperation, -32004, 400, "FAILED_PRECONDITION", "UNSUPPORTED_OPERATION"},
		{ErrorTypeContentTypeNotSupported, -32005, 400, "INVALID_ARGUMENT", "CONTENT_TYPE_NOT_SUPPORTED"},
		{ErrorTypeInvalidAgentResponse, -32006, 500, "INTERNAL", "INVALID_AGENT_RESPONSE"},
		{ErrorTypeExtendedAgentCardNotConfig, -32007, 400, "FAILED_PRECONDITION", "EXTENDED_AGENT_CARD_NOT_CONFIGURED"},
		{ErrorTypeExtensionSupportRequired, -32008, 400, "FAILED_PRECONDITION", "EXTENSION_SUPPORT_REQUIRED"},
		{ErrorTypeVersionNotSupported, -32009, 400, "FAILED_PRECONDITION", "VERSION_NOT_SUPPORTED"},

		{ErrorTypeJSONParse, -32700, 400, "INVALID_ARGUMENT", ""},
		{ErrorTypeInvalidRequest, -32600, 400, "INVALID_ARGUMENT", ""},
		{ErrorTypeMethodNotFound, -32601, 404, "NOT_FOUND", ""},
		{ErrorTypeInvalidParams, -32602, 400, "INVALID_ARGUMENT", ""},
		{ErrorTypeInternal, -32603, 500, "INTERNAL", ""},
	}
	for _, tc := range tests {
		t.Run(string(tc.errType), func(t *testing.T) {
			e := NewError(tc.errType, "")
			if e.Type != tc.errType {
				t.Fatalf("NewError produced type %q, want %q", e.Type, tc.errType)
			}
			if e.Message == "" {
				t.Error("an empty message must fall back to the spec's standard message")
			}
			if e.Code() != tc.code {
				t.Errorf("Code() = %d, want %d", e.Code(), tc.code)
			}
			if e.HTTPStatus() != tc.httpStatus {
				t.Errorf("HTTPStatus() = %d, want %d", e.HTTPStatus(), tc.httpStatus)
			}
			if e.GRPCStatus() != tc.grpcStatus {
				t.Errorf("GRPCStatus() = %q, want %q", e.GRPCStatus(), tc.grpcStatus)
			}
			if e.Reason() != tc.reason {
				t.Errorf("Reason() = %q, want %q", e.Reason(), tc.reason)
			}
		})
	}
}

// TestRequestResponseRoundTrip drives a full client-to-server-to-client cycle
// over the JSON-RPC envelopes.
func TestRequestResponseRoundTrip(t *testing.T) {
	params := SendMessageRequest{
		Message: NewUserMessage("msg-1", "Hello").InContext("ctx-1"),
		Configuration: &SendMessageConfiguration{
			AcceptedOutputModes: []string{"text/plain"},
			HistoryLength:       HistoryLength(10),
		},
	}
	req, err := NewRequest("req-1", MethodSendMessage, params)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	wire, err := req.Encode()
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}

	call, protoErr := DecodeCall(wire)
	if protoErr != nil {
		t.Fatalf("DecodeCall: %v", protoErr)
	}
	decoded, ok := call.Params.(*SendMessageRequest)
	if !ok {
		t.Fatalf("params type = %T", call.Params)
	}
	if !reflect.DeepEqual(*decoded, params) {
		t.Fatalf("params drifted:\nwant %+v\n got %+v", params, *decoded)
	}
	if string(call.ID()) != `"req-1"` {
		t.Fatalf("id = %s, want \"req-1\"", call.ID())
	}

	// Server side: answer with a task.
	task := Task{
		ID:        "task-1",
		ContextID: "ctx-1",
		Status:    TaskStatus{State: TaskStateCompleted},
		Artifacts: []Artifact{NewTextArtifact("a-1", "answer", "Hi there")},
	}
	resp, err := NewResultResponse(call.ID(), TaskResponse(task))
	if err != nil {
		t.Fatalf("NewResultResponse: %v", err)
	}
	respWire, err := resp.Encode()
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}

	// Client side: read it back.
	gotResp, err := DecodeResponse(respWire)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if string(gotResp.ID) != `"req-1"` {
		t.Fatalf("response id = %s, want the request id echoed back", gotResp.ID)
	}
	var result SendMessageResponse
	if err := gotResp.DecodeResult(&result); err != nil {
		t.Fatalf("DecodeResult: %v", err)
	}
	if result.Task == nil || result.Task.ID != "task-1" {
		t.Fatalf("result = %+v", result)
	}
	if err := ValidateSendMessageResponse(&result); err != nil {
		t.Fatalf("validate result: %v", err)
	}
}

// TestDecodeResponseErrors covers malformed and error-carrying responses on the
// client side.
func TestDecodeResponseErrors(t *testing.T) {
	t.Run("wrong jsonrpc version", func(t *testing.T) {
		if _, err := DecodeResponse([]byte(`{"jsonrpc":"1.0","id":1,"result":{}}`)); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("both result and error", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-32001,"message":"nope"}}`
		if _, err := DecodeResponse([]byte(body)); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("neither result nor error", func(t *testing.T) {
		resp, err := DecodeResponse([]byte(`{"jsonrpc":"2.0","id":1}`))
		if err != nil {
			t.Fatalf("DecodeResponse: %v", err)
		}
		var dst SendMessageResponse
		if err := resp.DecodeResult(&dst); err == nil {
			t.Fatal("expected an error from DecodeResult")
		}
	})
	t.Run("error is recovered with its taxonomy type", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":2,"error":{"code":-32001,"message":"Task not found",` +
			`"data":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TASK_NOT_FOUND","domain":"a2a-protocol.org"}]}}`
		resp, err := DecodeResponse([]byte(body))
		if err != nil {
			t.Fatalf("DecodeResponse: %v", err)
		}
		var dst SendMessageResponse
		decodeErr := resp.DecodeResult(&dst)
		if decodeErr == nil {
			t.Fatal("expected the response error to surface")
		}
		protoErr, ok := decodeErr.(*Error)
		if !ok {
			t.Fatalf("error type = %T, want *a2a.Error", decodeErr)
		}
		if protoErr.Type != ErrorTypeTaskNotFound {
			t.Fatalf("recovered type = %q, want %q", protoErr.Type, ErrorTypeTaskNotFound)
		}
	})
	t.Run("unmapped code falls back to internal", func(t *testing.T) {
		rpc := &RPCError{Code: -32999, Message: "who knows"}
		if got := rpc.AsError().Type; got != ErrorTypeInternal {
			t.Fatalf("type = %q, want %q", got, ErrorTypeInternal)
		}
	})
}

// TestNewResultResponseNilID defaults a missing id to JSON null, as JSON-RPC
// requires for responses to unidentifiable requests.
func TestNewResultResponseNilID(t *testing.T) {
	resp, err := NewResultResponse(nil, map[string]string{"ok": "yes"})
	if err != nil {
		t.Fatalf("NewResultResponse: %v", err)
	}
	if string(resp.ID) != "null" {
		t.Fatalf("id = %s, want null", resp.ID)
	}
}

// TestMethodsListing checks the advertised operation list matches what
// DecodeCall actually accepts.
func TestMethodsListing(t *testing.T) {
	for _, method := range Methods() {
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{"id":"t-1",` +
			`"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]}}}`
		if _, err := DecodeCall([]byte(body)); err != nil {
			t.Errorf("Methods() advertises %q but DecodeCall rejects it: %v", method, err)
		}
	}
}
