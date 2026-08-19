package a2aclient

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// The error model of this package, and why it is shaped this way.
//
// Three kinds of failure are distinguishable and a caller reacts to each
// differently, so each gets its own type rather than a formatted string:
//
//   - A protocol failure the remote reported is returned as the *a2a.Error it
//     encoded, unwrapped and unwrapped-in-turn by errors.As. A caller that
//     wants to branch on TaskNotFoundError should not have to parse prose.
//   - A failure of the remote to BE an A2A agent — an unparseable card, a
//     binding it does not expose, a stream that is not a stream — is one of the
//     types below. These are the "the other end is wrong" errors, and they are
//     the ones a caller reports to an operator rather than retries.
//   - A transport failure is returned wrapped with a "a2aclient: ..." prefix.
//
// Every one of them is a value a test can assert on, which is the point:
// "produces a clear error rather than a hang" is only testable if the error has
// a shape.

// ErrNoEndpoint reports that no endpoint is configured or discoverable for the
// selected binding. It is the sentinel behind a *BindingError raised when the
// card carries no matching interface.
var ErrNoEndpoint = errors.New("a2aclient: no endpoint for the selected protocol binding")

// CardError reports a failure to obtain a usable Agent Card: a transport
// failure, an HTTP error status, a body that will not decode, or a card that
// decodes but does not satisfy the specification's required fields.
type CardError struct {
	// URL is the card URL that was fetched.
	URL string
	// StatusCode is the HTTP status, or zero when the request never completed.
	StatusCode int
	// Stage names what failed: "fetch", "status", "decode" or "validate".
	Stage string
	// Err is the underlying cause. For "validate" it is the *a2a.Error the
	// codec's validator produced.
	Err error
}

func (e *CardError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("a2aclient: agent card %s (%s): HTTP %d: %v", e.URL, e.Stage, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("a2aclient: agent card %s (%s): %v", e.URL, e.Stage, e.Err)
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *CardError) Unwrap() error { return e.Err }

// BindingError reports that the remote cannot be reached over the binding this
// client was configured for: the card exposes no interface for it, the
// interface it exposes declares a protocol version this client does not speak,
// or the operation requires a capability the card denies.
type BindingError struct {
	// Binding is the protocol binding that was requested, as a string so this
	// type does not force a pkg/a2a import on a caller only formatting it.
	Binding string
	// Operation names the A2A operation, empty when the failure is about the
	// binding as a whole rather than one call.
	Operation string
	// Detail explains the mismatch in operator-readable terms.
	Detail string
	// Err is the underlying cause, ErrNoEndpoint for a missing interface.
	Err error
}

func (e *BindingError) Error() string {
	var b strings.Builder
	b.WriteString("a2aclient: ")
	b.WriteString(e.Binding)
	b.WriteString(" binding")
	if e.Operation != "" {
		b.WriteString(" for ")
		b.WriteString(e.Operation)
	}
	b.WriteString(": ")
	b.WriteString(e.Detail)
	return b.String()
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *BindingError) Unwrap() error { return e.Err }

// maxErrorBodySnippet bounds how much of an undecodable error body is retained
// for diagnostics. An HTML error page from an intermediary is common and there
// is no value in carrying all of it.
const maxErrorBodySnippet = 512

// HTTPError reports a non-2xx HTTP response that carried no decodable A2A error
// body. A response that DOES carry one is returned as the *a2a.Error instead,
// so this type means "something in front of the agent answered", which is
// exactly the case an operator needs to see the raw body for.
type HTTPError struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Status is the HTTP status line.
	Status string
	// URL is the request URL.
	URL string
	// ContentType is the response Content-Type, which is usually the tell.
	ContentType string
	// Body is a bounded snippet of the response body.
	Body string
}

func (e *HTTPError) Error() string {
	status := e.Status
	if status == "" {
		status = http.StatusText(e.StatusCode)
	}
	if e.Body == "" {
		return fmt.Sprintf("a2aclient: %s answered HTTP %d %s", e.URL, e.StatusCode, status)
	}
	return fmt.Sprintf("a2aclient: %s answered HTTP %d %s: %s", e.URL, e.StatusCode, status, e.Body)
}

// StreamReason classifies why a stream failed.
type StreamReason string

// Stream failure reasons.
const (
	// StreamReasonNotSSE means the response was not a text/event-stream body:
	// an HTTP error where a stream was promised, or a 200 with the wrong
	// content type.
	StreamReasonNotSSE StreamReason = "not_sse"
	// StreamReasonMalformed means a record could not be decoded into a frame.
	StreamReasonMalformed StreamReason = "malformed"
	// StreamReasonProtocol means a decoded frame violated the A2A stream
	// contract: an illegal opening frame, an update naming a different task or
	// context, an illegal task state transition, or a frame after a terminal
	// state.
	StreamReasonProtocol StreamReason = "protocol"
	// StreamReasonTruncated means the remote closed the stream before any frame
	// reported a terminal state and without parking the task in an interrupted
	// state. The frames decoded before the close are retained in the result.
	StreamReasonTruncated StreamReason = "truncated"
	// StreamReasonRemoteError means the remote deliberately framed a protocol
	// error onto the stream and ended it — an authentication failure, a version
	// mismatch, an unknown task. The wrapped cause is the *a2a.Error.
	StreamReasonRemoteError StreamReason = "remote_error"
	// StreamReasonOpenTimeout means the remote sent no response headers within
	// the configured stream-open timeout.
	StreamReasonOpenTimeout StreamReason = "open_timeout"
	// StreamReasonIdle means no bytes arrived for longer than the configured
	// idle timeout.
	StreamReasonIdle StreamReason = "idle"
	// StreamReasonCanceled means the caller's context was cancelled or the
	// stream was closed by the caller before it terminated.
	StreamReasonCanceled StreamReason = "canceled"
	// StreamReasonTransport means the connection failed mid-stream.
	StreamReasonTransport StreamReason = "transport"
)

// StreamError reports a stream that did not run to a clean terminal state. It
// wraps the underlying cause, so errors.As still recovers an *a2a.Error for a
// protocol violation the codec's reader detected and context.Canceled for a
// cancellation.
type StreamError struct {
	// Reason classifies the failure.
	Reason StreamReason
	// Operation is the A2A operation whose stream failed.
	Operation string
	// Detail explains the failure in operator-readable terms.
	Detail string
	// Frames is how many frames were successfully decoded before the failure.
	Frames int
	// Err is the underlying cause, nil when there is none beyond Detail.
	Err error
}

func (e *StreamError) Error() string {
	msg := fmt.Sprintf("a2aclient: %s stream failed after %d frames (%s): %s",
		e.Operation, e.Frames, string(e.Reason), e.Detail)
	if e.Err != nil {
		return msg + ": " + e.Err.Error()
	}
	return msg
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *StreamError) Unwrap() error { return e.Err }

// snippet bounds and tidies a response body for inclusion in an error.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > maxErrorBodySnippet {
		return s[:maxErrorBodySnippet] + "…"
	}
	return s
}
