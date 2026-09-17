package nexuscreds

import "context"

// Source produces the credential Nexus presents on an outbound request. It is
// the whole extension point: everything an embedder has to write to plug in
// Vault, SPIFFE or a corporate token broker is one Token method and one
// Register call.
//
// The interface is deliberately one method wide. A wider one — expiry,
// refresh, scopes, a typed credential struct — would push every implementation
// into modelling a lifecycle it may not have (a sidecar that rewrites a file on
// disk has no expiry to report), and the caller has no use for the extra
// surface: it is about to write one header.
//
// Implementations must be safe for concurrent use. A Source is constructed once
// per plugin and shared by every request that plugin makes.
type Source interface {
	// Token returns the credential to present, as the bare token value with no
	// scheme prefix — the caller composes "Bearer "+token, or whatever its
	// transport requires, because not every transport uses that scheme.
	//
	// Token is called on the request path, once per outbound request, and the
	// caller does NOT cache. Caching and refresh are the implementation's
	// responsibility: a Source that mints a fresh token per call will mint one
	// per LLM request. Implementations that talk to a token endpoint are
	// expected to hold a cached token until shortly before it expires.
	//
	// ctx is the request context. Token must honour its cancellation and
	// deadline, and must not retain it.
	Token(ctx context.Context) (string, error)
}

// Factory constructs a Source from its config block. It is called once, when
// the plugin that needs the credential initialises.
//
// cfg is the credentials block as YAML decoded it — a map[string]any, the same
// shape a plugin receives for its own config. It is nil when the operator wrote
// no options at all, so a Factory must treat nil as "every default", not as an
// error.
//
// Unlike objectstore.Factory, this signature takes no context.Context, and that
// is a constraint rather than an oversight: a Factory must not perform network
// I/O. Construction happens during a plugin's Init, which in the motivating
// case (nexus.llm.gemini) runs before the plugin's own HTTP client exists, so
// there is nothing to dial with and nothing to cancel. Validate the config,
// build the Source, and defer every network call to the first Token — where
// there is a request context to honour and a caller prepared for a failure.
type Factory func(cfg map[string]any) (Source, error)
