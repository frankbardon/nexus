package nexusauth

import (
	"errors"
	"log/slog"
	"strings"
)

// Kind classifies a denial. It exists so a caller can map a denial onto a
// transport status code without matching on error strings:
//
//	KindNoCredential      → 401 (nothing presented; challenge the caller)
//	KindInvalidCredential → 401 (presented and rejected; include the reason)
//	KindInsufficientScope → 403 (identity is good, authority is not)
type Kind string

const (
	// KindNoCredential means the request carried no credential at all.
	KindNoCredential Kind = "no_credential"

	// KindInvalidCredential means a credential was presented but did not
	// verify — wrong token, bad signature, expired, wrong audience.
	KindInvalidCredential Kind = "invalid_credential"

	// KindInsufficientScope means the credential verified but does not grant
	// the authority the operation requires.
	KindInsufficientScope Kind = "insufficient_scope"
)

// Sentinel errors for errors.Is. Both *Error and *DeniedError report themselves
// as matching the sentinel for their kind, so a caller can branch on
// errors.Is(err, ErrNoCredential) without knowing which concrete type it holds.
var (
	// ErrNoCredential matches any denial of kind KindNoCredential.
	ErrNoCredential = errors.New("nexusauth: no credential presented")

	// ErrInvalidCredential matches any denial of kind KindInvalidCredential.
	ErrInvalidCredential = errors.New("nexusauth: credential rejected")

	// ErrInsufficientScope matches any denial of kind KindInsufficientScope.
	ErrInsufficientScope = errors.New("nexusauth: insufficient scope")

	// ErrAuthDisabled is returned by a Chain with no validators. It is
	// deliberately not one of the three kinds: an unconfigured chain is a
	// deployment state, not a verdict on the request. The caller decides what
	// it means — refuse to start, warn and allow, or fail closed — but Validate
	// never returns a Principal for it.
	ErrAuthDisabled = errors.New("nexusauth: authentication is not configured")
)

// sentinel maps a Kind onto its errors.Is target, or nil for an unknown kind.
func (k Kind) sentinel() error {
	switch k {
	case KindNoCredential:
		return ErrNoCredential
	case KindInvalidCredential:
		return ErrInvalidCredential
	case KindInsufficientScope:
		return ErrInsufficientScope
	default:
		return nil
	}
}

// Error is a single classified denial, returned by a Validator. Reason is
// operator-facing prose: it is logged and may be echoed to the caller, so it
// must never contain the credential itself.
type Error struct {
	// Kind is the denial classification. See Kind for the status mapping.
	Kind Kind

	// Reason explains the denial in one short clause, e.g. "token expired".
	Reason string

	// Err is an optional underlying cause, exposed through Unwrap.
	Err error
}

// NewError builds a denial. cause may be nil.
func NewError(kind Kind, reason string, cause error) *Error {
	return &Error{Kind: kind, Reason: reason, Err: cause}
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("nexusauth: ")
	b.WriteString(string(e.Kind))
	if e.Reason != "" {
		b.WriteString(": ")
		b.WriteString(e.Reason)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the underlying cause, if any.
func (e *Error) Unwrap() error { return e.Err }

// Is reports a match against the sentinel for this error's kind, so callers can
// use errors.Is instead of a type assertion.
func (e *Error) Is(target error) bool {
	s := e.Kind.sentinel()
	return s != nil && target == s
}

// LogValue renders the denial as a structured slog group rather than a flat
// string, so a caller can log it with slog.Any and still filter on kind.
func (e *Error) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("kind", string(e.Kind))}
	if e.Reason != "" {
		attrs = append(attrs, slog.String("reason", e.Reason))
	}
	if e.Err != nil {
		attrs = append(attrs, slog.String("cause", e.Err.Error()))
	}
	return slog.GroupValue(attrs...)
}

// Attempt records one validator's verdict inside a DeniedError.
type Attempt struct {
	// Validator is the chain-assigned name of the validator that denied.
	Validator string

	// Kind is the denial classification that validator reported.
	Kind Kind

	// Reason is that validator's operator-facing explanation.
	Reason string

	// Err is the error the validator returned, verbatim.
	Err error
}

