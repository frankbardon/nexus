package a2a

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// JSONRPCVersion is the only value the "jsonrpc" field may carry.
const JSONRPCVersion = "2.0"

// A2A operation names. In A2A 1.0 the JSON-RPC method string is the PascalCase
// operation name, matching the gRPC method (specification sections 5.3 and 9.4).
// This is a breaking change from the 0.3-era dotted forms ("message/send",
// "tasks/get"), which this codec deliberately does not accept.
const (
	MethodSendMessage          = "SendMessage"
	MethodSendStreamingMessage = "SendStreamingMessage"
	MethodGetTask              = "GetTask"
	MethodListTasks            = "ListTasks"
	MethodCancelTask           = "CancelTask"
	MethodSubscribeToTask      = "SubscribeToTask"

	MethodCreateTaskPushNotificationConfig = "CreateTaskPushNotificationConfig"
	MethodGetTaskPushNotificationConfig    = "GetTaskPushNotificationConfig"
	MethodListTaskPushNotificationConfigs  = "ListTaskPushNotificationConfigs"
	MethodDeleteTaskPushNotificationConfig = "DeleteTaskPushNotificationConfig"

	MethodGetExtendedAgentCard = "GetExtendedAgentCard"
)

// supportedMethods are the operations this codec decodes into typed parameters.
var supportedMethods = map[string]bool{
	MethodSendMessage:          true,
	MethodSendStreamingMessage: true,
	MethodGetTask:              true,
	MethodListTasks:            true,
	MethodCancelTask:           true,
	MethodSubscribeToTask:      true,
}

// knownMethods are every operation the specification defines, including those
// this codec does not implement.
var knownMethods = map[string]ErrorType{
	// The push-notification operations are defined by the spec but not
	// implemented here. Section 3.3.4 requires PushNotificationNotSupportedError
	// when the agent card does not advertise the capability.
	MethodCreateTaskPushNotificationConfig: ErrorTypePushNotificationNotSupported,
	MethodGetTaskPushNotificationConfig:    ErrorTypePushNotificationNotSupported,
	MethodListTaskPushNotificationConfigs:  ErrorTypePushNotificationNotSupported,
	MethodDeleteTaskPushNotificationConfig: ErrorTypePushNotificationNotSupported,
	// Section 3.3.4 likewise requires UnsupportedOperationError when the
	// extendedAgentCard capability is absent.
	MethodGetExtendedAgentCard: ErrorTypeUnsupportedOperation,
}

// Methods returns the operation names this codec decodes, in a stable order.
func Methods() []string {
	return []string{
		MethodSendMessage,
		MethodSendStreamingMessage,
		MethodGetTask,
		MethodListTasks,
		MethodCancelTask,
		MethodSubscribeToTask,
	}
}

// IsStreamingMethod reports whether an operation responds with an SSE stream
// rather than a single JSON-RPC response.
func IsStreamingMethod(method string) bool {
	return method == MethodSendStreamingMessage || method == MethodSubscribeToTask
}

// ---- Envelopes ----

// Request is a JSON-RPC 2.0 request envelope.
type Request struct {
	// JSONRPC must be "2.0".
	JSONRPC string `json:"jsonrpc"`
	// ID correlates the response. JSON-RPC permits a string, a number or null;
	// it is kept raw so it round-trips byte-identically.
	ID json.RawMessage `json:"id,omitempty"`
	// Method is the A2A operation name.
	Method string `json:"method"`
	// Params is the operation's parameter object.
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response envelope. Exactly one of Result and Error
// is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object. Per specification section 9.5, A2A
// carries its structured detail objects in the "data" field as an array.
type RPCError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    []ErrorDetail `json:"data,omitempty"`
}

// RPCError renders the A2A error as a JSON-RPC error object, including the
// auto-generated ErrorInfo detail for A2A-specific error types.
func (e *Error) RPCError() *RPCError {
	return &RPCError{Code: e.Code(), Message: e.Message, Data: e.details()}
}

// ---- Request construction ----

// NewRequest builds a JSON-RPC request for an A2A operation. The id may be any
// JSON-encodable value; JSON-RPC conventionally uses a string or a number.
func NewRequest(id any, method string, params any) (*Request, error) {
	rawID, err := json.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("a2a: encode request id: %w", err)
	}
	req := &Request{JSONRPC: JSONRPCVersion, ID: rawID, Method: method}
	if params != nil {
		rawParams, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("a2a: encode %s params: %w", method, err)
		}
		req.Params = rawParams
	}
	return req, nil
}

