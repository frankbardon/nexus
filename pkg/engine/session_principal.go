package engine

// ReservedPrincipalIDKey is the reserved-namespace session label holding the
// identity a transport's credential validator resolved for the current turn.
//
// # Why this is a different kind of value from a request header
//
// Both live in the reserved namespace and both are per-turn, but they answer
// different questions and must not be confused by anything reading them.
// A request header (see session_headers.go) is an ASSERTION the caller made
// about itself: nothing verified it, and a caller that can reach the transport
// can write whatever it likes. This is the identity a pkg/nexusauth Validator
// CHECKED — a redeemed ticket, a validated bearer token, a JWKS-verified JWT.
//
// So authorization decisions belong on this, and descriptive context (tenant,
// locale, timezone) belongs on the headers. A plugin that gates behaviour on a
// header is trusting the caller's own word about itself; one that gates on
// this is trusting the transport's validator. Keeping them in separate,
// differently-named slots is what lets a reader tell which it has.
const ReservedPrincipalIDKey = "_principal_id"

// SetPrincipalID binds the turn's verified caller identity, or clears it when
// id is empty.
//
// Transports call this before emitting the turn's first downstream event, for
// the reason SetRequestHeaders documents: a plugin must never observe a turn
// ahead of the identity it belongs to. Every turn re-binds fresh rather than
// leaving the previous turn's value in place, so a conversation continued
// under a different principal is never read as still belonging to the first.
func (s *SessionWorkspace) SetPrincipalID(id string) error {
	if id == "" {
		return s.DeleteReservedLabel(ReservedPrincipalIDKey)
	}
	return s.SetReservedLabel(ReservedPrincipalIDKey, id)
}

// PrincipalID returns the turn's verified caller identity and whether one is
// bound. It is absent when the transport runs with no validator chain
// configured, which is the honest answer: with authentication disabled there
// is no verified identity, only whatever the caller claimed.
func (s *SessionWorkspace) PrincipalID() (string, bool) {
	meta, err := s.SessionMetadata()
	if err != nil {
		return "", false
	}
	id, ok := meta.Labels[ReservedPrincipalIDKey]
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