// String renders one attempt as "<validator>: <kind>: <reason>".
func (a Attempt) String() string {
	s := a.Validator + ": " + string(a.Kind)
	if a.Reason != "" {
		s += ": " + a.Reason
	}
	return s
}

// DeniedError aggregates every validator's refusal for one request. Aggregation
// is an operability requirement, not a nicety: when a caller is denied by a
// multi-validator chain the operator has to learn from a single log record which
// validator rejected the request and why. Both Error and LogValue therefore
// render every attempt.
type DeniedError struct {
	// Attempts holds one entry per validator the chain tried, in chain order.
	Attempts []Attempt
}

// Kind collapses the attempts into the kind the caller should act on, in
// severity order: an insufficient-scope refusal means some validator did verify
// the credential (403 is the honest answer), otherwise a rejection outranks a
// missing credential. An empty DeniedError fails closed as
// KindInvalidCredential rather than reporting no kind at all.
func (d *DeniedError) Kind() Kind {
	for _, k := range []Kind{KindInsufficientScope, KindInvalidCredential, KindNoCredential} {
		for _, a := range d.Attempts {
			if a.Kind == k {
				return k
			}
		}
	}
	return KindInvalidCredential
}

// Error implements error, rendering every attempt on one line.
func (d *DeniedError) Error() string {
	var b strings.Builder
	b.WriteString("nexusauth: request denied (")
	b.WriteString(string(d.Kind()))
	b.WriteString(")")
	for i, a := range d.Attempts {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(a.String())
	}
	return b.String()
}

// Is reports a match against the sentinel for the aggregate kind.
func (d *DeniedError) Is(target error) bool {
	s := d.Kind().sentinel()
	return s != nil && target == s
}

// LogValue renders the aggregate as a group carrying the effective kind plus one
// string per attempt, which keeps a full denial diagnosis inside one slog
// record.
func (d *DeniedError) LogValue() slog.Value {
	reasons := make([]string, 0, len(d.Attempts))
	for _, a := range d.Attempts {
		reasons = append(reasons, a.String())
	}
	return slog.GroupValue(
		slog.String("kind", string(d.Kind())),
		slog.Any("attempts", reasons),
	)
}

// add appends a validator's verdict, deriving the kind and reason from the error
// it returned. A validator that returns a bare error (a transport failure in a
// network-backed validator, say) is recorded as KindInvalidCredential: the
// request could not be authenticated, and failing closed is the only safe
// reading.
func (d *DeniedError) add(validator string, err error) {
	a := Attempt{Validator: validator, Kind: KindInvalidCredential, Err: err}
	var typed *Error
	switch {
	case errors.As(err, &typed):
		a.Kind = typed.Kind
		a.Reason = typed.Reason
		if a.Reason == "" && typed.Err != nil {
			a.Reason = typed.Err.Error()
		}
	case err != nil:
		a.Reason = err.Error()
	}
	d.Attempts = append(d.Attempts, a)
}

// addReason appends a verdict the chain itself produced, with no validator error
// behind it.
func (d *DeniedError) addReason(validator string, kind Kind, reason string) {
	d.Attempts = append(d.Attempts, Attempt{Validator: validator, Kind: kind, Reason: reason})
}

// KindOf extracts the denial kind from err, handling a single *Error, an
// aggregate *DeniedError, and a bare sentinel alike. It returns the empty Kind
// when err is nil or is not an authentication denial (ErrAuthDisabled included —
// that is a deployment state, not a verdict).
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var denied *DeniedError
	if errors.As(err, &denied) {
		return denied.Kind()
	}
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Kind
	}
	switch {
	case errors.Is(err, ErrNoCredential):
		return KindNoCredential
	case errors.Is(err, ErrInvalidCredential):
		return KindInvalidCredential
	case errors.Is(err, ErrInsufficientScope):
		return KindInsufficientScope
	}
	return ""
}