// Encode serializes the request envelope.
func (r *Request) Encode() ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("a2a: encode request: %w", err)
	}
	return data, nil
}

// Encode serializes the response envelope.
func (r *Response) Encode() ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("a2a: encode response: %w", err)
	}
	return data, nil
}

// nullID is the JSON-RPC id used when the request id could not be recovered,
// which the specification requires for parse and invalid-request errors.
var nullID = json.RawMessage("null")

// NewResultResponse builds a successful JSON-RPC response carrying result. The
// id should be the raw id from the originating request.
func NewResultResponse(id json.RawMessage, result any) (*Response, error) {
	if len(id) == 0 {
		id = nullID
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("a2a: encode response result: %w", err)
	}
	return &Response{JSONRPC: JSONRPCVersion, ID: id, Result: raw}, nil
}

// NewErrorResponse builds a JSON-RPC error response. A nil error is treated as
// an internal error so this never produces a response with neither arm set.
func NewErrorResponse(id json.RawMessage, err *Error) *Response {
	if len(id) == 0 {
		id = nullID
	}
	if err == nil {
		err = NewError(ErrorTypeInternal, "")
	}
	return &Response{JSONRPC: JSONRPCVersion, ID: id, Error: err.RPCError()}
}

// ---- Request decoding ----

// Call is a decoded JSON-RPC request with its parameters decoded into the typed
// struct for the operation.
type Call struct {
	// Request is the raw envelope, retained so callers can echo the id.
	Request *Request
	// Method is the A2A operation name.
	Method string
	// Params is the typed parameter object: one of *SendMessageRequest,
	// *GetTaskRequest, *ListTasksRequest, *CancelTaskRequest or
	// *SubscribeToTaskRequest. SendMessage and SendStreamingMessage share
	// *SendMessageRequest.
	Params any
}

// Streaming reports whether the call's response is an SSE stream.
func (c *Call) Streaming() bool { return IsStreamingMethod(c.Method) }

// ID returns the raw JSON-RPC id, for echoing into the response.
func (c *Call) ID() json.RawMessage {
	if c.Request == nil {
		return nullID
	}
	return c.Request.ID
}

// DecodeRequest parses a JSON-RPC request envelope without touching params. It
// validates the JSON-RPC version and the presence of a method, and returns the
// envelope even on validation failure so the caller can echo the id.
func DecodeRequest(data []byte) (*Request, *Error) {
	if !json.Valid(data) {
		return nil, NewError(ErrorTypeJSONParse, "")
	}
	var req Request
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&req); err != nil {
		return nil, Errorf(ErrorTypeInvalidRequest, "malformed JSON-RPC envelope: %v", err)
	}
	if req.JSONRPC != JSONRPCVersion {
		return &req, ErrInvalidRequest("jsonrpc", fmt.Sprintf("must be %q, got %q", JSONRPCVersion, req.JSONRPC))
	}
	if req.Method == "" {
		return &req, ErrInvalidRequest("method", "must be a non-empty A2A operation name")
	}
	return &req, nil
}

// ErrInvalidRequest builds an InvalidRequestError naming the offending envelope
// field.
func ErrInvalidRequest(field, description string) *Error {
	return NewError(ErrorTypeInvalidRequest, "").
		WithDetail(NewBadRequest(FieldViolation{Field: field, Description: description}))
}

// DecodeCall parses a JSON-RPC request and decodes its parameters into the typed
// struct for the operation. Parameters are validated for required fields.
//
// The error returned distinguishes the failure modes the specification calls
// out: malformed JSON is a JSONParseError (-32700), a bad envelope is an
// InvalidRequestError (-32600), an unrecognized method is a MethodNotFoundError
// (-32601), a method the spec defines but this codec does not implement is
// PushNotificationNotSupportedError (-32003) or UnsupportedOperationError
// (-32004) per section 3.3.4, and bad parameters are an InvalidParamsError
// (-32602).
func DecodeCall(data []byte) (*Call, *Error) {
	req, protoErr := DecodeRequest(data)
	if protoErr != nil {
		return nil, protoErr
	}
	params, protoErr := decodeParams(req)
	if protoErr != nil {
		return nil, protoErr
	}
	return &Call{Request: req, Method: req.Method, Params: params}, nil
}

