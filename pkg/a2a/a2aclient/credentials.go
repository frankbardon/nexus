package a2aclient

import (
	"context"
	"net/http"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// CredentialSource supplies the credentials for outbound A2A requests.
//
// It is an interface rather than a token string because A2A's security schemes
// are not one shape: an API key rides in a header, a query parameter or a
// cookie; a bearer token expires and must be refreshed; an OAuth2 client
// credentials grant needs a token endpoint and a scope set. All of them come
// down to "mutate this request before it is sent, possibly doing work first",
// which is what Apply is.
//
// Apply is called once per HTTP attempt, including each retry, so a source that
// refreshes an expired token does so at the last possible moment and a retry
// after a 401 carries a fresh credential. Implementations must be safe for
// concurrent use: one client may have several calls in flight.
//
// A nil source is legal and means "send no credentials", which is the correct
// configuration for an open endpoint such as a loopback development agent.
//
// Transport-level schemes — mutual TLS in particular — are not expressible as a
// request mutation. Those are configured by supplying a pre-built *http.Client
// with the right TLS configuration through WithHTTPClient, and a source that
// needs both implements CardAwareCredentialSource to read the card's mtls
// scheme and Apply to add whatever header accompanies it.
type CredentialSource interface {
	// Apply attaches credentials to an outgoing request. Returning an error
	// aborts the call without sending it.
	Apply(ctx context.Context, req *http.Request) error
}

// CardAwareCredentialSource is an optional interface a CredentialSource may
// implement to be told which security schemes the remote declares.
//
// It exists because the card is where the parameters of a scheme live: the
// token endpoint and scopes of an OAuth2 flow, the header name and location of
// an API key, the discovery URL of an OpenID Connect provider. A source that
// implements it is handed the card exactly once, after resolution and before
// the first request that uses it, so it can configure itself from the remote's
// own declaration instead of duplicating it in local config.
//
// UseCard returning an error aborts the call: a source that cannot satisfy any
// scheme the remote accepts has nothing useful to do afterwards, and failing at
// resolution is a clearer diagnosis than a 401 on every subsequent request.
//
// A client configured with an explicit endpoint never resolves a card, so a
// source that depends on UseCard must be given one with WithCard.
type CardAwareCredentialSource interface {
	CredentialSource
	// UseCard configures the source from the remote's Agent Card. It is called
	// at most once per resolved card.
	UseCard(ctx context.Context, card *a2a.AgentCard) error
}

// CredentialFunc adapts a plain function to CredentialSource.
type CredentialFunc func(ctx context.Context, req *http.Request) error

// Apply implements CredentialSource.
func (f CredentialFunc) Apply(ctx context.Context, req *http.Request) error { return f(ctx, req) }

// NoCredentials is a CredentialSource that sends nothing. It is what a client
// with no configured source uses, and exists so that "open endpoint" is a value
// a caller can pass explicitly rather than a nil it has to reason about.
type NoCredentials struct{}

// Apply implements CredentialSource and does nothing.
func (NoCredentials) Apply(context.Context, *http.Request) error { return nil }
