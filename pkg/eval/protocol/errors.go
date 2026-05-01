package protocol

import (
	"context"
	"errors"
	"fmt"
)

// Error codes surfaced in Response.Error.Code. Documented at
// docs/src/eval/inspect-protocol.md.
const (
	CodeInvalidRequest = "INVALID_REQUEST"
	CodeConfigLoad     = "CONFIG_LOAD"
	CodeEngineBoot     = "ENGINE_BOOT"
	CodeRunFailed      = "RUN_FAILED"
	CodeTimeout        = "TIMEOUT"
	CodeInternal       = "INTERNAL"
)

// codedError pairs a wire-format code with a Go error so MapError can
// recover the code without string-matching messages.
type codedError struct {
	code string
	err  error
}

func (c *codedError) Error() string { return c.err.Error() }
func (c *codedError) Unwrap() error { return c.err }

// newCodedError wraps a message and code in an Error so callers can
// `errors.As` for the code without bespoke sentinels.
func newCodedError(code, msg string) error {
	return &codedError{code: code, err: errors.New(msg)}
}

// ErrInvalidRequest constructs an INVALID_REQUEST error.
func ErrInvalidRequest(msg string) error {
	return newCodedError(CodeInvalidRequest, msg)
}

// ErrConfigLoad constructs a CONFIG_LOAD error wrapping the underlying cause.
func ErrConfigLoad(err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: CodeConfigLoad, err: fmt.Errorf("config load: %w", err)}
}

// ErrEngineBoot constructs an ENGINE_BOOT error wrapping the underlying cause.
func ErrEngineBoot(err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: CodeEngineBoot, err: fmt.Errorf("engine boot: %w", err)}
}

// ErrRunFailed constructs a RUN_FAILED error wrapping the underlying cause.
func ErrRunFailed(err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: CodeRunFailed, err: fmt.Errorf("run failed: %w", err)}
}

// ErrTimeout constructs a TIMEOUT error.
func ErrTimeout(msg string) error {
	return newCodedError(CodeTimeout, msg)
}

// ErrInternal constructs an INTERNAL error wrapping the underlying cause.
func ErrInternal(err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: CodeInternal, err: fmt.Errorf("internal: %w", err)}
}

// MapError recovers the wire-format code for an error. Falls back to
// INTERNAL when the error has no recognized code. Context-cancellation
// errors map to TIMEOUT — the only context-driven error this package
// produces is the deadline-exceeded path.
func MapError(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code, ce.err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeTimeout, "deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return CodeTimeout, "context cancelled"
	}
	return CodeInternal, err.Error()
}

// ResponseFromError builds a Response carrying err. Used at the CLI seam
// for errors raised before Run could itself return a Response.
func ResponseFromError(err error, metadata map[string]any) *Response {
	code, msg := MapError(err)
	return &Response{
		Schema:    SchemaVersion,
		ToolCalls: []ToolCall{}, // keep wire format consistent (non-null)
		Metadata:  metadata,
		Error: &Error{
			Code:    code,
			Message: msg,
		},
	}
}