// decodeParams unmarshals and validates a request's params for its method.
func decodeParams(req *Request) (any, *Error) {
	if !supportedMethods[req.Method] {
		if kind, ok := knownMethods[req.Method]; ok {
			return nil, Errorf(kind, "operation %q is not supported by this agent", req.Method)
		}
		return nil, Errorf(ErrorTypeMethodNotFound, "unknown A2A operation %q", req.Method)
	}

	switch req.Method {
	case MethodSendMessage, MethodSendStreamingMessage:
		var p SendMessageRequest
		if err := unmarshalParams(req, &p); err != nil {
			return nil, err
		}
		if err := ValidateSendMessageRequest(&p); err != nil {
			return nil, err
		}
		return &p, nil

	case MethodGetTask:
		var p GetTaskRequest
		if err := unmarshalParams(req, &p); err != nil {
			return nil, err
		}
		if p.ID == "" {
			return nil, ErrInvalidParams(FieldViolation{Field: "id", Description: "task id is required"})
		}
		if err := validateHistoryLength(p.HistoryLength, "historyLength"); err != nil {
			return nil, err
		}
		return &p, nil

	case MethodListTasks:
		var p ListTasksRequest
		if err := unmarshalParams(req, &p); err != nil {
			return nil, err
		}
		if err := ValidateListTasksRequest(&p); err != nil {
			return nil, err
		}
		return &p, nil

	case MethodCancelTask:
		var p CancelTaskRequest
		if err := unmarshalParams(req, &p); err != nil {
			return nil, err
		}
		if p.ID == "" {
			return nil, ErrInvalidParams(FieldViolation{Field: "id", Description: "task id is required"})
		}
		return &p, nil

	case MethodSubscribeToTask:
		var p SubscribeToTaskRequest
		if err := unmarshalParams(req, &p); err != nil {
			return nil, err
		}
		if p.ID == "" {
			return nil, ErrInvalidParams(FieldViolation{Field: "id", Description: "task id is required"})
		}
		return &p, nil
	}

	// Unreachable: supportedMethods and this switch are kept in lockstep.
	return nil, Errorf(ErrorTypeInternal, "no parameter decoder for supported operation %q", req.Method)
}

// unmarshalParams decodes a request's params into dst. Missing params are
// rejected: every A2A operation this codec supports takes a parameter object.
func unmarshalParams(req *Request, dst any) *Error {
	if len(req.Params) == 0 || bytes.Equal(bytes.TrimSpace(req.Params), []byte("null")) {
		return ErrInvalidParams(FieldViolation{Field: "params", Description: fmt.Sprintf("%s requires a parameter object", req.Method)})
	}
	if err := json.Unmarshal(req.Params, dst); err != nil {
		return Errorf(ErrorTypeInvalidParams, "decode %s params: %v", req.Method, err)
	}
	return nil
}

// DecodeResponse parses a JSON-RPC response envelope.
func DecodeResponse(data []byte) (*Response, error) {
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("a2a: decode response: %w", err)
	}
	if resp.JSONRPC != JSONRPCVersion {
		return nil, fmt.Errorf("a2a: response jsonrpc must be %q, got %q", JSONRPCVersion, resp.JSONRPC)
	}
	if resp.Result != nil && resp.Error != nil {
		return nil, fmt.Errorf("a2a: response carries both result and error")
	}
	return &resp, nil
}

// DecodeResult unmarshals a successful response's result into dst. A response
// carrying an error yields that error instead.
func (r *Response) DecodeResult(dst any) error {
	if r.Error != nil {
		return r.Error.AsError()
	}
	if len(r.Result) == 0 {
		return fmt.Errorf("a2a: response carries neither result nor error")
	}
	if err := json.Unmarshal(r.Result, dst); err != nil {
		return fmt.Errorf("a2a: decode response result: %w", err)
	}
	return nil
}

// AsError converts a received JSON-RPC error object back into an A2A Error,
// recovering the error type from the numeric code where possible.
func (e *RPCError) AsError() *Error {
	if t, ok := errorTypeByCode[e.Code]; ok {
		return &Error{Type: t, Message: e.Message, Details: e.Data}
	}
	return &Error{
		Type:    ErrorTypeInternal,
		Message: fmt.Sprintf("unmapped JSON-RPC error code %d: %s", e.Code, e.Message),
		Details: e.Data,
	}
}
