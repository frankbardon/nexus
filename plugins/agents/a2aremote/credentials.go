package a2aremote

import (
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

// The credential seam.
//
// a2aclient supplies credentials through a CredentialSource — an interface that
// mutates each outgoing request and may do work (a token refresh) first. This
// plugin binds one source per configured remote, and this file is where that
// binding happens.
//
// Today the binding is deliberately empty: every remote gets NoCredentials,
// which is exactly right for the loopback and open-endpoint cases and is what
// the plugin's own tests exercise. Concrete bearer, OAuth2 and mTLS sources —
// and the `credentials:` config block that selects between them — are a
// separate piece of work. Putting the seam in now means adding them changes
// this one function and the schema, not the call path: nothing else in the
// plugin knows how a credential is obtained, only that a remote has a source.
//
// A source is built once per remote at Init rather than per call, because
// a CredentialSource is documented as safe for concurrent use and a token cache
// that is rebuilt per call is not a cache.

// credentialFactory builds the credential source for one configured remote.
// It is a field on Plugin so a test can inject a source without a config block
// existing yet, and so the concrete sources land as one substitution here.
type credentialFactory func(agentConfig) (a2aclient.CredentialSource, error)

// noCredentials is the default factory: an open endpoint, no credentials sent.
// It returns a2aclient.NoCredentials rather than nil so the value a remote
// carries is always a real source and no caller has to reason about a nil one.
func noCredentials(agentConfig) (a2aclient.CredentialSource, error) {
	return a2aclient.NoCredentials{}, nil
}
