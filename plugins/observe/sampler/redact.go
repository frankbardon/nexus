package sampler

// Redactor is the pluggable hook for transforming or dropping bytes from
// individual journal envelopes before they are copied into the sample.
//
// The contract is intentionally minimal: implementations receive an event
// type and the JSON-encoded payload bytes, and return the (possibly
// rewritten) payload bytes for the snapshot. v1 ships only an identity
// implementation — concrete PII / secret patterns are deferred to the
// caller. Keeping the interface in place from day one preserves the option
// without bloating the v1 surface.
//
// Implementations must be safe to call concurrently; the sampler may snapshot
// multiple sessions in parallel.
type Redactor interface {
	// Redact rewrites or drops the payload bytes of a single envelope. The
	// returned slice replaces the original payload at copy time. Returning
	// nil signals "drop this payload entirely"; the envelope's metadata
	// (seq, type, ts) is preserved.
	Redact(eventType string, payload []byte) ([]byte, error)
}

// IdentityRedactor returns the payload bytes unchanged. This is the default
// the sampler uses when no other Redactor is supplied.
type IdentityRedactor struct{}

// Redact implements Redactor.
func (IdentityRedactor) Redact(_ string, payload []byte) ([]byte, error) {
	return payload, nil
}
