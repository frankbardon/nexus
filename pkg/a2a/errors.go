package a2a

import (
	"fmt"
	"net/http"
)

// Standard JSON-RPC 2.0 error codes (specification section 9.5).
const (
	CodeJSONParse      = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// A2A-specific JSON-RPC error codes (specification section 5.4). A2A reserves
// the -32001..-32099 range.
const (
	CodeTaskNotFound                 = -32001
	CodeTaskNotCancelable            = -32002
	CodePushNotificationNotSupported = -32003
	CodeUnsupportedOperation         = -32004
	CodeContentTypeNotSupported      = -32005
	CodeInvalidAgentResponse         = -32006
	CodeExtendedAgentCardNotConfig   = -32007
	CodeExtensionSupportRequired     = -32008
	CodeVersionNotSupported          = -32009
)

// ErrorDomain is the google.rpc.ErrorInfo domain A2A stamps on its errors.
const ErrorDomain = "a2a-protocol.org"

// Well-known ProtoJSON Any type URLs used in error detail objects.
const (
	TypeErrorInfo  = "type.googleapis.com/google.rpc.ErrorInfo"
	TypeBadRequest = "type.googleapis.com/google.rpc.BadRequest"
)

// ErrorType is the A2A error taxonomy from specification section 3.3.2. The
// values are the spec's error names verbatim.
type ErrorType string

// A2A error types, plus the four JSON-RPC transport-level conditions modeled as
// error types so that one constructor covers every failure this codec reports.
const (
	// A2A-specific errors (specification section 3.3.2).
	ErrorTypeTaskNotFound                 ErrorType = "TaskNotFoundError"
	ErrorTypeTaskNotCancelable            ErrorType = "TaskNotCancelableError"
	ErrorTypePushNotificationNotSupported ErrorType = "PushNotificationNotSupportedError"
	ErrorTypeUnsupportedOperation         ErrorType = "UnsupportedOperationError"
	ErrorTypeContentTypeNotSupported      ErrorType = "ContentTypeNotSupportedError"
	ErrorTypeInvalidAgentResponse         ErrorType = "InvalidAgentResponseError"
	ErrorTypeExtendedAgentCardNotConfig   ErrorType = "ExtendedAgentCardNotConfiguredError"
	ErrorTypeExtensionSupportRequired     ErrorType = "ExtensionSupportRequiredError"
	ErrorTypeVersionNotSupported          ErrorType = "VersionNotSupportedError"

	// Transport-level errors (specification section 9.5).
	ErrorTypeJSONParse      ErrorType = "JSONParseError"
	ErrorTypeInvalidRequest ErrorType = "InvalidRequestError"
	ErrorTypeMethodNotFound ErrorType = "MethodNotFoundError"
	ErrorTypeInvalidParams  ErrorType = "InvalidParamsError"
	ErrorTypeInternal       ErrorType = "InternalError"
)

// errorSpec is the per-binding mapping for one error type.
type errorSpec struct {
	code       int
	httpStatus int
	// grpcStatus is the canonical gRPC status name. It is not used to speak
	// gRPC, only to populate the REST error body's "status" field, which the
	// spec derives from the gRPC mapping (specification sections 5.4 and 11.6).
	grpcStatus string
	// reason is the google.rpc.ErrorInfo reason: the error type in
	// UPPER_SNAKE_CASE with the "Error" suffix dropped. Empty for transport
	// errors, which carry no ErrorInfo detail.
	reason  string
	message string
}

// errorSpecs is the single mapping table behind every binding conversion. It is
// a transcription of specification sections 5.4 and 9.5.
var errorSpecs = map[ErrorType]errorSpec{
	ErrorTypeTaskNotFound: {
		code: CodeTaskNotFound, httpStatus: http.StatusNotFound, grpcStatus: "NOT_FOUND",
		reason: "TASK_NOT_FOUND", message: "Task not found",
	},
	ErrorTypeTaskNotCancelable: {
		code: CodeTaskNotCancelable, httpStatus: http.StatusBadRequest, grpcStatus: "FAILED_PRECONDITION",
		reason: "TASK_NOT_CANCELABLE", message: "Task cannot be canceled",
	},
	ErrorTypePushNotificationNotSupported: {
		code: CodePushNotificationNotSupported, httpStatus: http.StatusBadRequest, grpcStatus: "FAILED_PRECONDITION",
		reason: "PUSH_NOTIFICATION_NOT_SUPPORTED", message: "Push notifications are not supported",
	},
	ErrorTypeUnsupportedOperation: {
		code: CodeUnsupportedOperation, httpStatus: http.StatusBadRequest, grpcStatus: "FAILED_PRECONDITION",
		reason: "UNSUPPORTED_OPERATION", message: "Unsupported operation",
	},
	ErrorTypeContentTypeNotSupported: {
		code: CodeContentTypeNotSupported, httpStatus: http.StatusBadRequest, grpcStatus: "INVALID_ARGUMENT",
		reason: "CONTENT_TYPE_NOT_SUPPORTED", message: "Content type not supported",
	},
	ErrorTypeInvalidAgentResponse: {
		code: CodeInvalidAgentResponse, httpStatus: http.StatusInternalServerError, grpcStatus: "INTERNAL",
		reason: "INVALID_AGENT_RESPONSE", message: "Invalid agent response",
	},
	ErrorTypeExtendedAgentCardNotConfig: {
		code: CodeExtendedAgentCardNotConfig, httpStatus: http.StatusBadRequest, grpcStatus: "FAILED_PRECONDITION",
		reason: "EXTENDED_AGENT_CARD_NOT_CONFIGURED", message: "Extended agent card is not configured",
	},
	ErrorTypeExtensionSupportRequired: {
		code: CodeExtensionSupportRequired, httpStatus: http.StatusBadRequest, grpcStatus: "FAILED_PRECONDITION",
		reason: "EXTENSION_SUPPORT_REQUIRED", message: "Required extension not supported by client",
	},
	ErrorTypeVersionNotSupported: {
		code: CodeVersionNotSupported, httpStatus: http.StatusBadRequest, grpcStatus: "FAILED_PRECONDITION",
		reason: "VERSION_NOT_SUPPORTED", message: "Protocol version not supported",
	},

	ErrorTypeJSONParse: {
		code: CodeJSONParse, httpStatus: http.StatusBadRequest, grpcStatus: "INVALID_ARGUMENT",
		message: "Invalid JSON payload",
	},
	ErrorTypeInvalidRequest: {
		code: CodeInvalidRequest, httpStatus: http.StatusBadRequest, grpcStatus: "INVALID_ARGUMENT",
		message: "Request payload validation error",
	},
	ErrorTypeMethodNotFound: {
		code: CodeMethodNotFound, httpStatus: http.StatusNotFound, grpcStatus: "NOT_FOUND",
		message: "Method not found",
	},
	ErrorTypeInvalidParams: {
		code: CodeInvalidParams, httpStatus: http.StatusBadRequest, grpcStatus: "INVALID_ARGUMENT",
		message: "Invalid parameters",
	},
	ErrorTypeInternal: {
		code: CodeInternal, httpStatus: http.StatusInternalServerError, grpcStatus: "INTERNAL",
		message: "Internal error",
	},
}

// ErrorDetail is one entry in an error's details array. Every detail object must
// carry an "@type" key identifying it, using ProtoJSON Any representation
// (specification section 3.3.2).
type ErrorDetail map[string]any

// NewErrorInfo builds a google.rpc.ErrorInfo detail object for an A2A error
// reason. The metadata map is optional and is omitted when empty.
func NewErrorInfo(reason string, metadata map[string]string) ErrorDetail {
	d := ErrorDetail{
		"@type":  TypeErrorInfo,
		"reason": reason,
		"domain": ErrorDomain,
	}
	if len(metadata) > 0 {
		d["metadata"] = metadata
	}
	return d
}

// FieldViolation names a request field that failed validation, along with why.
type FieldViolation struct {
	Field       string `json:"field"`
	Description string `json:"description"`
}

// NewBadRequest builds a google.rpc.BadRequest detail object carrying field
// violations.
func NewBadRequest(violations ...FieldViolation) ErrorDetail {
	return ErrorDetail{
		"@type":           TypeBadRequest,
		"fieldViolations": violations,
	}
}

// Error is a protocol-level A2A error. It carries everything needed to render
// itself into any binding: the JSON-RPC code, the HTTP status, the gRPC status
// name, and the structured detail objects.
//
// Error implements the error interface, so protocol failures flow through
// ordinary Go error handling and can be recovered with errors.As.
type Error struct {
	// Type is the A2A error taxonomy entry.
	Type ErrorType
	// Message is the human-readable description.
	Message string
	// Details are structured detail objects. The ErrorInfo detail for
	// A2A-specific errors is added automatically by the binding renderers, so
	// callers only add supplementary details here.
	Details []ErrorDetail
	// Metadata is attached to the auto-generated ErrorInfo detail, carrying
	// context such as the offending task ID.
	Metadata map[string]string
}

// NewError builds an A2A error of the given type. An empty message falls back to
// the spec's standard message for that type.
func NewError(t ErrorType, message string) *Error {
	spec, ok := errorSpecs[t]
	if !ok {
		// An unmapped type is a programming error in this package, not a wire
		// condition; report it as an internal error rather than panicking.
		return &Error{Type: ErrorTypeInternal, Message: fmt.Sprintf("unmapped a2a error type %q: %s", string(t), message)}
	}
	if message == "" {
		message = spec.message
	}
	return &Error{Type: t, Message: message}
}

// Errorf builds an A2A error with a formatted message.
func Errorf(t ErrorType, format string, args ...any) *Error {
	return NewError(t, fmt.Sprintf(format, args...))
}

// WithMetadata attaches ErrorInfo metadata and returns the error for chaining.
func (e *Error) WithMetadata(key, value string) *Error {
	if e.Metadata == nil {
		e.Metadata = map[string]string{}
	}
	e.Metadata[key] = value
	return e
}

// WithDetail appends a structured detail object and returns the error for
// chaining.
func (e *Error) WithDetail(d ErrorDetail) *Error {
	e.Details = append(e.Details, d)
	return e
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("a2a: %s: %s", string(e.Type), e.Message)
}

// Code returns the JSON-RPC error code for this error.
func (e *Error) Code() int { return errorSpecs[e.Type].code }

// HTTPStatus returns the HTTP status code for this error.
func (e *Error) HTTPStatus() int { return errorSpecs[e.Type].httpStatus }

// GRPCStatus returns the canonical gRPC status name for this error, which is
// also the value of the REST error body's "status" field.
func (e *Error) GRPCStatus() string { return errorSpecs[e.Type].grpcStatus }

// Reason returns the google.rpc.ErrorInfo reason for this error, or the empty
// string for transport-level errors, which carry no ErrorInfo.
func (e *Error) Reason() string { return errorSpecs[e.Type].reason }

// details assembles the full detail array: the auto-generated ErrorInfo for
// A2A-specific errors, followed by any caller-supplied details.
func (e *Error) details() []ErrorDetail {
	var out []ErrorDetail
	if reason := e.Reason(); reason != "" {
		out = append(out, NewErrorInfo(reason, e.Metadata))
	}
	out = append(out, e.Details...)
	return out
}

// Convenience constructors for the errors this codec's callers raise most.

// ErrTaskNotFound builds a TaskNotFoundError naming the task.
func ErrTaskNotFound(taskID string) *Error {
	return NewError(ErrorTypeTaskNotFound, "").WithMetadata("taskId", taskID)
}

// ErrTaskNotCancelable builds a TaskNotCancelableError for a task already in a
// terminal state.
func ErrTaskNotCancelable(taskID string, state TaskState) *Error {
	return Errorf(ErrorTypeTaskNotCancelable,
		"task is in terminal state %s and cannot be canceled", state.String()).
		WithMetadata("taskId", taskID)
}

// ErrUnsupportedOperation builds an UnsupportedOperationError naming the
// operation.
func ErrUnsupportedOperation(operation string) *Error {
	return Errorf(ErrorTypeUnsupportedOperation, "operation %q is not supported", operation)
}

// ErrInvalidParams builds an InvalidParamsError carrying field violations.
func ErrInvalidParams(violations ...FieldViolation) *Error {
	e := NewError(ErrorTypeInvalidParams, "")
	if len(violations) > 0 {
		e = e.WithDetail(NewBadRequest(violations...))
	}
	return e
}

// errorTypeByCode is the reverse of errorSpecs, for recovering an error type
// from a received JSON-RPC code. Codes are unique across the table.
var errorTypeByCode = func() map[int]ErrorType {
	out := make(map[int]ErrorType, len(errorSpecs))
	for t, spec := range errorSpecs {
		out[spec.code] = t
	}
	return out
}()

// errorTypeByReason recovers an error type from a received google.rpc.ErrorInfo
// reason, which is how the REST binding disambiguates the several A2A errors
// that share one HTTP status. Transport-level errors carry no reason and are
// absent from this map.
var errorTypeByReason = func() map[string]ErrorType {
	out := make(map[string]ErrorType, len(errorSpecs))
	for t, spec := range errorSpecs {
		if spec.reason != "" {
			out[spec.reason] = t
		}
	}
	return out
}()
